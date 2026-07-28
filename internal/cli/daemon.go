// internal/cli/daemon.go
package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"loop-eng/internal/budget"
	"loop-eng/internal/channel"
	"loop-eng/internal/config"
	"loop-eng/internal/daemon"
	"loop-eng/internal/loop"
	"loop-eng/internal/skill"
	"loop-eng/internal/state"
	"loop-eng/internal/verify"
)

// NewDaemonCmd builds `loop-eng daemon`: the resident loop engine (spec §8.3).
// It polls the ticket channel every --poll-interval, ingests new tasks, dispatches
// them one at a time (single-active, synchronous — spec §12: no concurrency), and
// parks/resumes tier-3 needs-review tasks. The daemon never blocks on a human
// (spec principle 7): tier-3 parks → releases the active slot → polls replies → resumes.
func NewDaemonCmd() *cobra.Command {
	var repo, channelFlag, models string
	var pollInterval, cooldown time.Duration
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "常驻 loop 引擎（轮询工单 + 单活跃子 loop + park/resume）",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := mustLoad(repo)
			// 单实例保护：在 <repo>/.loop/daemon.lock 上取独占 flock。两个 daemon 指向同一
			// 仓库会在同一 tick 各拿到 FIFO 队首 → 双 worktree / 双份 token / 竞争 FF-merge。
			// 第二个实例取不到锁 → return err，cobra 以非 0 退出并把原因打印到 stderr；锁由
			// flock 绑在 fd 上，RunE 全程持有，进程退出（含被杀）内核自动释放，重启无残留。
			// run-once 刻意不加锁：它是人工单发，且不经 NextReadyTask/FIFO 派发（直接
			// ch.ListNewTasks→sl.Run），与 issue 第 3 条一致。
			lockFile, err := daemon.AcquireLock(repo)
			if err != nil {
				return err
			}
			defer lockFile.Close()
			st := mustOpenState(repo)
			defer st.Close()

			if channelFlag != "" {
				cfg.Channel.Provider = channelFlag
			}
			ch, err := buildChannel(cfg, repo)
			if err != nil {
				return err
			}

			// Preflight gate (#65): refuse to start when the channel's
			// prerequisites are missing, with an actionable checklist — fail
			// fast at boot instead of crashing mid-run (e.g. a missing
			// loop:running label only surfacing when UpdateStatus hits it).
			if err := runPreflight(context.Background(), ch); err != nil {
				return err
			}

			// Ensure the channel's status markers exist (GitHub labels / Linear
			// workflow states), creating any missing — the unified startup
			// provisioning (StatusEnsurer), the channel-agnostic parallel of
			// GitHub's EnsureLabels. Runs after preflight (the check) so
			// UpdateStatus never 404s on a missing label/state. Best-effort: a
			// failure (e.g. transient API error, no write scope) is logged, not
			// fatal — the daemon starts; per-status UpdateStatus failures surface
			// at runtime. Idempotent + config-driven (label_prefix / status_map).
			if se, ok := ch.(channel.StatusEnsurer); ok {
				if err := se.EnsureStatusMarkers(context.Background()); err != nil {
					fmt.Fprintf(os.Stderr, "[daemon] ensure status markers: %v (continuing)\n", err)
				}
			}

			// RunTask: construct a fresh SubLoop per dispatched task + run it.
			// SubLoop does the full plan→execute→verify→writeback (incl. channel
			// comment + status mark via report()). PreinsertedTaskID = the daemon's
			// already-ingested task ID (avoids duplicate InsertTask).
			runTask := func(ctx context.Context, task state.TaskRow) (string, string, error) {
				bz := budget.New(cfg.Budget.PerCallTokens, cfg.Budget.PerTaskTokens, cfg.Budget.MaxRetries)
				// 任务级 agent override：daemon 从 issue 摄取的 task.Agent 覆盖各角色 provider
				// （agent: codex → 该任务全程用 codex）。未知 provider 静默回落 config 默认。
				taskCfg := applyTaskAgent(cfg, task.Agent)
				exec, plan, verifySkill, _, help := buildModels(taskCfg, models, bz)

				// tier-1 不再从 config 接入——plan 每轮按任务产出验收脚本，SubLoop.tiersFor
				// 据此挂 tier-1（在当前 worktree 里跑）。无静态/兜底列表。
				sl := &loop.SubLoop{
					Repo:              repo,
					Store:             st,
					Budget:            bz,
					Execute:           exec,
					Plan:              plan,
					Help:              help,
					VerifyLLM:         verify.LLM{Skill: verifySkill},
					Tier3Human:        taskCfg.Verify.Tier3Human,
					Channel:           ch,
					PreinsertedTaskID: task.ID,
					PlanModelRef:      providerLabel(taskCfg.Models.Plan),
					ExecuteModelRef:   providerLabel(taskCfg.Models.Execute),
					VerifyModelRef:    providerLabel(taskCfg.Models.Verify),
					AgentForRole:      agentForRole(taskCfg),
				}

				ct := channel.Task{
					Ref:                task.IssueRef,
					Title:              task.Title, // issue 标题：完整透出（与 Body/Agent 同构）
					Description:        task.Description,
					TaskType:           task.TaskType,
					AcceptanceCriteria: task.Criteria,
					Body:               task.Body, // 全文保留：plan/execute 的背景上下文
					CreatedAt:          task.CreatedAt,
				}
				out, err := sl.Run(ctx, ct)
				if err != nil {
					fmt.Fprintf(os.Stderr, "[daemon] task %s error: %v\n", task.ID, err)
					return "error", "", err
				}
				// Integrate done work + decide close-vs-defer (finalizeLand). The
				// issue is no longer closed by SubLoop.report() — "done" only means
				// verify passed + committed on a branch. A local FF-merge success
				// closes it now; a PR / LAND PARTIAL / land-failure leaves it OPEN
				// pending merge and records the branch (land_branch) so reconcile
				// polls the branch's PR and auto-closes the issue once merged.
				if out.Status == "done" {
					if out.Branch == "" {
						// commitWorktree 失败（done 但无 branch）：记哨兵 land_branch，
						// 防 reconcile 把它当旧语义（done+open+无 land_branch）重排进 Busy-loop。
						if err := st.SetLandBranch(task.ID, "(unlanded)"); err != nil {
							fmt.Fprintf(os.Stderr, "[daemon] task %s SetLandBranch sentinel failed: %v\n", task.ID, err)
						}
					} else {
						prTitle := prTitleFor(task.Title, task.Description, task.IssueRef)
						prBody := prBodyFor(task.Title, task.IssueRef)
						prURL, prErr := createPR(repo, cfg.Channel.Repo, out.Branch, out.Worktree, prTitle, prBody)
						if prErr == nil {
							fmt.Printf("[daemon] task %s PR created: %s\n", task.ID, prURL)
						}
						logf := func(format string, args ...any) {
							fmt.Fprintf(os.Stderr, "[daemon] task "+task.ID+" "+format+"\n", args...)
						}
						res := finalizeLand(ctx, ch, task.IssueRef, repo, out.Worktree, out.Branch, prURL, prErr, logf)
						if res.Note != "" {
							out.Detail += "\n" + res.Note
						}
						// 未集成且仍有待合并分支 → 记 land_branch，reconcile 据此不重排、并按分支查 PR 合并。
						if !res.Integrated && res.Branch != "" {
							if err := st.SetLandBranch(task.ID, res.Branch); err != nil {
								fmt.Fprintf(os.Stderr, "[daemon] task %s SetLandBranch failed: %v\n", task.ID, err)
							}
						}
					}
				}
				fmt.Printf("[daemon] task %s → %s\n", task.ID, out.Status)
				return out.Status, out.Detail, nil
			}

			// Triage 门：派发前对 FIFO 队首分诊（spec triage/gate）。triage 先于任务 run
			// （daemon 派发门），无法字面「共用任务 Enforcer」（任务的 bz 在 runTask 里按任务
			// 新建），故用一个具名的派发级 triageBz：跨任务持有、不丢弃。triage 经 buildModels
			// 套 budget.Client{Role:"triage"} 受 triageBz 的 per-call/per-task 闸约束并记账；
			// triageFn 再用 task.ID 作 scope key 落一行 ledger（triage 无 runID——append-only
			// trace，不影响 run 级 Replay）。满足 issue 实质诉求：triage token 不丢弃、记账、受闸、可审计。
			triageBz := budget.New(cfg.Budget.PerCallTokens, cfg.Budget.PerTaskTokens, cfg.Budget.MaxRetries)
			_, _, _, triageSkill, _ := buildModels(cfg, models, triageBz)
			triageFn := func(ctx context.Context, task state.TaskRow) (skill.TriageOutput, error) {
				// 预算刹车·每调用 token（triage）：记录被检查的估算 vs PerCall（与 plan/execute/
				// verify 同款）。triage 的实际 token 计入与 per-call/per-task 闸在 budget.Client
				// 装饰器内生效（triageSkill.Model = &budget.Client{Role:"triage"}）。
				est := triageBz.Estimate("triage")
				_ = st.AppendBudget(task.ID, "call", "triage", est, cfg.Budget.PerCallTokens)
				// Read (don't pop) the user's prior reply so triage can see what was
				// already answered — without it, triage only sees the original (vague)
				// issue body and re-asks the same questions every round (#needs-info loop).
				// SubLoop's PopResumeFeedback still clears it when the task runs.
				fb, _ := st.GetResumeFeedback(task.ID)
				out, _, err := triageSkill.Run(ctx, skill.TriageInput{
					TaskDescription:    task.Description,
					AcceptanceCriteria: task.Criteria,
					TaskType:           task.TaskType,
					Body:               task.Body,           // 全文：判断「缺不缺信息」以全文为准
					PriorFeedback:      fb,                  // 上轮人回复：用户已补充的信息
				})
				return out, err
			}

			interval := pollInterval
			if cfg.Daemon.PollInterval > 0 {
				interval = cfg.Daemon.PollInterval
			}
			eng := &daemon.Engine{
				Channel:   ch,
				Store:     st,
				Interval:  interval,
				Cooldown:  cooldown,
				RunTask:   runTask,
				Triage:    triageFn,
				IngestMin: 3 * time.Second,
				IngestMax: 10 * time.Second,
			}
			// worktree 现场 GC：每 tick 一次（状态驱动 + 48h 宽限期；blocked 现场
			// 缓存按 SceneTTL 回收）。repo 路径在 CLI 层，故以闭包注入 engine。
			eng.GC = func(ctx context.Context) error {
				acts, err := loop.GCWorktrees(repo, st, loop.DefaultGCPolicy())
				if err != nil {
					return err
				}
				for _, a := range acts {
					if a.Delete {
						fmt.Printf("[daemon] gc: deleted %s (%s)\n", a.Name, a.Reason)
					}
				}
				return nil
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
			go func() {
				<-sigCh
				fmt.Fprintln(os.Stderr, "[daemon] shutting down...")
				cancel()
			}()

			fmt.Printf("[daemon] starting (poll=%s, channel=%s)\n", pollInterval, cfg.Channel.Provider)
			return eng.Run(ctx)
		},
	}
	cmd.Flags().StringVar(&repo, "repo", ".", "仓库路径")
	cmd.Flags().StringVar(&channelFlag, "channel", "", "local | github（空=用 cfg.Channel.Provider）")
	cmd.Flags().StringVar(&models, "models", "real", "real | fake")
	cmd.Flags().DurationVar(&pollInterval, "poll-interval", 60*time.Second, "轮询间隔")
	cmd.Flags().DurationVar(&cooldown, "cooldown", 5*time.Minute, "瞬时基础设施阻塞（如上游 529 限流）后的派发冷却时长")
	return cmd
}

// runTriage runs the triage skill for one dispatched task under a fresh per-call
// Enforcer and records its tokens into a dedicated triage run. Triage runs BEFORE
// the per-task SubLoop run, so it opens its own run + its own Enforcer (built
// per-call, never the daemon-level Enforcer — reusing that would let spend
// accumulate across tasks and globally gate triage). The triage skill's Model is
// decorated with budget.Client here (buildModels returns triage raw) so
// BeforeCall(PerCall) caps the call and AfterCall accrues real tokens (spec §8.8).
// Extracted from the daemon's triageFn closure so the accounting is directly
// unit-testable. It does NOT touch the in_flight slot or task_status — triage is
// the dispatch gate, not the active task; the engine manages those.
func runTriage(ctx context.Context, st *state.Store, triage skill.Skill[skill.TriageInput, skill.TriageOutput], cfg *config.Config, task state.TaskRow) (skill.TriageOutput, error) {
	tbz := budget.New(cfg.Budget.PerCallTokens, cfg.Budget.PerTaskTokens, cfg.Budget.MaxRetries)
	triRunID, err := st.StartRun(task.ID)
	if err != nil {
		return skill.TriageOutput{}, err
	}
	dec := triage
	dec.Model = &budget.Client{Base: triage.Model, Enf: tbz}
	_ = st.AppendBudget(triRunID, "call", "tokens", tbz.PerCall, tbz.PerCall)
	out, u, err := dec.Run(ctx, skill.TriageInput{
		TaskDescription:    task.Description,
		AcceptanceCriteria: task.Criteria,
		TaskType:           task.TaskType,
		Body:               task.Body, // 全文：判断「缺不缺信息」以全文为准
	})
	_ = st.AppendStep(state.StepRow{
		RunID: triRunID, Seq: 1, Role: "triage",
		Status: triStatus(err), Error: triErrStr(err),
		TokensIn: u.TokensIn, TokensOut: u.TokensOut,
	})
	_ = st.EndRun(triRunID, triOutcome(err))
	return out, err
}

// triStatus / triOutcome / triErrStr map a triage call's error to the step
// status, run outcome, and step error string the triage accounting writes. A
// triage run's outcome is "triage" on success (a completed triage, distinct from
// a task run's done/blocked) vs "error" on failure.
func triStatus(err error) string {
	if err == nil {
		return "ok"
	}
	return "fail"
}

func triOutcome(err error) string {
	if err == nil {
		return "triage"
	}
	return "error"
}

func triErrStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

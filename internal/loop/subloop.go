// Package loop implements the per-task control loop (SubLoop): plan → execute
// → verify (Chain of tiers) → writeback (state trace + channel report), with
// bounded retries governed by the budget Enforcer. Each attempt creates one
// fresh worktree up front and plan/execute/verify all run inside it: plan's
// exploration (and any stray writes) lands on the same disposable tree the
// attempt will discard, so the base repo is never touched before verify passes.
package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"loop-eng/internal/budget"
	"loop-eng/internal/channel"
	"loop-eng/internal/isolation"
	"loop-eng/internal/model"
	"loop-eng/internal/skill"
	"loop-eng/internal/state"
	"loop-eng/internal/verify"
)

// Outcome is the terminal result of a SubLoop.Run. Status is one of
// done | blocked | needs-info | needs-review (M1 only produces done/blocked;
// needs-review is M3 machinery). On done, Worktree+Branch identify where the
// committed work lives so the caller can land it on main (the execute model is
// forbidden from committing, so SubLoop captures the diff on the worktree's
// branch and hands the branch off).
type Outcome struct {
	Status   string // done | blocked | needs-info | needs-review
	Detail   string
	Worktree string // done only: absolute path to the worktree holding the committed work
	Branch   string // done only: branch holding that commit; caller FF-merges to main then cleans up
}

// SubLoop drives a single task through plan→execute→verify→writeback, retrying
// on verify failure up to Budget.MaxRetries. Each attempt runs plan, execute,
// and verify in one fresh worktree created at attempt start; a non-passing
// attempt discards that worktree (plan included — its exploration is read-only
// by contract, and any violation dies with the discarded tree).
type SubLoop struct {
	Repo    string
	Store   *state.Store
	Budget  *budget.Enforcer
	Execute model.Executer
	Plan    skill.Skill[skill.PlanInput, skill.PlanOutput]
	// Help is the structured-help skill (spec §8.8): when a retry hits zero-gain
	// (same failure signature twice in a row), SubLoop escalates to blocked and
	// the help skill renders a structured "stuck_at / tried / need_from_human"
	// report into the blocked battle report. Zero value (Model == nil) → SubLoop
	// falls back to the deterministic synthesizeHelp, so tests that do not wire
	// Help are unaffected and a blocked report always carries the three fields.
	Help              skill.Skill[skill.HelpInput, skill.HelpOutput]
	VerifyLLM         verify.LLM  // tier2
	Tier3Human        bool        // tier3 开关：true 时挂 tier-3（HumanTier，否则回落 HumanStub）
	HumanTier         verify.Tier // M3 真 tier-3 人审 tier；nil 时回落 HumanStub（自动通过占位）
	Channel           channel.Channel
	PreinsertedTaskID string // daemon path: if set, skip InsertTask (task already ingested by daemon tick)

	// PlanModelRef / ExecuteModelRef / VerifyModelRef carry each phase's provider
	// label ("who ran this step") into steps.model_ref — the per-step audit column
	// that always existed but was never populated. Optional (zero value "" =
	// legacy empty model_ref); set by run-once/daemon from the effective config
	// (incl. task-level agent override) so dashboard/replay show the agent per
	// step. subloop_test.go does not reference these — zero value = old behavior.
	PlanModelRef    string
	ExecuteModelRef string
	VerifyModelRef  string

	// AgentForRole constructs an agent for a role ("execute"|"verify") with its
	// provider overridden — the step-level agent override (#71-B). When plan
	// emits AgentHints, SubLoop calls this AFTER plan returns (execute/verify
	// run after plan, so the hint is known in time). nil = hints ignored
	// (legacy/tests). The CLI builds it over config with forProvider semantics
	// (provider+binary swapped, model name kept, cmd flags dropped). A hint
	// that errors here falls back to the role's configured agent, never crashes.
	AgentForRole func(role, provider string) (model.Agent, error)

	// Log is the observability sink for phase start/done, retry, and budget
	// events. When nil, defaults to os.Stderr with a "[subloop]" prefix. Tests
	// inject a logger backed by a bytes.Buffer to assert on log output without
	// scraping stderr.
	Log *log.Logger

	// RetryBackoff is the backoff per attempt before re-running after a verify
	// failure. Zero (the default) means no sleep — useful in tests. In
	// production a small backoff (e.g. 1s × attempt) spaces retries so the
	// daemon's self-healing logs have observable gaps.
	RetryBackoff time.Duration
}

// logf writes a formatted line to the SubLoop log (or stderr if Log is nil).
func (sl *SubLoop) logf(format string, args ...interface{}) {
	if sl.Log != nil {
		sl.Log.Printf(format, args...)
	} else {
		fmt.Fprintf(os.Stderr, format+"\n", args...)
	}
}

// shortID returns a truncated task ID for log lines (first 12 chars).
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// tiersFor 在每轮按 worktree + plan 产出重建 tier 链：tier1（plan 产出的验收脚本，
// 在 wt 里跑）→ tier2 → tier3。llm 是本轮生效的 tier-2（可能被 plan 的 agent_hints
// 步骤级 override 替换，见 #71-B）。
//
// tier-1 完全来自 plan（planOut.VerifyScript），无任何静态/兜底列表：
//   - plan 产出且 Valid（非空、有运行命令）→ 挂 tier-1（Dir=wt，脚本 body 先落盘）。
//   - plan 未产出（VerifyScript=nil）→ tier-1 缺席，链直接落 tier-2。
//   - plan 产出了但非法（缺运行命令等）→ Run 已记一行，这里同样跳过，落 tier-2。
func (sl *SubLoop) tiersFor(wt string, planOut skill.PlanOutput, llm verify.LLM) []verify.Tier {
	var tiers []verify.Tier
	if s := planOut.VerifyScript; s.Valid() {
		tiers = append(tiers, verify.Deterministic{
			Label:      labelOf(s),
			Cmd:        s.Run,
			Dir:        wt,
			ScriptFile: s.File,
			ScriptBody: s.Body,
		})
	}
	// tier-2 也进 worktree：agentic verify 会拿 diff 对照文件系统 ground-check，
	// 它必须站在改动真实发生的树里（#81 假驳回的病根：在主仓库根做 ground-check）。
	llm.Dir = wt
	tiers = append(tiers, llm)
	// tier-3：M3 注入了真人审 tier（HumanTier）就用它；否则 Tier3Human 时挂 HumanStub
	// 自动通过占位。HumanStub 不再产 NeedsHuman，故 M1/M2 的 done/blocked 路径不受影响。
	switch {
	case sl.HumanTier != nil:
		tiers = append(tiers, sl.HumanTier)
	case sl.Tier3Human:
		tiers = append(tiers, verify.HumanStub{})
	}
	return tiers
}

// labelOf picks the tier-1 observability tag from a plan verify script, falling
// back to the run command (or "tier-1") when the planner left Label blank.
func labelOf(s *skill.PlanVerifyScript) string {
	if s.Label != "" {
		return s.Label
	}
	if len(s.Run) > 0 {
		return strings.Join(s.Run, " ")
	}
	return "tier-1"
}

// resolveAgentHints applies plan's per-phase agent hints (#71-B): a hinted
// phase gets a freshly-constructed agent (provider swapped via AgentForRole);
// an unbuildable hint falls back to the role's configured agent (never crash on
// untrusted model output). Selection priority: step hint → task agent (already
// baked into the role's agent by the CLI's applyTaskAgent) → role config →
// default. Returns the effective execute gateway + label and tier-2 LLM + label
// for THIS attempt.
func (sl *SubLoop) resolveAgentHints(sid string, hints *skill.AgentHints) (model.Executer, string, verify.LLM, string) {
	exec, execRef := sl.Execute, sl.ExecuteModelRef
	llm, verifyRef := sl.VerifyLLM, sl.VerifyModelRef
	if hints == nil || sl.AgentForRole == nil {
		return exec, execRef, llm, verifyRef
	}
	if p := hints.Execute; p != "" {
		if a, err := sl.AgentForRole("execute", p); err == nil {
			exec, execRef = model.AsExecuter(a), p
			sl.logf("[subloop] %s execute agent override: %s (plan hint)", sid, p)
		} else {
			sl.logf("[subloop] %s execute agent hint %q unusable (%v) — fall back to role config", sid, p, err)
		}
	}
	if p := hints.Verify; p != "" {
		if a, err := sl.AgentForRole("verify", p); err == nil {
			ov := sl.VerifyLLM
			// 保留预算装饰器（verify.Chain 的冻结 Tier 接口不带 Enforcer，
			// budget.Client 是 verify token 计入预算的唯一通道）；无 Budget 时
			// （纯测试装配）退化为裸 client。
			if sl.Budget != nil {
				ov.Skill.Model = &budget.Client{Base: model.AsClient(a), Enf: sl.Budget}
			} else {
				ov.Skill.Model = model.AsClient(a)
			}
			llm, verifyRef = ov, p
			sl.logf("[subloop] %s verify agent override: %s (plan hint)", sid, p)
		} else {
			sl.logf("[subloop] %s verify agent hint %q unusable (%v) — fall back to role config", sid, p, err)
		}
	}
	return exec, execRef, llm, verifyRef
}

// planExecEstimate is the conservative per-call token estimate SubLoop feeds
// the plan and execute BeforeCall pre-checks. AppendBudget logs the same value
// so the durable budget_ledger row records exactly the estimate the Enforcer
// checked (spec §8.8).
const planExecEstimate = 1000

// Run executes the plan→execute→verify→writeback loop for one task.
//
// Verify routing (spec §7.2c/§8.6/§10): NeedsHuman → needs-review (park; the
// M3 tier-3 signal, takes precedence over Passed), else Passed → done, else the
// failure becomes next round's feedback (retry within budget). HumanStub is now
// an auto-pass placeholder (NeedsHuman=false), so M1/M2 done/blocked outcomes
// are unchanged. Every terminal outcome writes a state transition + battle report.
func (sl *SubLoop) Run(ctx context.Context, task channel.Task) (out Outcome, err error) {
	var taskID string
	if sl.PreinsertedTaskID != "" {
		taskID = sl.PreinsertedTaskID // daemon path: task already ingested by tick
	} else {
		var err error
		taskID, err = sl.Store.InsertTask(state.TaskRow{
			IssueRef: task.Ref, Description: task.Description,
			TaskType: task.TaskType, Source: "run-once", Criteria: task.AcceptanceCriteria,
			CreatedAt: task.CreatedAt,
		})
		if err != nil {
			return Outcome{Status: "error"}, err
		}
	}
	sl.Store.AppendTransition(taskID, "", "running", "dispatched")

	// →running 即在 channel 上标出「正在处理」（loop:running）。SubLoop.Run 是所有
	// 执行路径（daemon 派发 + cli run-once）的漏斗，在这里打标覆盖 run-once——它
	// 绕过 daemon，daemon 派发点的打标不会触发。daemon 路径下这会与派发点的打标
	// 重复，但幂等（标签互斥后只是 no-op 的 remove+add）。失败只记日志、不翻转
	// 任务结局（与 report 的写回容错一致）。
	if sl.Channel != nil {
		if err := sl.Channel.UpdateStatus(ctx, task.Ref, "running"); err != nil {
			sl.logf("[subloop] %s running mark failed: %v", shortID(taskID), err)
		}
	}

	// 开一行 run（spec §4.1）：每次 Run 用 StartRun 拿真 runID 透传给 steps/budget。
	// 修 resume 后 replay 交错 bug——修前 run_id 都是 taskID，同任务两次 run 的 step seq
	// 撞车后 Replay 串在一起。defer 引用命名返回 out：每条终态路径都自动 EndRun，
	// out.Status 即结局（done/blocked/needs-review/error），无需改现有 return。
	runID, rerr := sl.Store.StartRun(taskID)
	if rerr != nil {
		_ = sl.Store.ClearInFlight()
		return Outcome{Status: "error"}, rerr
	}
	defer func() { _ = sl.Store.EndRun(runID, out.Status) }()

	// Mark the active slot in the cross-process in_flight table.
	_ = sl.Store.SetInFlight(taskID, "starting")
	sid := shortID(taskID)

	// 每次运行（不论触发原因：首次 / reopen / resume / 重排队）都收集 issue 的全部
	// 评论作为「战报」上下文喂给 plan —— 修「reopen 写的反馈 plan 看不到」的 bug。
	issueContext := sl.collectIssueComments(ctx, task.Ref)
	priorFailure := ""
	if fb, err := sl.Store.PopResumeFeedback(taskID); err == nil && fb != "" {
		priorFailure = fb
	}
	// 失败现场回灌（跨 run）：找回该 issue 上一轮 execute 的 diff。判决（驳回理由）
	// 经战报/评论/priorFailure 传递，现场（实际写出的代码）经这里传递——两者合起来
	// 才是完整的失败上下文。run 内每次 verify 驳回会就地更新它，故 attempt N+1 的
	// plan/execute 看到的永远是「最近一次被驳回的实现」，而非重掷骰子。
	lastRejectedDiff := sl.loadPriorSceneDiff(ctx, task.Ref)
	// 合同回灌（跨 run）：找回上一轮 plan 冻结的实现合同（steps+verify_script）——
	// plan 每轮是全新会话，看不到前任冻结的签名就会盲重设计（#71 三轮 4→3→2
	// 振荡的病根）。run 内每轮 plan 成功后就地更新它。
	lastPlanContract := sl.loadPriorPlanContract(task.Ref)
	// sceneKept：重试耗尽的末轮被驳回时保留的 worktree（供人排查/复用；GC 按 TTL
	// 清理）。非末轮的 attempt 树仍即建即弃——diff 已进 lastRejectedDiff 与
	// steps.output_json，弃树不丢信息。
	sceneKept := ""
	// lastCompileError 携带「上一轮 verify 驳回若是编译/构建类错误」的原始 detail，作为
	// 结构化的一手信号喂给下一轮 execute prompt 的独立显眼段（#46）：编译错误原本要绕
	// verify detail → issue 评论 → collectIssueComments → 战报散文 才到 execute，信号被
	// 稀释到 execute 连续多轮不修。空串表示上一轮无编译错误（首次或语义驳回）。
	lastCompileError := ""
	// prevFailureSig 跨 attempt 记录上一轮失败签名——重试增益门槛（零增益）的判定基准：
	// 本轮签名与上一轮相同（且非空）→ 零新增信息 → 不再机械重试、升级求助（见 gain.go）。
	prevFailureSig := ""
	for attempt := 1; sl.Budget.ShouldRetry(attempt); attempt++ {
		// 协作式 cancel：phase 边界自查（spec §4.5/§7）。命中则提前以 cancelled 收尾。
		if ok, _ := sl.Store.CancelRequested(taskID); ok {
			sl.logf("[subloop] %s cancelled by TUI at phase boundary", sid)
			_ = sl.Store.ClearInFlight()
			return sl.report(ctx, taskID, task, "cancelled", "cancelled by TUI"), nil
		}
		// 预算刹车·重试：每轮入口记一行（spec §8.8）
		sl.Store.AppendBudget(runID, "task", "retry", attempt, sl.Budget.MaxRetries)

		// ---- worktree：attempt 开头创建，plan/execute/verify 共用 ----
		// plan 也在 worktree 里跑（RunIn → cmd.Dir=wt）：探索内容与 HEAD 一致，
		// 但任何违反「只读」契约的落笔都写进这棵随 attempt 丢弃的树，主仓库在
		// verify 通过前零接触（§8.9 回滚原语自此覆盖 plan 阶段）。
		wt, err := isolation.Create(sl.Repo, taskID+"-r"+fmt.Sprint(attempt))
		if err != nil {
			_ = sl.Store.ClearInFlight()
			return Outcome{Status: "error", Detail: err.Error()}, err
		}

		// ---- plan ----
		sl.logf("[subloop] %s phase=plan start", sid)
		_ = sl.Store.SetInFlight(taskID, "plan")
		if err := sl.Budget.BeforeCall(planExecEstimate); err != nil {
			isolation.Discard(sl.Repo, wt)
			_ = sl.Store.ClearInFlight()
			return sl.report(ctx, taskID, task, "blocked", "budget: "+err.Error()), nil
		}
		// 预算刹车·每调用 token：plan 模型调用前记一行（spec §8.8）
		sl.Store.AppendBudget(runID, "call", "tokens", planExecEstimate, sl.Budget.PerCall)
		planIn := skill.PlanInput{
			Task: task.Description, AcceptanceCriteria: task.AcceptanceCriteria,
			BattleReport: joinNonEmpty(issueContext, priorFailure),
			Body:         task.Body, // 全文保留：issue 原文（背景/约束）也喂给 plan
			// 重试诊断（attempt≥2 + 当轮 priorFailure 非空时注入）：引用当轮驳回原文，
			// 要求 plan 诊断 loop 数据流的结构性不可满足、行使 revised_criteria 把证据要求
			// 翻译成 tier-1 可机械判定的退出码/编译期判据。attempt=1 或无 priorFailure 时为空串，
			// 不干扰首次规划。承载在独立字段（不进 BattleReport）：这是「如何规划」的元指令，
			// 与「发生了什么」的战报分离——经 plan embed 的 {{.RetryDiagnosis}} 条件块渲染进
			// planPrompt，落进 plan step 的 input_json（attempt≥2 的 seq≥20 行）可审计。
			RetryDiagnosis: retryDiagnosisFor(attempt, priorFailure),
			// 失败现场（plan 侧）：上一轮被驳回实现的 diff（截断后）。与 RetryDiagnosis
			// 分工：诊断是「为什么被驳回」的元指令，这是「实际写了什么」的现场。
			RejectedDiff: truncateSceneDiff(lastRejectedDiff),
			// 合同回灌（plan 侧）：上一轮冻结的实现合同——默认保持稳定，防每轮
			// 盲重设计签名（#71 振荡）。
			PriorPlanContract: lastPlanContract,
		}
		// 把喂给 plan 的原始提示词落进 step trace（input_json）——dashboard 详情页
		// 的「初始提示词」读它。RenderPrompt 与 Plan.Run 内部渲染同一模板+输入，
		// 文本一致；渲染失败（模板错）时 Plan.Run 同样会报 render 错，这里留空即可。
		planPrompt, _ := skill.RenderPrompt(sl.Plan.PromptTmpl, planIn)
		planOut, u, err := sl.Plan.RunIn(ctx, planIn, wt)
		sl.Budget.AfterCall(u)
		// 空 plan 防护（plan-execute-contract-drift）：plan 调用成功但产出空计划
		// （Plan nil 或 len 0，即 `{"plan":null}` / `{"plan":[]}`）= 模型摆烂，视为
		// 可重试失败——不进 execute（否则 execute 只能靠战报上下文瞎续，浪费整轮
		// plan→execute→verify）。与 plan error 路径同构：记 plan step status=fail、设
		// priorFailure、continue 重试，MaxRetries 耗尽 → blocked（detail 含「plan 产出空计划」）。
		emptyPlan := err == nil && len(planOut.Plan) == 0
		// plan 产出也落 trace（output_json）：含 plan 步骤、verify_script 及
		// 修订后的验收标准——修订权的审计轨迹（dashboard 详情页/人审可查）。
		// 空计划也落 output_json（plan 到底返回了啥可审计），只把 step 的 status/error 标 fail。
		var planOutJSON string
		if err == nil {
			if jb, mErr := json.Marshal(planOut); mErr == nil {
				planOutJSON = string(jb)
			}
		}
		// step 的 status/error：调用级 err 优先；调用成功但空计划记 fail + "empty plan"。
		planStatus, planStepErr := statusOf(err), errStr(err)
		if emptyPlan {
			planStatus, planStepErr = "fail", "empty plan"
		}
		sl.Store.AppendStep(state.StepRow{RunID: runID, Seq: attempt*10 + 1, Role: "plan", Status: planStatus, ModelRef: sl.PlanModelRef, InputJSON: planPrompt, OutputJSON: planOutJSON, Error: planStepErr, TokensIn: u.TokensIn, TokensOut: u.TokensOut})
		if err != nil {
			isolation.Discard(sl.Repo, wt)
			sl.logf("[subloop] %s phase=plan fail: %v", sid, err)
			if errors.Is(err, model.ErrClaudeFatal) {
				_ = sl.Store.ClearInFlight()
				return sl.report(ctx, taskID, task, "blocked", "fatal model error: "+err.Error()), nil
			}
			priorFailure = "plan error: " + err.Error()
			// 增益门槛（plan error）：本轮签名与上一轮相同 → 零增益，升级 blocked（含 help 求助），
			// 不再机械重试。wt 已 Discard → sceneWT=""、diffChanged=false。
			if out, esc := sl.escalateIfZeroGain(ctx, taskID, task.Ref, task, runID, attempt, priorFailure, "", false, &prevFailureSig); esc {
				return out, nil
			}
			sl.logRetry(sid, attempt, priorFailure)
			continue
		}
		if emptyPlan {
			// priorFailure 用中文锚点「plan 产出空计划」：MaxRetries 耗尽时 blocked
			// detail = "retries exhausted: " + priorFailure，故含此字样；下一轮 plan 的
			// 重试诊断也会逐字引用它，提示「上一轮你给了空计划」。
			isolation.Discard(sl.Repo, wt)
			sl.logf("[subloop] %s phase=plan fail: plan 产出空计划 (empty plan)", sid)
			priorFailure = "plan 产出空计划"
			// 增益门槛（empty plan）：连续空计划 = 同签名零增益 → 升级 blocked。
			if out, esc := sl.escalateIfZeroGain(ctx, taskID, task.Ref, task, runID, attempt, priorFailure, "", false, &prevFailureSig); esc {
				return out, nil
			}
			sl.logRetry(sid, attempt, priorFailure)
			continue
		}
		sl.logf("[subloop] %s phase=plan done", sid)
		// 合同回灌（run 内）：本轮 plan 冻结的合同成为下一轮 plan 的 PriorPlanContract
		// （attempt N+1 的 plan 不再盲重设计）。
		lastPlanContract = contractOf(planOut)
		// plan 产出验收脚本但非法（缺运行命令等）→ tier-1 缺席，落 tier-2。记一行可观测。
		// plan 未产出是正常分支（判定不可脚本化 → tier-2），不算异常，不打 warning。
		if vs := planOut.VerifyScript; vs != nil && !vs.Valid() {
			sl.logf("[subloop] %s plan verify_script invalid (run command missing?) — skip tier-1, fall to tier-2", sid)
		}

		// 有效验收标准：plan 行使评审权（RevisedCriteria 非 nil）时以 plan 承诺的
		// 版本为准——execute 按它实现、verify 按它判；未修订（nil）沿用 issue 原版。
		// effTask 仅替换标准（verifyFailComment 的签名被测试钉死，从调用点喂修订版）。
		effTask := task
		criteriaRevised := planOut.RevisedCriteria != nil
		if criteriaRevised {
			effTask.AcceptanceCriteria = *planOut.RevisedCriteria
			sl.logf("[subloop] %s plan revised acceptance criteria: %d → %d 条 (%s)",
				sid, len(task.AcceptanceCriteria), len(*planOut.RevisedCriteria), truncateStr(planOut.CriteriaNotes, 80))
		}

		// 协作式 cancel：phase 边界自查（spec §4.5/§7）。命中则提前以 cancelled 收尾。
		if ok, _ := sl.Store.CancelRequested(taskID); ok {
			sl.logf("[subloop] %s cancelled by TUI at phase boundary", sid)
			isolation.Discard(sl.Repo, wt)
			_ = sl.Store.ClearInFlight()
			return sl.report(ctx, taskID, task, "cancelled", "cancelled by TUI"), nil
		}

		// 步骤级 agent override（#71-B）：plan 返回后、execute 前解析 hints——
		// execute/verify 本轮用哪个 agent 由 plan 的 hint（如有）决定，优先级
		// step → task（已烘进 role agent）→ role → 默认。
		exec, execModelRef, llm, verifyModelRef := sl.resolveAgentHints(sid, planOut.AgentHints)

		// ---- execute (in the attempt's worktree, created before plan) ----
		sl.logf("[subloop] %s phase=execute start", sid)
		_ = sl.Store.SetInFlight(taskID, "execute")
		if err := sl.Budget.BeforeCall(planExecEstimate); err != nil {
			isolation.Discard(sl.Repo, wt)
			_ = sl.Store.ClearInFlight()
			return sl.report(ctx, taskID, task, "blocked", "budget: "+err.Error()), nil
		}
		// 预算刹车·每调用 token：execute 模型调用前记一行（spec §8.8）
		sl.Store.AppendBudget(runID, "call", "tokens", planExecEstimate, sl.Budget.PerCall)
		execPrompt := "EXECUTE: 你在一个 git worktree 里（当前工作目录即工作区）。\n" +
			"任务: " + task.Description + "\n" +
			"验收标准:\n" + criteriaBlock(effTask.AcceptanceCriteria) + "\n"
		// plan 修订过标准时把修订理由也告诉 execute——它实现的是 plan 承诺的合同，
		// 知道「为什么改」才能不在被删除/改写的条款上浪费力气或自作主张补回。
		if criteriaRevised && planOut.CriteriaNotes != "" {
			execPrompt += "（以上验收标准经 plan 评审修订：" + planOut.CriteriaNotes + "）\n"
		}
		// 合同可见性（execute 侧）：plan 冻结的步骤（含签名）+ tier-1 验收脚本直达
		// execute——被合同约束的人必须能看到合同。#71 三轮 blocked 的病根就是
		// execute 看不到 plan 冻结的 parseClaudeResult 签名，每轮瞎猜一个。
		execPrompt += executeContractSection(planOut)
		// 编译错误特化（#46）：上一轮 verify 驳回若是编译/构建类错误，作为独立且显眼的段
		// 直达 execute prompt——不埋进下面的战报散文（issue 评论）。这是确定性、可机械判定
		// 的杠杆：字符串特征命中 + prompt 拼装，直接命中 #46「execute 连续多轮不修编译错误」
		// 的失败模式。非编译错误（如 tier-2 语义驳回）compileErrorSection 返回空串，不触发。
		if sec := compileErrorSection(lastCompileError); sec != "" {
			execPrompt += sec
		}
		// 全文保留：issue 原文（背景/约束/上下文）也喂给 execute——「任务」行只是
		// 首行蒸馏，实现细节往往藏在正文其余段落里。
		if task.Body != "" {
			execPrompt += "Issue 全文（背景/约束，实现时以全文为准）:\n" + task.Body + "\n"
		}
		// 战报/反馈也喂给 execute（不只是 plan）：否则 execute 只对照验收标准、看不见
		// issue 里的反馈，对「已实现但需按反馈精修」的任务会反复产出空 diff（#20 即此）。
		if issueContext != "" {
			execPrompt += "战报/反馈（issue 评论，含历轮驳回与人审意见）——务必据此修正代码，" +
				"不要只对照验收标准就说「已完成」:\n" + issueContext + "\n"
		}
		// 失败现场回灌（execute 侧）：上一轮被驳回实现的 diff + 驳回理由直达 execute。
		// execute 过去只看得到判决散文（战报/评论），看不到被驳回的代码本身——驳回理由
		// 引用的代码对象在它的上下文里不存在。有现场才注入，空串不影响首次 attempt。
		if sec := rejectedSceneSection(priorFailure, lastRejectedDiff); sec != "" {
			execPrompt += sec
		}
		execPrompt += "上下文：若任务/issue 引用了设计文档或 spec，开工前先读相关章节；也可浏览仓库的 README/docs 了解项目约定与冻结接口，再动手。\n" +
			"在当前目录实现任务，确保满足全部验收标准（若项目有测试，确保测试通过）。\n" +
			"注意：不要执行 git add / git commit —— 只修改或创建文件；loop-eng 会自动捕获你的改动生成 diff。"
		execOut, u2, err := exec.Exec(ctx, wt, execPrompt)
		sl.Budget.AfterCall(u2)
		if err != nil {
			isolation.Discard(sl.Repo, wt)
			sl.logf("[subloop] %s phase=execute fail: %v", sid, err)
			if errors.Is(err, model.ErrClaudeFatal) {
				_ = sl.Store.ClearInFlight()
				return sl.report(ctx, taskID, task, "blocked", "fatal model error: "+err.Error()), nil
			}
			priorFailure = "execute error: " + err.Error()
			sl.Store.AppendStep(state.StepRow{RunID: runID, Seq: attempt*10 + 2, Role: "execute", Status: "fail", ModelRef: execModelRef, InputJSON: execPrompt, Error: err.Error()})
			// 增益门槛（execute error）：同签名连续失败 → 零增益升级 blocked。wt 已 Discard。
			if out, esc := sl.escalateIfZeroGain(ctx, taskID, task.Ref, task, runID, attempt, priorFailure, "", false, &prevFailureSig); esc {
				return out, nil
			}
			sl.logRetry(sid, attempt, priorFailure)
			continue
		}
		diff := worktreeDiff(sl.Repo, wt)
		// 捕获 execute 的完整 I/O 进 trace：之前 execOut 被 `_ = execOut` 丢弃，
		// 导致「空 diff」时无从诊断 claude 到底返回了啥、为什么没改文件。
		// InputJSON=execute prompt；OutputJSON={out: 模型输出, diff: 捕获的改动}。
		rec, _ := json.Marshal(struct {
			Out  string `json:"out"`
			Diff string `json:"diff"`
		}{Out: execOut, Diff: diff})
		sl.Store.AppendStep(state.StepRow{RunID: runID, Seq: attempt*10 + 2, Role: "execute", Status: "ok", ModelRef: execModelRef, InputJSON: execPrompt, OutputJSON: string(rec), TokensIn: u2.TokensIn, TokensOut: u2.TokensOut})
		sl.logf("[subloop] %s phase=execute done", sid)

		// 协作式 cancel：phase 边界自查（spec §4.5/§7）。命中则提前以 cancelled 收尾。
		if ok, _ := sl.Store.CancelRequested(taskID); ok {
			sl.logf("[subloop] %s cancelled by TUI at phase boundary", sid)
			isolation.Discard(sl.Repo, wt)
			_ = sl.Store.ClearInFlight()
			return sl.report(ctx, taskID, task, "cancelled", "cancelled by TUI"), nil
		}

		// ---- verify (Chain of tiers; independent judgment) ----
		sl.logf("[subloop] %s phase=verify start", sid)
		_ = sl.Store.SetInFlight(taskID, "verify")
		res, err := verify.Chain(ctx, sl.tiersFor(wt, planOut, llm), diff, effTask.AcceptanceCriteria, priorFailure)
		// NeedsHuman（tier-3 人审信号）记录进 verify trace；下面在 Passed 之前优先裁决。
		// verifyTrace 写成结构化 JSON：驳回时 Detail 由 verify.detailFor 兜底永不空，
		// 且 failing_criteria 随行落库——修 #10 黑箱（旧 trace 只剩空的 detail=）。
		vt, _ := json.Marshal(verifyTrace{
			Passed:          res.Passed,
			NeedsHuman:      res.NeedsHuman,
			Detail:          res.Detail,
			FailingCriteria: res.FailingCriteria,
		})
		sl.Store.AppendStep(state.StepRow{
			RunID: runID, Seq: attempt*10 + 3, Role: "verify",
			Status:     statusOf2(res.Passed),
			ModelRef:   verifyModelRef,
			OutputJSON: string(vt),
		})
		// 逐 tier 落盘 verifications（spec §4.6）：best-effort，trace 不 gate loop。
		// runID 来自 Task 2 的 StartRun 透传；短路时只落实际跑过的 tier。
		for _, to := range res.Tiers {
			_ = sl.Store.AppendVerification(runID, to.Tier, to.Passed, to.Detail)
		}
		if err != nil {
			// verify 基础设施错误：致命（auth）→ 立刻中断；可重试 flake → 当作可重试失败。
			isolation.Discard(sl.Repo, wt)
			sl.logf("[subloop] %s phase=verify error: %v", sid, err)
			if errors.Is(err, model.ErrClaudeFatal) {
				_ = sl.Store.ClearInFlight()
				return sl.report(ctx, taskID, task, "blocked", "fatal model error: "+err.Error()), nil
			}
			// 预算类错误（budget.Client 拒付 tier-2）→ 立即 blocked，不进重试循环：
			// 重跑 plan+execute 只会让预算更糟（#86/#87 实战：verify 被拒付后空烧两轮
			// plan+execute 才撞墙）。它与 plan/execute BeforeCall 的预算闸语义对齐。
			if errors.Is(err, budget.ErrPerCall) || errors.Is(err, budget.ErrPerTask) {
				_ = sl.Store.ClearInFlight()
				return sl.report(ctx, taskID, task, "blocked", "budget: "+err.Error()), nil
			}
			priorFailure = "verify error: " + err.Error()
			// 增益门槛（verify error）：同签名连续失败 → 零增益升级 blocked。wt 已 Discard。
			if out, esc := sl.escalateIfZeroGain(ctx, taskID, task.Ref, task, runID, attempt, priorFailure, "", false, &prevFailureSig); esc {
				return out, nil
			}
			sl.logRetry(sid, attempt, priorFailure)
			continue
		}
		// tier-3 人审 → needs-review：park、释放活跃位，worktree 保留待 daemon 恢复（spec §7.2c/§8.6/§10）。
		// 在 Passed 之前裁决——NeedsHuman 优先于 done/反馈。注意：worktree 不丢弃（park 保留）。
		if res.NeedsHuman {
			sl.logf("[subloop] %s phase=verify done needs-human", sid)
			_ = sl.Store.ClearInFlight()
			return sl.report(ctx, taskID, task, "needs-review", res.Detail), nil
		}
		if res.Passed {
			sl.logf("[subloop] %s phase=verify done passed", sid)
			// Capture the execute output on the worktree's branch, then hand the
			// branch to the caller for landing. The execute prompt forbids the
			// model from committing, so loop-eng commits the work itself; the
			// caller (daemon/run-once) FF-merges this branch to main + cleans up.
			// Closes the done-worktree-never-landed gap (lost #12/#14): a Passed
			// verify used to leave the worktree uncommitted → diff vanished.
			branch := branchName(taskID, attempt)
			if cerr := commitWorktree(wt, branch, landCommitMessage(task, taskID)); cerr != nil {
				_ = sl.Store.ClearInFlight()
				// commit failed — verify still passed, so this stays done; but landing
				// is now manual. Leave the worktree (uncommitted changes survive on
				// disk) and surface in Detail so the operator can salvage by hand.
				return sl.report(ctx, taskID, task, "done", res.Detail+
					" [land: worktree commit failed: "+cerr.Error()+"; worktree preserved at "+wt+"]"), nil
			}
			_ = sl.Store.ClearInFlight()
			// plan 修订过验收标准时，在 done 战报里留人可见的审计线索（tier-3 人审
			// 与 issue 读者能看到「按修订版判过」及理由）。
			doneDetail := res.Detail
			if criteriaRevised {
				doneDetail += "\n（验收标准经 plan 评审修订：" + planOut.CriteriaNotes + "）"
			}
			out := sl.report(ctx, taskID, task, "done", doneDetail)
			out.Worktree = wt
			out.Branch = branch
			return out, nil
		}
		// 不过 → 反馈，下一轮重试
		sl.logf("[subloop] %s phase=verify done rejected: %s", sid, res.Detail)
		// verify 不过必给「失败理由 + 改进建议」，落进 issue 评论：给人看（驳回不再黑箱）
		// + 作下一轮持久反馈（collectIssueComments 下一轮读回喂 plan/execute）。best-effort——
		// 发评论失败只记日志、不 gate loop（与 report 的 PostComment 容错一致）。
		if cErr := sl.Channel.PostComment(ctx, task.Ref, verifyFailComment(effTask, attempt, res)); cErr != nil {
			sl.logf("[subloop] %s verify-fail comment post failed: %v", sid, cErr)
		}
		// 编译错误信号更新（#46）：本轮驳回是编译/构建类 → 把原始 detail 存为下一轮 execute
		// 的结构化一手信号；非编译错误（如 tier-2 语义驳回）→ 清空，避免上一轮已修好的编译
		// 错误作为陈旧信号残留、误导下一轮 execute。lastCompileError 永远反映「最近一轮驳回
		// 是否为编译错误」。
		if looksLikeCompileError(res.Detail) {
			lastCompileError = res.Detail
		} else {
			lastCompileError = ""
		}
		priorFailure = res.Detail
		// 现场 diff 是否实质变化：本轮 diff 与上一轮被驳回 diff（lastRejectedDiff 旧值）不同 →
		// diffChanged=true（零增益判据之一，落 trace；硬门控仍是失败签名）。先取旧值再更新。
		rejectedDiffChanged := lastRejectedDiff != diff
		// 现场回灌（run 内）：本轮被驳回的 diff 成为下一轮 plan/execute 的上下文——
		// 不绕 SQLite，直接用内存里的 diff（同一份已落 execute step output_json）。
		lastRejectedDiff = diff
		// 增益门槛（verify 驳回）：连续两轮失败签名相同 → 零增益，升级 blocked。sceneWT=wt
		// （命中即 return，wt 不丢弃，供人排查；GC 按 TTL）。verify-fail 评论已在门控前发出，
		// 升级时 blocked 评论紧随其后，信息完整。未命中则走既有 scene-keep + 重试。
		if out, esc := sl.escalateIfZeroGain(ctx, taskID, task.Ref, task, runID, attempt, priorFailure, wt, rejectedDiffChanged, &prevFailureSig); esc {
			return out, nil
		}
		// 末轮（重试即将耗尽）保留这棵被驳回的 worktree：供人排查/复用，GC 按 TTL
		// 清理。非末轮即弃——下一 attempt 从零建新树，diff 已在 lastRejectedDiff 与
		// steps.output_json 双份留存，弃树不丢信息。
		if sl.Budget.ShouldRetry(attempt + 1) {
			isolation.Discard(sl.Repo, wt)
		} else {
			sceneKept = wt
			sl.logf("[subloop] %s final attempt rejected: scene worktree preserved at %s", sid, wt)
		}
		sl.logRetry(sid, attempt, priorFailure)
	}
	_ = sl.Store.ClearInFlight()
	blockedDetail := "retries exhausted: " + priorFailure
	if sceneKept != "" {
		blockedDetail += "\n（末轮被驳回实现的 worktree 保留于 " + sceneKept + "，供排查/复用；GC 将按 TTL 清理）"
	}
	return sl.report(ctx, taskID, task, "blocked", blockedDetail), nil
}

// logRetry logs a retry event and sleeps the backoff. The backoff is
// attempt × RetryBackoff (zero RetryBackoff = no sleep, test-friendly).
func (sl *SubLoop) logRetry(sid string, attempt int, reason string) {
	backoff := time.Duration(attempt) * sl.RetryBackoff
	sl.logf("[subloop] %s retry attempt=%d reason=%q backoff=%s",
		sid, attempt, truncateStr(reason, 80), backoff)
	if backoff > 0 {
		time.Sleep(backoff)
	}
}

// ---- 重试增益门槛（zero-gain gate；spec §8.8 / 病根：#71 振荡 + run_b154ff82 同错误×3）----
//
// 机械重试（attempt<=MaxRetries 就再来一轮）不看「这一轮相比上一轮多了什么有效信息」，
// 会把同签名失败反复重烧 token。门控的核心确定性杠杆是 failureSignature：把振荡的数字
// 差异（4→3 vs 2→3）和逐字相同错误都归一为同一签名，而不同符号（undefined: Foo vs Bar）
// 保持不同。zeroGain 命中 → 升级（blocked + help 结构化求助），而非 continue。

// hasNewReplySinceLastComment 报告自 SubLoop 上次发评论（LastCommentAt）以来 channel 上
// 是否有新人回复。best-effort：nil Channel / Store 或 channel 出错 / 无回复均返回 false。
// 单次 Run 内各 attempt 之间通常没有新评论（run 是同步的），故几乎总是 false——它作为
// trace 判据之一被记录（若有新回复，说明重试携带了新的人的信息，不算零增益）。
func (sl *SubLoop) hasNewReplySinceLastComment(ctx context.Context, taskID, ref string) bool {
	if sl.Channel == nil {
		return false
	}
	since, err := sl.Store.LastCommentAt(taskID)
	if err != nil {
		return false
	}
	replies, err := sl.Channel.ListReplies(ctx, []string{ref}, since)
	if err != nil {
		return false
	}
	for _, r := range replies[ref] {
		if strings.TrimSpace(r.Body) != "" {
			return true
		}
	}
	return false
}

// recordRetryGate 落一行 role="retry-gate" step，记录本轮重试决策（retry/escalate）及其
// 输入（失败签名、上一轮签名、是否有新回复、现场 diff 是否实质变化）。seq=attempt*10+9，
// 在 Replay 里落在本轮 verify（…3）之后、下一轮 plan（…1）之前——「这一轮凭什么值得跑」
// 可审计。best-effort：AppendStep 失败只忽略（trace 不 gate loop，与既有 step 写入一致）。
func (sl *SubLoop) recordRetryGate(runID string, attempt int, sig, prevSig string, newReply, diffChanged, escalate bool) {
	decision := "retry"
	if escalate {
		decision = "escalate"
	}
	tr := retryGateTrace{
		ZeroGain:      escalate,
		Signature:     sig,
		PrevSignature: prevSig,
		NewReplySince: newReply,
		DiffChanged:   diffChanged,
		Decision:      decision,
		Attempt:       attempt,
	}
	b, _ := json.Marshal(tr)
	_ = sl.Store.AppendStep(state.StepRow{
		RunID: runID, Seq: attempt*10 + 9, Role: "retry-gate",
		OutputJSON: string(b), Status: decision,
	})
}

// helpOutput 产出零增益升级战报的结构化求助。Help skill 已接线（Model!=nil）且 Run 成功 →
// 用其 HelpOutput；否则退回 synthesizeHelp 兜底（测试不接线 Help 时走此路，Help 出错时也走
// 此路——blocked 战报始终带 stuck_at/tried/need_from_human）。
func (sl *SubLoop) helpOutput(ctx context.Context, task channel.Task, attempt int, priorFailure string) skill.HelpOutput {
	if sl.Help.Model != nil {
		in := skill.HelpInput{
			Task:            task.Description,
			BlockedState:    priorFailure,
			AttemptsSummary: fmt.Sprintf("已重试 %d 轮，连续失败签名相同（零增益）", attempt),
			LastError:       truncateStr(priorFailure, 500),
		}
		if out, _, err := sl.Help.Run(ctx, in); err == nil {
			return out
		}
	}
	return synthesizeHelp(task, attempt, priorFailure)
}

// escalateZeroGain 以 blocked 终结本次 run 并附结构化求助：清活跃位、拼 blocked detail
// （「重试零增益提前终止…」+ help 的 stuck_at/tried/need_from_human + sceneWT 非空时的
// worktree 保留注记）、经 report 写回。sceneWT 非空时被驳回的 worktree 不丢弃（供人排查；
// GC 按 TTL 回收）。
func (sl *SubLoop) escalateZeroGain(ctx context.Context, taskID string, task channel.Task, attempt int, priorFailure, sig, sceneWT string) Outcome {
	_ = sl.Store.ClearInFlight()
	help := sl.helpOutput(ctx, task, attempt, priorFailure)
	detail := "重试零增益提前终止（连续两轮失败签名相同，重试不会自愈）。签名: " + sig + "\n" + formatHelp(help)
	if sceneWT != "" {
		detail += "\n（被驳回实现的 worktree 保留于 " + sceneWT + "，供排查/复用；GC 将按 TTL 清理）"
	}
	return sl.report(ctx, taskID, task, "blocked", detail)
}

// escalateIfZeroGain 是重试的前置闸门（确定性优先，签名驱动）：算本轮失败签名 → 记 trace →
// zeroGain 命中（与上一轮签名相同且非空）则返回 (escalateZeroGain(...), true)，调用方即 return；
// 否则 *prevSig=sig、返回 (zero,false)，调用方走既有 logRetry+continue（携带既有新信息通道）。
//
// newReply/diffChanged 作为零增益判据落进 trace（新回复/现场 diff 变化意味着重试带了新信息）；
// 硬门控仍是失败签名（tier-1 合同钉死的确定性杠杆）。sceneWT 透传给 escalateZeroGain（命中时
// worktree 保留）。
func (sl *SubLoop) escalateIfZeroGain(ctx context.Context, taskID, ref string, task channel.Task, runID string, attempt int, priorFailure, sceneWT string, diffChanged bool, prevSig *string) (Outcome, bool) {
	sig := failureSignature(priorFailure)
	newReply := sl.hasNewReplySinceLastComment(ctx, taskID, ref)
	escalate := zeroGain(*prevSig, sig)
	sl.recordRetryGate(runID, attempt, sig, *prevSig, newReply, diffChanged, escalate)
	if escalate {
		return sl.escalateZeroGain(ctx, taskID, task, attempt, priorFailure, sig, sceneWT), true
	}
	*prevSig = sig
	return Outcome{}, false
}

// truncateStr returns s cut to at most n runes with "…" appended.
func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// criteriaBlock formats acceptance criteria into a bullet list string.
// Returns "(未提供)" when the list is empty.
func criteriaBlock(c []string) string {
	if len(c) == 0 {
		return "(未提供)"
	}
	b := ""
	for _, line := range c {
		b += "- " + line + "\n"
	}
	return b
}

// verifyTrace is the structured record SubLoop writes to the verify step's
// OutputJSON (state.steps.output_json). Structured JSON — not a free-form
// "k=v" line — so the trace is greppable and the failing_criteria list survives
// intact. This closes the #10 observability hole: a bare "detail=" was empty
// whenever the LLM left its reason blank, making rejections a black box. Detail
// is now never empty on rejection (verify.detailFor), and failing_criteria is
// carried alongside so every rejection is explainable and feeds the next retry.
type verifyTrace struct {
	Passed          bool     `json:"passed"`
	NeedsHuman      bool     `json:"needs_human"`
	Detail          string   `json:"detail"`
	FailingCriteria []string `json:"failing_criteria,omitempty"`
}

// collectIssueComments 拉取 issue/ticket 的全部评论（人审反馈 + 历轮战报）作为 plan
// 的上下文。每次 Run 都调——不论触发原因（修「reopen 写的反馈 plan 看不到」）：
// reopen 走 reconcile 只翻状态、不读评论，导致 plan 拿不到人在 issue 里写的反馈。
// since 为零值表示「全部评论」。读失败非致命（plan 只是少了上下文，不致崩）。
func (sl *SubLoop) collectIssueComments(ctx context.Context, ref string) string {
	if sl.Channel == nil {
		return ""
	}
	replies, err := sl.Channel.ListReplies(ctx, []string{ref}, time.Time{})
	if err != nil {
		sl.logf("[subloop] collect comments: ListReplies(%s) failed: %v", ref, err)
		return ""
	}
	parts := make([]string, 0, len(replies[ref]))
	for _, r := range replies[ref] {
		if s := strings.TrimSpace(r.Body); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "\n---\n")
}

// joinNonEmpty 用双换行拼接非空片段（issue 战报 + 当轮失败/反馈），喂给 plan 的
// BattleReport。空片段跳过，避免前导空行。
func joinNonEmpty(parts ...string) string {
	var out []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "\n\n")
}

// retryDiagnosisFor builds the retry-diagnosis meta-instruction SubLoop injects
// into PlanInput on retries. It is the automation of the human-written diagnosis
// that #47's second run proved is the actual trigger for plan exercising
// revised_criteria: a vague "you may revise criteria" nudge produced 0 revisions
// across 3 rounds, while a concrete unsatisfiability diagnosis + revision
// direction produced a (higher-quality) revision that passed first try.
//
// Gating (deliberate, never relax): injected ONLY when attempt ≥ 2 AND this
// round's priorFailure is non-empty. attempt=1 has nothing failed yet to
// diagnose, so the first plan is left undisturbed. attempt ≥ 2 with an empty
// priorFailure is a defensive clause — a retry only fires after a failure, so
// priorFailure is in practice always non-empty by attempt ≥ 2.
//
// The instruction is a "how to plan" meta-directive, carried in its own
// PlanInput.RetryDiagnosis field — NOT folded into BattleReport. BattleReport is
// "what happened" (history/context); this is "how to plan" (meta). Mixing them
// would make it hard for plan to tell a past battle log apart from a directive.
//
// Returned text is rendered verbatim by the plan embed's {{.RetryDiagnosis}}
// conditional block, so it lands in the plan step's input_json (steps table,
// seq ≥ 20 for attempt ≥ 2) — auditable in the dashboard detail / Replay. By
// design the dashboard's "初始提示词" panel reads the first round (attempt=1,
// seq LIMIT 1), where this is empty, so the diagnosis is not shown there; it is
// auditable in attempt ≥ 2 plan steps + Replay.
func retryDiagnosisFor(attempt int, priorFailure string) string {
	if attempt < 2 || strings.TrimSpace(priorFailure) == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("## 重试诊断（attempt=" + fmt.Sprint(attempt) + " 的元指令：如何规划，不是战报）\n\n")
	b.WriteString("本轮 priorFailure 原文（逐字引用，请据此诊断）：\n")
	b.WriteString("<<<\n" + priorFailure + "\n>>>\n\n")
	b.WriteString("诊断要求：判断上述驳回理由是否属于 loop 数据流的**结构性不可满足**——即证据要求本身" +
		"在 loop 的数据流里拿不到，与实现质量无关。典型形态：\n")
	b.WriteString("- 要求 diff/battle-report 附某条命令的输出，但 execute 在 worktree 里跑，其 stdout 不进战报" +
		"（战报只有 diff + verify 判定），这条标准对 loop 永远不可满足。\n")
	b.WriteString("- 要求 diff 出现某文件，而调用面已兼容该场景、合法地无需改动（空 diff 即正确）。\n")
	b.WriteString("- 要求出现某运行时产物，而该产物只在 execute 沙箱内短暂存在、不落进可验收的 diff。\n\n")
	b.WriteString("若判定属于结构性不可满足：行使 **revised_criteria**，把那条「拿不到的证据要求」翻译成" +
		"tier-1 可机械判定的退出码/编译期判据（例如把「报告附 build 输出」改写为「退出码 0 = 编译通过」、" +
		"或用编译期钉子 `var _ T = expr` 锁定签名），使验收能真正在 worktree 里判定通过与否。\n")
	b.WriteString("若不属于结构性不可满足（驳回指向真实未完成的实现）→ **不要**修订标准，按 priorFailure 修正实现。\n")
	return b.String()
}

// landCommitMessage builds the commit subject for a done task's auto-land: the
// issue ref + the first (truncated) line of the description. The execute model
// writes no commit message (it's forbidden from committing), so loop-eng
// synthesizes one that identifies the task in `git log`.
func landCommitMessage(task channel.Task, taskID string) string {
	_ = taskID
	subject := task.Description
	if i := strings.IndexByte(subject, '\n'); i >= 0 {
		subject = subject[:i]
	}
	if len(subject) > 72 {
		subject = subject[:72]
	}
	return "loop-eng auto-land #" + task.Ref + ": " + subject
}

// verifyFailComment 构造「verify 驳回」的 issue 评论正文：失败理由 + 改进建议。
//
// verify 不过（且非 needs-human）时落进 issue 评论——双重作用：
//   - 给人看：驳回不再是黑箱，每轮失败原因 + 该怎么改都可见（issue 侧的可观测性）。
//   - 作下一轮的持久反馈：collectIssueComments 下一轮（及 reopen/resume 后）读回，
//     喂给 plan/execute。in-memory 的 priorFailure 只活在一次 Run 内；落成评论后，
//     即便跨进程重启、跨 reopen，反馈也不丢。
//
// 纯函数（不碰 channel），便于直接单测正文；调用方负责 PostComment + best-effort 容错。
// 签名固定为 (channel.Task, int, verify.VerifyResult)：task 给验收标准（推导建议），
// attempt 标第几轮，res 给失败理由 + 未满足标准。测试必须与此签名对齐。
func verifyFailComment(task channel.Task, attempt int, res verify.VerifyResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## VERIFY-FAIL（第 %d 轮 verify 驳回）\n\n", attempt)

	b.WriteString("### 失败理由\n")
	if reason := strings.TrimSpace(res.Detail); reason != "" {
		b.WriteString(reason)
	} else {
		b.WriteString("（verify 驳回但未给出理由——请逐条复核验收标准）")
	}
	b.WriteString("\n\n")

	b.WriteString("### 改进建议\n")
	for _, s := range improvementSuggestions(task, res) {
		b.WriteString("- " + s + "\n")
	}
	return b.String()
}

// improvementSuggestions 由 verify 结果推导下一轮的可执行修正方向：
//   - 有 FailingCriteria → 每条未满足标准转成「针对性修正」（最准）。
//   - 否则回退到任务的验收标准，逐条提示复核（tier-1 这类不给 FailingCriteria 的驳回）。
//   - 两者皆空 → 兜底一条通用建议（引用失败理由，绝不交空的建议区）。
func improvementSuggestions(task channel.Task, res verify.VerifyResult) []string {
	var fc []string
	for _, c := range res.FailingCriteria {
		if s := strings.TrimSpace(c); s != "" {
			fc = append(fc, s)
		}
	}
	if len(fc) > 0 {
		out := make([]string, 0, len(fc))
		for _, c := range fc {
			out = append(out, "针对未满足标准修正："+c)
		}
		return out
	}
	var out []string
	for _, c := range task.AcceptanceCriteria {
		if s := strings.TrimSpace(c); s != "" {
			out = append(out, "复核验收标准是否满足："+s)
		}
	}
	if len(out) > 0 {
		return out
	}
	return []string{"对照上面的失败理由，逐条重做未满足的验收标准后再提交"}
}

// report writes the terminal state transition + channel battle report for a
// terminal outcome (done or blocked) and returns the corresponding Outcome.
// Per spec §7.2d / §14, writeback happens on EVERY terminal outcome — not just
// blocked. Writeback has three legs: state transition, channel comment, and a
// channel status mark (Local: status/<ref>; GitHub: loop:<status> label).
//
// Writeback errors (state DB write, channel comment post, status mark) are
// surfaced rather than swallowed: each is logged to os.Stderr AND folded into
// the returned Detail so callers/tests can see partial writeback. The Outcome
// status itself is unchanged — a writeback failure does not flip a done to a
// blocked.
func (sl *SubLoop) report(ctx context.Context, taskID string, task channel.Task, status, detail string) Outcome {
	if err := sl.Store.AppendTransition(taskID, "running", status, detail); err != nil {
		fmt.Fprintf(os.Stderr, "writeback error: AppendTransition failed: %v\n", err)
		detail += " [writeback partial: transition: " + err.Error() + "]"
	}
	if err := sl.Channel.PostComment(ctx, task.Ref, strings.ToUpper(status)+": "+detail); err != nil {
		fmt.Fprintf(os.Stderr, "writeback error: PostComment failed: %v\n", err)
		detail += " [writeback partial: comment: " + err.Error() + "]"
	} else if err := sl.Store.SetLastCommentAt(taskID, time.Now()); err != nil {
		fmt.Fprintf(os.Stderr, "writeback error: SetLastCommentAt failed: %v\n", err)
		detail += " [writeback partial: last_comment_at: " + err.Error() + "]"
	}
	// Mark the ticket's status: Local writes status/<ref>; GitHub adds a
	// loop:<status> label. Same error contract as PostComment — surface to
	// stderr + fold into Detail, but never flip the outcome status.
	if err := sl.Channel.UpdateStatus(ctx, task.Ref, status); err != nil {
		fmt.Fprintf(os.Stderr, "writeback error: UpdateStatus failed: %v\n", err)
		detail += " [writeback partial: status: " + err.Error() + "]"
	}
	// Issue closure is NOT a done writeback side-effect here. "done" only means
	// the work passed verify + committed on a branch — it is not yet integrated.
	// The landing path decides: a local FF-merge success closes the issue
	// immediately (finalizeLand); a PR / LAND PARTIAL / land-failure leaves it
	// OPEN pending merge, and the daemon's reconcile closes it once the PR
	// merges (MergeChecker). Closing here prematurely marked issues CLOSED while
	// their PRs were still OPEN (#72/#75).
	return Outcome{Status: status, Detail: detail}
}

// internal/cli/run_once.go
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"loop-eng/internal/budget"
	"loop-eng/internal/channel"
	"loop-eng/internal/config"
	"loop-eng/internal/loop"
	"loop-eng/internal/model"
	"loop-eng/internal/skill"
	"loop-eng/internal/state"
	"loop-eng/internal/verify"
)

// NewRunOnceCmd builds `loop-eng run-once`: the M1 synchronous entry point.
// It loads .loop/config.yaml, opens .loop/state.db, assembles the model
// client (Fake under --models=fake, ClaudeClient otherwise — 裁决 I single
// claude-p path, no API/anthropic branch), builds the four skills + verify
// tier chain, wires a SubLoop, pulls the first inbox task, and prints the
// Outcome. Budget is enforced three ways (per-call / per-task / retries);
// verify's LLM Call is budget-wrapped via budget.Client to close the
// carry-forward gap from Task 12 (SubLoop budget-wraps plan+execute
// manually; that stays untouched — no double-count).
func NewRunOnceCmd() *cobra.Command {
	var repo, inbox, models, channelFlag string
	cmd := &cobra.Command{
		Use:   "run-once",
		Short: "M1 同步入口：从 inbox 捞一个任务跑完整 loop",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := mustLoad(repo)
			st := mustOpenState(repo)
			defer st.Close()

			// --channel 非空则覆盖 cfg.Channel.Provider（命令行优先于 config）
			if channelFlag != "" {
				cfg.Channel.Provider = channelFlag
			}

			bz := budget.New(cfg.Budget.PerCallTokens, cfg.Budget.PerTaskTokens, cfg.Budget.MaxRetries)

			ch, err := buildChannel(cfg, repo)
			if err != nil {
				return err
			}

			tasks, err := ch.ListNewTasks(context.Background())
			if err != nil || len(tasks) == 0 {
				return fmt.Errorf("no task in inbox %s", inbox)
			}
			// 任务级 agent override：issue frontmatter `agent: codex` 把整任务切到指定
			// provider（覆盖各角色默认）。未知 provider 静默回落 config 默认（不崩进程）。
			cfg = applyTaskAgent(cfg, tasks[0].Agent)
			exec, plan, verifySkill, _, help := buildModels(cfg, models, bz)

			// tier-1 不再从 config 接入——plan 每轮按任务产出验收脚本，SubLoop.tiersFor
			// 据此挂 tier-1（在当前 worktree 里跑）。无静态/兜底列表。
			sl := &loop.SubLoop{
				Repo:            repo,
				Store:           st,
				Budget:          bz,
				Execute:         exec,
				Plan:            plan,
				Help:            help,
				VerifyLLM:       verify.LLM{Skill: verifySkill},
				Tier3Human:      cfg.Verify.Tier3Human,
				Channel:         ch,
				PlanModelRef:    providerLabel(cfg.Models.Plan),
				ExecuteModelRef: providerLabel(cfg.Models.Execute),
				VerifyModelRef:  providerLabel(cfg.Models.Verify),
				AgentForRole:    agentForRole(cfg),
			}
			out, err := sl.Run(context.Background(), tasks[0])
			// Integrate done work + decide close-vs-defer (finalizeLand). Same
			// close-vs-defer logic as the daemon: a local FF-merge success closes
			// the issue; a PR / LAND PARTIAL / land-failure leaves it OPEN with a
			// 「待合并」note (the issue is no longer closed by SubLoop.report()).
			// run-once is one-shot (no reconcile), so it does not record a
			// land_branch — a PR left open here is closed by hand or a later daemon
			// run, not by a reconcile tick.
			if err == nil && out.Status == "done" && out.Branch != "" {
				prTitle := prTitleFor(tasks[0].Title, tasks[0].Description, tasks[0].Ref)
				prBody := prBodyFor(tasks[0].Title, tasks[0].Ref)
				prURL, prErr := createPR(repo, cfg.Channel.Repo, out.Branch, out.Worktree, prTitle, prBody)
				if prErr == nil {
					fmt.Printf("PR created: %s\n", prURL)
				}
				logf := func(format string, args ...any) {
					fmt.Fprintf(os.Stderr, format+"\n", args...)
				}
				res := finalizeLand(context.Background(), ch, tasks[0].Ref, repo, out.Worktree, out.Branch, prURL, prErr, logf)
				if res.Note != "" {
					out.Detail += "\n" + res.Note
				}
			}
			fmt.Printf("outcome: %s — %s\n", out.Status, out.Detail)
			return err
		},
	}
	cmd.Flags().StringVar(&repo, "repo", ".", "仓库路径")
	cmd.Flags().StringVar(&inbox, "task-inbox", "", "任务 inbox 目录（用于错误提示；实际读 <repo>/inbox）")
	cmd.Flags().StringVar(&models, "models", "real", "real | fake（测试用）")
	cmd.Flags().StringVar(&channelFlag, "channel", "", "local | github（空=用 cfg.Channel.Provider）")
	return cmd
}

// buildChannel picks the channel.Channel by cfg.Channel.Provider: "" or
// "local" → channel.Local (reads <repo>/inbox, writes <repo>/outbox);
// "github" → channel.GitHub backed by the authenticated `gh` CLI;
// "linear" → channel.Linear backed by the Linear GraphQL API (key from env
// LOOP_ENG_LINEAR_API_KEY, per #24 决定 A — never from config.yaml). Unknown
// providers error. This is the M2-6 wiring point that replaces the M1
// hard-coded channel.NewLocal(repo).
func buildChannel(cfg *config.Config, repo string) (channel.Channel, error) {
	prov := cfg.Channel.Provider
	if prov == "" {
		prov = "local"
	}
	switch prov {
	case "local":
		ch := channel.NewLocal(repo)
		if cfg.Channel.Inbox != "" {
			ch.InboxDir = cfg.Channel.Inbox
		}
		return ch, nil
	case "github":
		gh := channel.NewGitHub(cfg.Channel.Repo, cfg.Channel.TaskLabel)
		gh.LabelPrefix = cfg.Channel.LabelPrefix // 空 = 默认 "loop:"（channel 内部归一）
		return gh, nil
	case "linear":
		key := os.Getenv(channel.LinearAPIKeyEnv)
		if key == "" {
			return nil, fmt.Errorf("linear provider: 环境变量 %s 未设置", channel.LinearAPIKeyEnv)
		}
		// cfg.Channel.Linear 是指针（config 交互命令的产物）；validate() 已保证
		// provider=linear 时非 nil。
		lc := cfg.Channel.Linear
		return channel.NewLinear(key, "", lc.Project, lc.Team, lc.StatusMap), nil
	default:
		return nil, fmt.Errorf("unknown channel provider: %s", prov)
	}
}

// buildModels assembles the four skills' shared model.Client plus the typed
// Skill wrappers. mode="fake" wires a FakeClient keyed by prompt prefix (for
// CI/hermetic tests); otherwise a single ClaudeClient backs every role
// (spec §8.1 / 裁决 I — M1 simplifies plan/execute/verify to one claude -p
// path, no API branch, no anthropic SDK).
//
// The verify skill's Model is wrapped in a budget.Client sharing the SAME
// *Enforcer passed to SubLoop.Budget. This closes the Task 12 carry-forward
// gap: verify.Chain's frozen Tier interface takes no Enforcer, so without
// the decorator the verify LLM Call would bypass BeforeCall/AfterCall and
// its tokens would never accrue into spent (spec §8.8 deviation). plan and
// execute keep the raw client because SubLoop already budget-wraps those
// two manually — wrapping them again would double-count.
func buildModels(cfg *config.Config, mode string, bz *budget.Enforcer) (
	exec model.Executer,
	plan skill.Skill[skill.PlanInput, skill.PlanOutput],
	vs skill.Skill[skill.VerifyInput, skill.VerifyOutput],
	triage skill.Skill[skill.TriageInput, skill.TriageOutput],
	help skill.Skill[skill.HelpInput, skill.HelpOutput],
) {
	if mode == "fake" {
		// fakeHelp: help skill 的 fake 输出（零增益 blocked 战报的结构化求助占位）。
		fakeHelp := skill.HelpOutput{}
		fakeHelp.HelpRequest.StuckAt = "（fake）零增益卡住"
		fakeHelp.HelpRequest.Tried = []string{"（fake）已重试多轮，失败签名相同"}
		fakeHelp.HelpRequest.NeedFromHuman = "（fake）请人决策合同/补信息/排查环境"
		f := model.NewFake(map[string]string{
			"TRIAGE:":  jsonStr(skill.TriageOutput{Startable: true, LoopDoable: true}),
			"PLAN:":    jsonStr(skill.PlanOutput{Plan: []skill.PlanStep{{Step: "实现任务以满足验收标准"}}}),
			"EXECUTE:": "ok",
			"VERIFY:":  jsonStr(skill.VerifyOutput{Passed: true}),
			"HELP:":    jsonStr(fakeHelp),
		})
		exec = f
		plan = skill.Skill[skill.PlanInput, skill.PlanOutput]{Name: "plan", PromptTmpl: mustSkillPrompt("plan"), ParseJSON: parseJSON[skill.PlanOutput], Model: f}
		vs = skill.Skill[skill.VerifyInput, skill.VerifyOutput]{Name: "verify", PromptTmpl: mustSkillPrompt("verify"), ParseJSON: parseJSON[skill.VerifyOutput], Model: &budget.Client{Base: f, Enf: bz}}
		triage = skill.Skill[skill.TriageInput, skill.TriageOutput]{Name: "triage", PromptTmpl: mustSkillPrompt("triage"), ParseJSON: parseJSON[skill.TriageOutput], Model: f}
		help = skill.Skill[skill.HelpInput, skill.HelpOutput]{Name: "help", PromptTmpl: mustSkillPrompt("help"), ParseJSON: parseJSON[skill.HelpOutput], Model: f}
		return
	}
	// real: dispatch each role's agent by config.ModelRef.Provider via NewAgent
	// (spec §8.10 — the shell-out is provider-neutral). Default config sets no
	// provider → "" → claude → the same `claude -p` path as before (out-of-box
	// behavior unchanged). provider: codex (or opencode/kimi/kilo once added)
	// routes to that provider's adapter. Each Agent is bridged onto the frozen
	// Client/Executer interfaces via AsClient/AsExecuter, so budget/skill/subloop
	// wiring is untouched. The verify skill's Model is still wrapped in a
	// budget.Client sharing bz (the Task 12 carry-forward fix — verify.Chain's
	// frozen Tier takes no Enforcer, so the decorator is how verify's Call accrues).
	exec = model.AsExecuter(mustAgent(cfg.Models.Execute))
	plan = skill.Skill[skill.PlanInput, skill.PlanOutput]{Name: "plan", PromptTmpl: mustSkillPrompt("plan"), ParseJSON: parseJSON[skill.PlanOutput], Model: model.AsClient(mustAgent(cfg.Models.Plan))}
	vs = skill.Skill[skill.VerifyInput, skill.VerifyOutput]{Name: "verify", PromptTmpl: mustSkillPrompt("verify"), ParseJSON: parseJSON[skill.VerifyOutput], Model: &budget.Client{Base: model.AsClient(mustAgent(cfg.Models.Verify)), Enf: bz}}
	triage = skill.Skill[skill.TriageInput, skill.TriageOutput]{Name: "triage", PromptTmpl: mustSkillPrompt("triage"), ParseJSON: parseJSON[skill.TriageOutput], Model: model.AsClient(mustAgent(cfg.Models.Triage))}
	// help 复用 triage 的 agent：config.Models 无 Help 字段（已核实只有 Triage/Plan/Execute/
	// Verify），help 与 triage 同属分类/诊断类，按 spec §8.8 原设计接线上（模板/类型早就在，
	// 本次补接线）。零增益 blocked 战报由此产出结构化 stuck_at/tried/need_from_human。
	help = skill.Skill[skill.HelpInput, skill.HelpOutput]{Name: "help", PromptTmpl: mustSkillPrompt("help"), ParseJSON: parseJSON[skill.HelpOutput], Model: model.AsClient(mustAgent(cfg.Models.Triage))}
	return
}

// mustAgent resolves a config.ModelRef to an Agent, panicking on an unknown
// provider. Real configs only ever carry registered providers (""/claude via the
// default, or a task-agent that applyTaskAgent has already validated against
// NewAgent), so the panic is a truly-unreachable guard — not a runtime path.
func mustAgent(ref config.ModelRef) model.Agent {
	a, err := model.NewAgent(ref)
	if err != nil {
		panic(err)
	}
	return a
}

// providerLabel renders a role's effective provider for the trace's model_ref
// column ("who ran this step"). It normalizes "" → "claude" and appends the
// model name when set (e.g. "codex/gpt-5.1"), so dashboard/replay can show the
// agent+model that produced each step.
func providerLabel(ref config.ModelRef) string {
	p := ref.Provider
	if p == "" {
		p = "claude"
	}
	if ref.Name != "" {
		return p + "/" + ref.Name
	}
	return p
}

// applyTaskAgent returns a config clone with every model role switched to the
// task's requested provider. A task-level agent hint (issue frontmatter
// `agent: codex`) opts the WHOLE task into a different provider stack, so each
// role's provider+binary are reset to that provider's defaults and the old
// provider's cmd flags (e.g. claude's --dangerously-skip-permissions, which
// other binaries reject) are dropped; a per-role model name is preserved.
//
// agent=="" returns cfg unchanged (the common case: no override). An agent that
// isn't a registered provider (e.g. a provider not yet implemented) also returns
// cfg unchanged — validated against NewAgent so an unknown name falls back to
// the configured default rather than crashing the daemon on untrusted issue input.
func applyTaskAgent(cfg *config.Config, agent string) *config.Config {
	if agent == "" {
		return cfg
	}
	if _, err := model.NewAgent(config.ModelRef{Provider: agent, Binary: agent}); err != nil {
		return cfg
	}
	clone := *cfg
	clone.Models = config.Models{
		Triage:  forProvider(cfg.Models.Triage, agent),
		Plan:    forProvider(cfg.Models.Plan, agent),
		Execute: forProvider(cfg.Models.Execute, agent),
		Verify:  forProvider(cfg.Models.Verify, agent),
	}
	return &clone
}

// forProvider resets a ModelRef to a provider's defaults: provider+binary set to
// the provider name, model name kept, cmd dropped (the old provider's native
// flags don't apply to the new one).
func forProvider(ref config.ModelRef, provider string) config.ModelRef {
	return config.ModelRef{
		Provider: provider,
		Binary:   provider,
		Name:     ref.Name,
	}
}

// agentForRole builds SubLoop's step-level agent factory (#71-B): the role's
// ModelRef with the provider swapped (forProvider semantics — model name kept,
// the old provider's cmd flags dropped). Unknown providers surface as errors;
// SubLoop falls back to the role's configured agent (never crashes on untrusted
// plan output).
func agentForRole(cfg *config.Config) func(role, provider string) (model.Agent, error) {
	return func(role, provider string) (model.Agent, error) {
		var base config.ModelRef
		switch role {
		case "execute":
			base = cfg.Models.Execute
		case "verify":
			base = cfg.Models.Verify
		default:
			return nil, fmt.Errorf("agent hint: unknown role %q", role)
		}
		return model.NewAgent(forProvider(base, provider))
	}
}

// mustSkillPrompt reads an embedded skill template (embed/skills/<name>.md) and
// panics on read failure. The embedded templates render ALL Input fields (spec
// §8.5) — notably verify.md carries AcceptanceCriteria + PriorFailureSignal, so
// the verifier sees the criteria it must judge against (§8.6 命门 — the old
// truncated "VERIFY: {{.Diff}}" literal omitted them). Each template begins with
// its role prefix (TRIAGE:/PLAN:/VERIFY:), so FakeClient's HasPrefix matching is
// preserved unchanged.
func mustSkillPrompt(name string) string {
	b, err := skillFiles.ReadFile("embed/skills/" + name + ".md")
	if err != nil {
		panic("read embedded skill " + name + ": " + err.Error())
	}
	return string(b)
}

func parseJSON[O any](b []byte) (O, error) {
	var o O
	return o, json.Unmarshal(b, &o)
}

func jsonStr(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// loadConfig reads a config file and validates its model providers
// (model.ValidateProviders — the same registry NewAgent dispatches on), so a
// misspelled `models.<role>.provider` fails at load time instead of surfacing
// mid-run when NewAgent shells out. It is the shared gate for every CLI path
// that reads config (daemon / run-once / dashboard / doctor / config): routing
// config.Load through here means provider validation can't be skipped.
func loadConfig(path string) (*config.Config, error) {
	c, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	if err := model.ValidateProviders(c); err != nil {
		return nil, err
	}
	return c, nil
}

// mustLoad reads <repo>/.loop/config.yaml via loadConfig (provider validation
// included). M1 simplification: load failure panics (run-once is a dev/test
// entry point; the daemon and dashboard reach config through mustLoad too, so a
// bad provider now panics at boot rather than mid-run).
func mustLoad(repo string) *config.Config {
	c, err := loadConfig(filepath.Join(repo, ".loop", "config.yaml"))
	if err != nil {
		panic(err)
	}
	return c
}

// mustOpenState opens <repo>/.loop/state.db. Same M1 panic-on-failure policy
// as mustLoad.
func mustOpenState(repo string) *state.Store {
	st, err := state.Open(filepath.Join(repo, ".loop", "state.db"))
	if err != nil {
		panic(err)
	}
	return st
}

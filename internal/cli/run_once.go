// internal/cli/run_once.go
package cli

import (
	"context"
	"encoding/json"
	"fmt"
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
	var repo, inbox, models string
	cmd := &cobra.Command{
		Use:   "run-once",
		Short: "M1 同步入口：从 inbox 捞一个任务跑完整 loop",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := mustLoad(repo)
			st := mustOpenState(repo)
			defer st.Close()

			bz := budget.New(cfg.Budget.PerCallTokens, cfg.Budget.PerTaskTokens, cfg.Budget.MaxRetries)
			exec, plan, verifySkill, triage := buildModels(cfg, models, bz)
			_ = triage // M1 SubLoop 外分诊（M3 daemon 调用）

			tiers := buildTiers(cfg, repo, verifySkill)
			ch := channel.NewLocal(repo)

			tasks, err := ch.ListNewTasks(context.Background())
			if err != nil || len(tasks) == 0 {
				return fmt.Errorf("no task in inbox %s", inbox)
			}
			sl := &loop.SubLoop{
				Repo: repo, Store: st, Budget: bz,
				Execute: exec,
				Plan:    plan,
				Tiers:   tiers,
				Channel: ch,
			}
			out, err := sl.Run(context.Background(), tasks[0])
			fmt.Printf("outcome: %s — %s\n", out.Status, out.Detail)
			return err
		},
	}
	cmd.Flags().StringVar(&repo, "repo", ".", "仓库路径")
	cmd.Flags().StringVar(&inbox, "task-inbox", "", "任务 inbox 目录（用于错误提示；实际读 <repo>/inbox）")
	cmd.Flags().StringVar(&models, "models", "real", "real | fake（测试用）")
	return cmd
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
) {
	var m model.Client
	if mode == "fake" {
		f := model.NewFake(map[string]string{
			"TRIAGE:":  jsonStr(skill.TriageOutput{Startable: true, LoopDoable: true}),
			"PLAN:":    jsonStr(skill.PlanOutput{}),
			"EXECUTE:": "ok",
			"VERIFY:":  jsonStr(skill.VerifyOutput{Passed: true}),
		})
		m = f
		exec = f
	} else {
		c := model.NewClaudeClient(cfg.Models.Execute.Binary, cfg.Models.Execute.Cmd)
		m = c
		exec = c
	}
	plan = skill.Skill[skill.PlanInput, skill.PlanOutput]{
		Name: "plan", PromptTmpl: mustSkillPrompt("plan"),
		ParseJSON: parseJSON[skill.PlanOutput], Model: m,
	}
	vs = skill.Skill[skill.VerifyInput, skill.VerifyOutput]{
		Name: "verify", PromptTmpl: mustSkillPrompt("verify"),
		ParseJSON: parseJSON[skill.VerifyOutput],
		Model:     &budget.Client{Base: m, Enf: bz}, // ← verify-budget gap closed
	}
	triage = skill.Skill[skill.TriageInput, skill.TriageOutput]{
		Name: "triage", PromptTmpl: mustSkillPrompt("triage"),
		ParseJSON: parseJSON[skill.TriageOutput], Model: m,
	}
	return
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

// buildTiers returns the M1 verify chain. Per 裁决 E, the tier1 deterministic
// script is NOT wired here — M3 daemon will assemble tiers dynamically from
// cfg.Verify.Deterministic. M1 ships tier2 (LLM) + tier3 (HumanStub) only.
func buildTiers(_ *config.Config, _ string, vs skill.Skill[skill.VerifyInput, skill.VerifyOutput]) []verify.Tier {
	return []verify.Tier{verify.LLM{Skill: vs}, verify.HumanStub{}}
}

func parseJSON[O any](b []byte) (O, error) {
	var o O
	return o, json.Unmarshal(b, &o)
}

func jsonStr(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// mustLoad reads <repo>/.loop/config.yaml. M1 simplification: load failure
// panics (run-once is a dev/test entry point; M3 daemon will surface errors
// via issue comments).
func mustLoad(repo string) *config.Config {
	c, err := config.Load(filepath.Join(repo, ".loop", "config.yaml"))
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

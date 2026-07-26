// internal/cli/run_once_test.go
package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/template"

	"loop-eng/internal/budget"
	"loop-eng/internal/config"
	"loop-eng/internal/skill"
)

// TestRunOnceEndToEnd drives the M1 synchronous entry point end-to-end:
// init a repo → drop an inbox task → run-once --models fake → assert the
// terminal battle report landed in outbox (裁决 B1: every terminal outcome
// writes a channel report).
func TestRunOnceEndToEnd(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	// 先 init：生成 .loop/config.yaml + state.db + skills
	initCmd := NewRootCmd()
	initCmd.SetArgs([]string{"init", "--repo", repo})
	if err := initCmd.Execute(); err != nil {
		t.Fatalf("init: %v", err)
	}

	// 准备 inbox 任务
	inbox := filepath.Join(repo, "inbox")
	if err := os.MkdirAll(inbox, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inbox, "1.md"),
		[]byte("# 任务\ndo thing\ntype: bugfix\n## 验收标准\n- [ ] c"), 0644); err != nil {
		t.Fatal(err)
	}

	// run-once（用 --models=fake 注入 Fake，便于 CI）
	cmd := NewRootCmd()
	cmd.SetArgs([]string{"run-once", "--repo", repo, "--task-inbox", inbox, "--models", "fake"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	// 战报应写到 outbox（SubLoop.report → channel.PostComment → outbox/<ref>.md）
	if _, err := os.Stat(filepath.Join(repo, "outbox", "1.md")); err != nil {
		t.Fatalf("no battle report: %v", err)
	}
}

// TestRunOnceGitHubPathWiresChannel verifies run-once honors the --channel
// flag (here: local) and still wires the SubLoop end-to-end — init a repo,
// drop an inbox task, run-once --channel local --models fake, assert the
// terminal battle report landed in outbox. This is the assembly test for
// Task 6: --channel flag + buildChannel (tier-1 is now plan-driven, not config).
// It exercises the local provider path (no real `gh` call).
func TestRunOnceGitHubPathWiresChannel(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	initCmd := NewRootCmd()
	initCmd.SetArgs([]string{"init", "--repo", repo})
	if err := initCmd.Execute(); err != nil {
		t.Fatalf("init: %v", err)
	}
	inbox := filepath.Join(repo, "inbox")
	if err := os.MkdirAll(inbox, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inbox, "1.md"),
		[]byte("# 任务\ndo thing\ntype: bugfix\n## 验收标准\n- [ ] c"), 0644); err != nil {
		t.Fatal(err)
	}
	cmd := NewRootCmd()
	cmd.SetArgs([]string{"run-once", "--repo", repo, "--channel", "local", "--models", "fake"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, "outbox", "1.md")); err != nil {
		t.Fatalf("no battle report: %v", err)
	}
}

// TestVerifySkillCarriesAcceptanceCriteria guards spec §8.6 命门: the verify
// skill assembled by buildModels MUST render the acceptance criteria into its
// prompt — the verifier cannot judge a diff against criteria it cannot see.
// buildModels now sources the prompt from the embedded embed/skills/verify.md
// (which renders ALL VerifyInput fields), not the old truncated
// "VERIFY: {{.Diff}}" literal that omitted AcceptanceCriteria +
// PriorFailureSignal. This test fails the moment someone re-truncates the
// template or stops wiring the embedded file.
func TestVerifySkillCarriesAcceptanceCriteria(t *testing.T) {
	_, _, vs, _, _ := buildModels(nil, "fake", budget.New(1000, 10000, 1))

	prompt := renderTemplate(t, vs.PromptTmpl, skill.VerifyInput{
		Diff:               "diff body",
		AcceptanceCriteria: []string{"c1"},
		PriorFailureSignal: "none",
	})
	for _, want := range []string{"c1", "diff body", "none", "VERIFY:"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("verify prompt missing %q; got:\n%s", want, prompt)
		}
	}
}

// TestPlanPromptScriptIsPersonalized guards the reopened feedback on plan.md:
// the plan skill MUST instruct the planner to write a tier-1 verify_script that
// is personalized to its own implementation design — testing the specific
// functions/classes/signatures the plan introduces — and MUST NOT recommend
// project-wide smoke checks (`go test ./...`, `npm test`, lint, `cargo build`,
// …) as the tier-1 output. Those pass regardless of whether the task's work is
// correct, manufacturing a fake green light, and they misled the planner's
// thinking in earlier rounds. This test pins the fix and fails the moment
// plan.md regresses to the generic "适合脚本化（建议产出）" smoke-check examples.
func TestPlanPromptScriptIsPersonalized(t *testing.T) {
	p := mustSkillPrompt("plan")
	if !strings.Contains(p, "个性化") {
		t.Fatalf("plan.md 必须要求验收脚本针对 plan 自身的实现方案个性化定制")
	}
	// 旧的「建议产出」裸全仓命令（直接拿 go test ./... 当 tier-1）必须消失——假绿灯。
	if strings.Contains(p, `{"run":["go","test","./..."]}`) {
		t.Fatalf("plan.md 不得再把裸 `go test ./...` 当作 tier-1 验收脚本的推荐产出（假绿灯）")
	}
}

// renderTemplate is a test-only helper mirroring skill.render (unexported) so
// the test can assert on the rendered prompt without going through model.Call.
func renderTemplate(t *testing.T, tmpl string, in any) string {
	t.Helper()
	tpl, err := template.New("t").Parse(tmpl)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, in); err != nil {
		t.Fatalf("execute: %v", err)
	}
	return buf.String()
}

// TestPlanEmbedRendersRetryDiagnosis 钉死 plan embed（embed/skills/plan.md）含
// {{.RetryDiagnosis}} 条件块且能正确渲染：非空时整段渲染进 prompt、空时整段省略。
// 这保证 SubLoop 注入的 retryDiagnosisFor 产出能经生产用 plan 模板到达模型 +
// 落进 plan step input_json（dashboard 详情/Replay 可审计），而非只在测试用自定义模板里生效。
func TestPlanEmbedRendersRetryDiagnosis(t *testing.T) {
	tmpl := mustSkillPrompt("plan")

	// 非空 RetryDiagnosis → 必渲染进 prompt（条件块为真）。
	with := renderTemplate(t, tmpl, skill.PlanInput{
		Task:               "t",
		AcceptanceCriteria: []string{"c"},
		RetryDiagnosis:     "## 重试诊断\nSENTINEL_DIAG_42",
	})
	if !strings.Contains(with, "SENTINEL_DIAG_42") {
		t.Fatalf("plan embed 未渲染 RetryDiagnosis 字段（缺 {{.RetryDiagnosis}} 条件块？）:\n%s", with)
	}

	// 空 RetryDiagnosis → 条件块整段省略；诊断标识与 sentinel 都不得出现。
	without := renderTemplate(t, tmpl, skill.PlanInput{
		Task:               "t",
		AcceptanceCriteria: []string{"c"},
	})
	if strings.Contains(without, "SENTINEL_DIAG_42") {
		t.Fatalf("空 RetryDiagnosis 时 plan embed 不应渲染 sentinel:\n%s", without)
	}
	if strings.Contains(without, "重试诊断") {
		t.Fatalf("空 RetryDiagnosis 时 plan embed 不应渲染诊断块:\n%s", without)
	}
}

// TestPlanEmbedInstructsProactiveExploration 钉死 plan embed（embed/skills/plan.md）含
// 「规划前主动探索仓库」指令——修复「各阶段看到的代码仓库不一致」第 1 点（最关键）：
// plan 走 claude -p --dangerously-skip-permissions 本就有完整仓库探索能力，「盲规划」
// 根因是 plan.md 没引导探索，故改 prompt 而非喂死摘要（撤回 RepoStateSummary 方向）。
//
// 本测试**只断言结构落点**——embed 文本经 mustSkillPrompt("plan") 读出后含 4 个标记串
// （等价于 grep embed 源文件 internal/cli/embed/skills/plan.md）；**不**断言「plan 探索
// 后规划更好」——后者非确定性、不可机械判定（#47 教训）。读 embed 而非 .loop/skills：
// runtime 经 //go:embed embed/skills/*.md 读取，.loop/skills 只是 scaffold 副本，改它无效。
func TestPlanEmbedInstructsProactiveExploration(t *testing.T) {
	p := mustSkillPrompt("plan")
	for _, want := range []string{
		"只读不写",               // (b) 「只规划、不写代码」澄清为「只读不写」
		"repo-knowledge-map", // (a) 优先 repo-knowledge-map
		"主动探索",               // (a) 规划前主动探索
		"禁止发明不存在的文件",         // (c) 铁律：禁止发明不存在的文件/符号/字段
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("plan.md embed 缺标记串 %q（探索/只读指令未落进 embed？）:\n%s", want, p)
		}
	}
}

// TestProviderLabel: providerLabel renders the role's effective provider for
// steps.model_ref — "" normalizes to "claude" (the default), and a set model
// name appends as provider/name.
func TestProviderLabel(t *testing.T) {
	cases := []struct {
		ref  config.ModelRef
		want string
	}{
		{config.ModelRef{}, "claude"},
		{config.ModelRef{Provider: "codex"}, "codex"},
		{config.ModelRef{Provider: "codex", Name: "gpt-5.1"}, "codex/gpt-5.1"},
		{config.ModelRef{Name: "haiku"}, "claude/haiku"},
	}
	for _, c := range cases {
		if got := providerLabel(c.ref); got != c.want {
			t.Fatalf("providerLabel(%+v) = %q, want %q", c.ref, got, c.want)
		}
	}
}

// TestApplyTaskAgent: a task-level agent hint opts every role into the named
// provider (provider+binary reset to it, claude-specific cmd dropped so it
// doesn't get passed to the other binary, model name preserved). An empty OR
// unregistered agent leaves cfg unchanged (no crash on untrusted issue input).
func TestApplyTaskAgent(t *testing.T) {
	base := &config.Config{Models: config.Models{
		Execute: config.ModelRef{Provider: "", Binary: "claude", Name: "sonnet", Cmd: []string{"--dangerously-skip-permissions"}},
		Plan:    config.ModelRef{Provider: "", Binary: "claude", Cmd: []string{"--dangerously-skip-permissions"}},
	}}

	// codex override: each role switches provider+binary; cmd dropped; name kept.
	over := applyTaskAgent(base, "codex")
	if over == base {
		t.Fatal("applyTaskAgent(codex) must return a clone, not the same config")
	}
	if got := over.Models.Execute.Provider; got != "codex" {
		t.Fatalf("execute provider want codex, got %q", got)
	}
	if got := over.Models.Execute.Binary; got != "codex" {
		t.Fatalf("execute binary want codex, got %q", got)
	}
	if got := over.Models.Execute.Name; got != "sonnet" {
		t.Fatalf("execute model name should be preserved, got %q", got)
	}
	if len(over.Models.Execute.Cmd) != 0 {
		t.Fatalf("claude-specific cmd must be dropped on provider switch, got %v", over.Models.Execute.Cmd)
	}
	// base config is untouched (clone, not in-place mutation).
	if base.Models.Execute.Provider != "" || base.Models.Execute.Binary != "claude" {
		t.Fatalf("base config mutated: %+v", base.Models.Execute)
	}

	// empty agent → unchanged (same pointer, the common no-override path).
	if got := applyTaskAgent(base, ""); got != base {
		t.Fatal("empty agent must return cfg unchanged")
	}
	// unregistered agent → unchanged (no crash; falls back to default).
	if got := applyTaskAgent(base, "no-such-provider"); got != base {
		t.Fatal("unknown agent must fall back to cfg unchanged (no crash)")
	}

	// ReadOnly intent survives a provider switch (forProvider carries ReadOnly),
	// so a read-only role is NOT silently relaxed to writable when a task opts
	// into a different provider.
	roBase := &config.Config{Models: config.Models{
		Plan: config.ModelRef{Provider: "claude", Binary: "claude", Cmd: []string{"--disallowedTools"}, ReadOnly: true},
	}}
	roOver := applyTaskAgent(roBase, "codex")
	if !roOver.Models.Plan.ReadOnly {
		t.Fatalf("forProvider must carry ReadOnly across provider switch: got %+v", roOver.Models.Plan)
	}
}

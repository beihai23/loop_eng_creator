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
// Task 6: --channel flag + buildChannel + tier1 from cfg.Verify.Deterministic.
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
	_, _, vs, _ := buildModels(nil, "fake", budget.New(1000, 10000, 1))

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

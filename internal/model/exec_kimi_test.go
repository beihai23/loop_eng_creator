package model

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"loop-eng/internal/config"
)

// TestKimiAgentExecRunsInWorktreeDir is the kimi equivalent of
// TestCodexAgentExecRunsInWorktreeDir (exec_codex_test.go): the kimi provider,
// adapted to the frozen Executer interface, runs its binary with
// cmd.Dir=worktreeDir so the agent's edits land on the isolated worktree.
func TestKimiAgentExecRunsInWorktreeDir(t *testing.T) {
	bin := writeFakePwdBinary(t, "fake-kimi")
	wt := t.TempDir()
	a, err := NewAgent(config.ModelRef{Provider: "kimi", Binary: bin})
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := AsExecuter(a).Exec(context.Background(), wt, "do the task")
	if err != nil {
		t.Fatal(err)
	}
	abs, _ := filepath.Abs(wt)
	if !strings.Contains(out, abs) {
		t.Fatalf("kimi Exec should run in worktree %s, got %q", abs, out)
	}
}

// TestKimiAgentProviderAndCheck covers provider identity + doctor preflight:
// Provider() reports "kimi", and Check fails when the binary is absent (so
// loop-eng doctor can flag a missing kimi install via providerPreflight).
func TestKimiAgentProviderAndCheck(t *testing.T) {
	a, err := NewAgent(config.ModelRef{Provider: "kimi", Binary: "kimi"})
	if err != nil {
		t.Fatal(err)
	}
	if got := a.Provider(); got != "kimi" {
		t.Fatalf("provider want kimi, got %q", got)
	}
	missing, err := NewAgent(config.ModelRef{Provider: "kimi", Binary: "/no/such/kimi-bin-zzz"})
	if err != nil {
		t.Fatal(err)
	}
	if err := missing.Check(context.Background()); err == nil {
		t.Fatal("Check must fail when the kimi binary is missing")
	}
}

// TestKimiAgentDeliversPromptAsArgv guards the prompt-delivery contract: kimi
// takes the prompt as a POSITIONAL argv element after `-p` (NOT via stdin like
// codex). The fake binary echoes every argv element; the agent must pass the
// full prompt through as one positional argument.
func TestKimiAgentDeliversPromptAsArgv(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakeArgvBinary(t, "fake-kimi-argv")
	a, err := NewAgent(config.ModelRef{Provider: "kimi", Binary: bin})
	if err != nil {
		t.Fatal(err)
	}
	prompt := "do the task — prompt body that must reach kimi via positional argv"
	out, _, err := AsExecuter(a).Exec(context.Background(), dir, prompt)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, prompt) {
		t.Fatalf("kimi agent must pass the full prompt as a positional argv element; got %q", out)
	}
}

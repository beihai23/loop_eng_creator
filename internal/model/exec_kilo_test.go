package model

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"loop-eng/internal/config"
)

// TestKiloAgentExecRunsInWorktreeDir is the kilo equivalent of
// TestCodexAgentExecRunsInWorktreeDir (exec_codex_test.go): the kilo provider,
// adapted to the frozen Executer interface, runs its binary with
// cmd.Dir=worktreeDir so the agent's edits land on the isolated worktree.
func TestKiloAgentExecRunsInWorktreeDir(t *testing.T) {
	bin := writeFakePwdBinary(t, "fake-kilo")
	wt := t.TempDir()
	a, err := NewAgent(config.ModelRef{Provider: "kilo", Binary: bin})
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := AsExecuter(a).Exec(context.Background(), wt, "do the task")
	if err != nil {
		t.Fatal(err)
	}
	abs, _ := filepath.Abs(wt)
	if !strings.Contains(out, abs) {
		t.Fatalf("kilo Exec should run in worktree %s, got %q", abs, out)
	}
}

// TestKiloAgentProviderAndCheck covers provider identity + doctor preflight:
// Provider() reports "kilo", and Check fails when the binary is absent (so
// loop-eng doctor can flag a missing kilo install via providerPreflight).
func TestKiloAgentProviderAndCheck(t *testing.T) {
	a, err := NewAgent(config.ModelRef{Provider: "kilo", Binary: "kilo"})
	if err != nil {
		t.Fatal(err)
	}
	if got := a.Provider(); got != "kilo" {
		t.Fatalf("provider want kilo, got %q", got)
	}
	missing, err := NewAgent(config.ModelRef{Provider: "kilo", Binary: "/no/such/kilo-bin-zzz"})
	if err != nil {
		t.Fatal(err)
	}
	if err := missing.Check(context.Background()); err == nil {
		t.Fatal("Check must fail when the kilo binary is missing")
	}
}

// TestKiloAgentDeliversPromptAsArgv guards the prompt-delivery contract: kilo
// takes the prompt as a POSITIONAL argv element after `run --auto` (NOT via
// stdin like codex). The fake binary echoes every argv element; the agent must
// pass the full prompt through as one positional argument.
func TestKiloAgentDeliversPromptAsArgv(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakeArgvBinary(t, "fake-kilo-argv")
	a, err := NewAgent(config.ModelRef{Provider: "kilo", Binary: bin})
	if err != nil {
		t.Fatal(err)
	}
	prompt := "do the task — prompt body that must reach kilo via positional argv"
	out, _, err := AsExecuter(a).Exec(context.Background(), dir, prompt)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, prompt) {
		t.Fatalf("kilo agent must pass the full prompt as a positional argv element; got %q", out)
	}
}

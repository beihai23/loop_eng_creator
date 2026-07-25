package model

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"loop-eng/internal/config"
)

// TestOpencodeAgentExecRunsInWorktreeDir is the opencode equivalent of
// TestCodexAgentExecRunsInWorktreeDir (exec_codex_test.go): the opencode
// provider, adapted to the frozen Executer interface, runs its binary with
// cmd.Dir=worktreeDir so the agent's edits land on the isolated worktree.
func TestOpencodeAgentExecRunsInWorktreeDir(t *testing.T) {
	bin := writeFakePwdBinary(t, "fake-opencode")
	wt := t.TempDir()
	a, err := NewAgent(config.ModelRef{Provider: "opencode", Binary: bin})
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := AsExecuter(a).Exec(context.Background(), wt, "do the task")
	if err != nil {
		t.Fatal(err)
	}
	abs, _ := filepath.Abs(wt)
	if !strings.Contains(out, abs) {
		t.Fatalf("opencode Exec should run in worktree %s, got %q", abs, out)
	}
}

// TestOpencodeAgentProviderAndCheck covers provider identity + doctor preflight:
// Provider() reports "opencode", and Check fails when the binary is absent (so
// loop-eng doctor can flag a missing opencode install via providerPreflight).
func TestOpencodeAgentProviderAndCheck(t *testing.T) {
	a, err := NewAgent(config.ModelRef{Provider: "opencode", Binary: "opencode"})
	if err != nil {
		t.Fatal(err)
	}
	if got := a.Provider(); got != "opencode" {
		t.Fatalf("provider want opencode, got %q", got)
	}
	missing, err := NewAgent(config.ModelRef{Provider: "opencode", Binary: "/no/such/opencode-bin-zzz"})
	if err != nil {
		t.Fatal(err)
	}
	if err := missing.Check(context.Background()); err == nil {
		t.Fatal("Check must fail when the opencode binary is missing")
	}
}

// TestOpencodeAgentDeliversPromptAsArgv guards the prompt-delivery contract:
// opencode takes the prompt as a POSITIONAL argv element after `run` (NOT via
// stdin like codex). The fake binary echoes every argv element; the agent must
// pass the full prompt through as one positional argument.
func TestOpencodeAgentDeliversPromptAsArgv(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakeArgvBinary(t, "fake-opencode-argv")
	a, err := NewAgent(config.ModelRef{Provider: "opencode", Binary: bin})
	if err != nil {
		t.Fatal(err)
	}
	prompt := "do the task — prompt body that must reach opencode via positional argv"
	out, _, err := AsExecuter(a).Exec(context.Background(), dir, prompt)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, prompt) {
		t.Fatalf("opencode agent must pass the full prompt as a positional argv element; got %q", out)
	}
}

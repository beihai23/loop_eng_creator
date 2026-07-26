package model

import (
	"context"
	"errors"
	"os"
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

// TestKiloAgentFatalInsufficientBalanceAbortsImmediately pins the agent-smoke fix
// (task #87) for the kilo 同类: a kilo "Insufficient Balance" failure — an account
// with no credit — is FATAL. 1 attempt, ErrClaudeFatal, no 30/60/120s backoff.
func TestKiloAgentFatalInsufficientBalanceAbortsImmediately(t *testing.T) {
	withNoBackoff(t)
	bin, countDir := writeFakeFatalBinary(t, "fake-kilo-balance",
		"Error: Insufficient Balance")
	a, err := NewAgent(config.ModelRef{Provider: "kilo", Binary: bin})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = AsExecuter(a).Exec(context.Background(), t.TempDir(), "do the task")
	if !errors.Is(err, ErrClaudeFatal) {
		t.Fatalf("kilo balance error must be ErrClaudeFatal; got: %v", err)
	}
	countBytes, _ := os.ReadFile(filepath.Join(countDir, "count"))
	attempts := len(strings.Split(strings.TrimSpace(string(countBytes)), "\n"))
	if attempts != 1 {
		t.Fatalf("kilo balance error must abort after 1 attempt (no retry), got %d", attempts)
	}
}

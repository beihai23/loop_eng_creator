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

// TestOpencodeAgentFatalInsufficientBalanceAbortsImmediately pins the agent-smoke
// fix (task #87): an opencode "Insufficient Balance" failure — an account with
// no credit, an environment problem not a code bug — is FATAL. runWithRetry must
// return after exactly 1 attempt wrapped in ErrClaudeFatal, not burn the ~97s of
// empty backoff the smoke observed retrying a balance error retry cannot heal.
func TestOpencodeAgentFatalInsufficientBalanceAbortsImmediately(t *testing.T) {
	withNoBackoff(t)
	bin, countDir := writeFakeFatalBinary(t, "fake-opencode-balance",
		"Error: Insufficient Balance")
	a, err := NewAgent(config.ModelRef{Provider: "opencode", Binary: bin})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = AsExecuter(a).Exec(context.Background(), t.TempDir(), "do the task")
	if !errors.Is(err, ErrClaudeFatal) {
		t.Fatalf("opencode balance error must be ErrClaudeFatal; got: %v", err)
	}
	countBytes, _ := os.ReadFile(filepath.Join(countDir, "count"))
	attempts := len(strings.Split(strings.TrimSpace(string(countBytes)), "\n"))
	if attempts != 1 {
		t.Fatalf("opencode balance error must abort after 1 attempt (no retry), got %d", attempts)
	}
}

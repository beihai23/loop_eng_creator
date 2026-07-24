package model

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"loop-eng/internal/config"
)

// TestTier1AgentRegistry: NewAgent dispatches by ref.Provider.
// default/empty → claude (开箱行为不变); explicit codex → codex; unknown → error.
func TestTier1AgentRegistry(t *testing.T) {
	a, err := NewAgent(config.ModelRef{Provider: "", Binary: "claude"})
	if err != nil {
		t.Fatalf("default agent: %v", err)
	}
	if got := a.Provider(); got != "claude" {
		t.Fatalf("default provider want claude, got %q", got)
	}
	c, err := NewAgent(config.ModelRef{Provider: "codex", Binary: "codex"})
	if err != nil {
		t.Fatalf("codex agent: %v", err)
	}
	if got := c.Provider(); got != "codex" {
		t.Fatalf("codex provider want codex, got %q", got)
	}
	if _, err := NewAgent(config.ModelRef{Provider: "no-such-provider"}); err == nil {
		t.Fatal("unknown provider must return error")
	}
}

// TestTier1AgentCodexExecRunsInWorktree: codex provider, adapted to the frozen
// Executer interface, runs its binary with cmd.Dir=worktreeDir — the
// exec_test.go-equivalent contract for a 2nd provider.
func TestTier1AgentCodexExecRunsInWorktree(t *testing.T) {
	bin := writeTier1FakeBinary(t)
	wt := t.TempDir()
	a, err := NewAgent(config.ModelRef{Provider: "codex", Binary: bin})
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := AsExecuter(a).Exec(context.Background(), wt, "do the task")
	if err != nil {
		t.Fatal(err)
	}
	abs, _ := filepath.Abs(wt)
	if !strings.Contains(out, abs) {
		t.Fatalf("codex Exec should run in worktree %s, got %q", abs, out)
	}
}

// TestTier1AgentCheckMissingBinary: Check (doctor preflight) fails when the
// provider binary is absent.
func TestTier1AgentCheckMissingBinary(t *testing.T) {
	a, err := NewAgent(config.ModelRef{Provider: "codex", Binary: "/no/such/codex-bin-zzz"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Check(context.Background()); err == nil {
		t.Fatal("Check must fail when binary missing")
	}
}

func writeTier1FakeBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-codex")
	content := "#!/bin/sh\ncat >/dev/null\npwd\n"
	if runtime.GOOS == "windows" {
		path = filepath.Join(dir, "fake-codex.bat")
		content = "@echo off\r\ncd\r\n"
	}
	if err := os.WriteFile(path, []byte(content), 0755); err != nil {
		t.Fatal(err)
	}
	return path
}

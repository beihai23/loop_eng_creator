package model

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"loop-eng/internal/config"
)

// writeFakeCodexBinary writes a stand-in `codex` that discards stdin and prints
// its cwd — mirrors exec_test.go's writeFakeBinary so we can assert the codex
// provider's Exec sets cmd.Dir=worktreeDir without a real codex binary. Distinct
// name from writeFakeBinary / writeTier1FakeBinary so it never collides with a
// concurrently-present tier-1 verify script in the same package.
func writeFakeCodexBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	var path, content string
	if runtime.GOOS == "windows" {
		path = filepath.Join(dir, "fake-codex.bat")
		content = "@echo off\r\ncd\r\n"
	} else {
		path = filepath.Join(dir, "fake-codex")
		content = "#!/bin/sh\ncat >/dev/null\npwd\n"
	}
	if err := os.WriteFile(path, []byte(content), 0755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestCodexAgentExecRunsInWorktreeDir is the codex equivalent of
// TestClaudeClientExecRunsInWorktreeDir (exec_test.go): the codex provider,
// adapted to the frozen Executer interface, runs its binary with
// cmd.Dir=worktreeDir so the agent's edits land on the isolated worktree. This
// is the second provider passing the exec_test.go-equivalent contract.
func TestCodexAgentExecRunsInWorktreeDir(t *testing.T) {
	bin := writeFakeCodexBinary(t)
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

// TestCodexAgentProviderAndCheck covers the provider identity + doctor preflight
// contract: Provider() reports "codex", and Check fails when the binary is
// absent (so loop-eng doctor can flag a missing codex install).
func TestCodexAgentProviderAndCheck(t *testing.T) {
	a, err := NewAgent(config.ModelRef{Provider: "codex", Binary: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if got := a.Provider(); got != "codex" {
		t.Fatalf("provider want codex, got %q", got)
	}
	missing, err := NewAgent(config.ModelRef{Provider: "codex", Binary: "/no/such/codex-bin-zzz"})
	if err != nil {
		t.Fatal(err)
	}
	if err := missing.Check(context.Background()); err == nil {
		t.Fatal("Check must fail when the codex binary is missing")
	}
}

// TestCodexAgentDeliversPromptViaStdin guards the prompt-delivery contract: codex
// reads the prompt from stdin (not a positional arg). The fake binary echoes how
// many stdin bytes it received; the agent must pipe the full prompt through.
func TestCodexAgentDeliversPromptViaStdin(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-codex-stdin")
	// Prints the byte count of stdin it consumed.
	script := "#!/bin/sh\nwc -c | tr -d ' '\n"
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	a, err := NewAgent(config.ModelRef{Provider: "codex", Binary: path})
	if err != nil {
		t.Fatal(err)
	}
	prompt := "do the task — prompt body that must reach codex via stdin"
	out, _, err := AsExecuter(a).Exec(context.Background(), dir, prompt)
	if err != nil {
		t.Fatal(err)
	}
	// wc -c of the piped prompt = len(prompt) bytes (the agent must not drop it).
	if want, got := strconv.Itoa(len(prompt)), strings.TrimSpace(out); want != got {
		t.Fatalf("codex agent must pipe the full prompt to stdin: want %s bytes, got %q", want, got)
	}
}

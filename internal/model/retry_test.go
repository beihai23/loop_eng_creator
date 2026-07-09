package model

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFakeBinaryFailing writes a "claude" stand-in that ALWAYS exits 1 and
// appends one line per invocation to <dir>/count (so the test can assert how
// many attempts were made). It also prints a marker to stdout — claude prints
// its real error to stdout, which a discard-on-error caller would lose.
func writeFakeBinaryFailing(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-claude-fail")
	script := "#!/bin/sh\ncat >/dev/null\nN=$(cat \"" + dir + "/count\" 2>/dev/null | wc -l | tr -d ' '); N=$((N+1)); printf \"%s\\n\" \"$N\" >> \"" + dir + "/count\"\necho \"claude-stdout-err-marker\"\nexit 1\n"
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestClaudeClientRetriesOnExit1ThenFails: a flaky claude (always exit 1) must
// be retried claudeRetry times, then surface an error that INCLUDES stdout
// (the real claude error message, which claude prints to stdout, not stderr).
func TestClaudeClientRetriesOnExit1ThenFails(t *testing.T) {
	bin := writeFakeBinaryFailing(t)
	dir := filepath.Dir(bin)
	c := NewClaudeClient(bin, nil)
	_, _, err := c.Call(context.Background(), "x")
	if err == nil {
		t.Fatal("want error after all retries fail")
	}
	countBytes, _ := os.ReadFile(filepath.Join(dir, "count"))
	attempts := len(strings.Split(strings.TrimSpace(string(countBytes)), "\n"))
	if attempts != claudeRetry {
		t.Fatalf("want %d attempts (claudeRetry), got %d", claudeRetry, attempts)
	}
	if !strings.Contains(err.Error(), "claude-stdout-err-marker") {
		t.Fatalf("error must include stdout (claude prints errors there); got: %v", err)
	}
}

// TestClaudeClientRetriesThenSucceeds: a claude that fails the first two times
// then succeeds must surface the success (retry absorbs the transient flake).
func TestClaudeClientRetriesThenSucceeds(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-claude-recover")
	// Succeeds once count >= 2 (i.e. on the 2nd attempt).
	script := "#!/bin/sh\ncat >/dev/null\nN=$(cat \"" + dir + "/count\" 2>/dev/null | wc -l | tr -d ' '); N=$((N+1)); printf \"%s\\n\" \"$N\" >> \"" + dir + "/count\"\nif [ \"$N\" -ge 2 ]; then echo \"recovered\"; exit 0; fi\nexit 1\n"
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	c := NewClaudeClient(path, nil)
	out, _, err := c.Call(context.Background(), "x")
	if err != nil {
		t.Fatalf("retry should absorb a 2-failure flake; got err: %v", err)
	}
	if !strings.Contains(out, "recovered") {
		t.Fatalf("expected recovered output, got %q", out)
	}
}

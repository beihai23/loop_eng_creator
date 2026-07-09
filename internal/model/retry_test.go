package model

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withNoBackoff zeroes claudeBackoffBase for the test (retries are otherwise
// 1s/2s/4s — too slow for the suite) and restores it after.
func withNoBackoff(t *testing.T) {
	t.Helper()
	prev := claudeBackoffBase
	claudeBackoffBase = 0
	t.Cleanup(func() { claudeBackoffBase = prev })
}

// writeFakeBinaryFailing writes a "claude" stand-in that ALWAYS exits 1 (no
// fatal signal) and records one line per invocation to <dir>/count (so the test
// can assert how many attempts were made). Prints a marker to stdout — claude
// prints its real error to stdout, which a discard-on-error caller would lose.
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

// TestClaudeClientRetriesOnExit1ThenFails: a retryable flake (always exit 1, no
// auth signal) is retried claudeRetry times with backoff, then surfaces an error
// that INCLUDES stdout and is NOT ErrClaudeFatal.
func TestClaudeClientRetriesOnExit1ThenFails(t *testing.T) {
	withNoBackoff(t)
	bin := writeFakeBinaryFailing(t)
	dir := filepath.Dir(bin)
	c := NewClaudeClient(bin, "", nil)
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
		t.Fatalf("error must include stdout; got: %v", err)
	}
	if errors.Is(err, ErrClaudeFatal) {
		t.Fatalf("non-auth flake must NOT be fatal: %v", err)
	}
}

// TestClaudeClientRetriesThenSucceeds: a claude that fails once then succeeds
// must surface the success (retry absorbs the transient flake).
func TestClaudeClientRetriesThenSucceeds(t *testing.T) {
	withNoBackoff(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-claude-recover")
	script := "#!/bin/sh\ncat >/dev/null\nN=$(cat \"" + dir + "/count\" 2>/dev/null | wc -l | tr -d ' '); N=$((N+1)); printf \"%s\\n\" \"$N\" >> \"" + dir + "/count\"\nif [ \"$N\" -ge 2 ]; then echo \"recovered\"; exit 0; fi\nexit 1\n"
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	c := NewClaudeClient(path, "", nil)
	out, _, err := c.Call(context.Background(), "x")
	if err != nil {
		t.Fatalf("retry should absorb a 1-failure flake; got err: %v", err)
	}
	if !strings.Contains(out, "recovered") {
		t.Fatalf("expected recovered output, got %q", out)
	}
}

// TestClaudeClientFatalAbortsImmediately: an auth/credential error
// ("authentication required (401)" on stdout) is FATAL — no retry, no backoff,
// marked ErrClaudeFatal, and carries the stdout detail. Retrying an expired
// credential just wastes budget.
func TestClaudeClientFatalAbortsImmediately(t *testing.T) {
	withNoBackoff(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-claude-auth")
	script := "#!/bin/sh\ncat >/dev/null\nN=$(cat \"" + dir + "/count\" 2>/dev/null | wc -l | tr -d ' '); N=$((N+1)); printf \"%s\\n\" \"$N\" >> \"" + dir + "/count\"\necho \"Error: authentication required (401)\"\nexit 1\n"
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	c := NewClaudeClient(path, "", nil)
	_, _, err := c.Call(context.Background(), "x")
	if err == nil {
		t.Fatal("want error for fatal auth failure")
	}
	if !errors.Is(err, ErrClaudeFatal) {
		t.Fatalf("auth error must be ErrClaudeFatal; got: %v", err)
	}
	countBytes, _ := os.ReadFile(filepath.Join(dir, "count"))
	attempts := len(strings.Split(strings.TrimSpace(string(countBytes)), "\n"))
	if attempts != 1 {
		t.Fatalf("fatal error must NOT retry; want 1 attempt, got %d", attempts)
	}
	if !strings.Contains(err.Error(), "authentication required") {
		t.Fatalf("error must carry the stdout detail; got: %v", err)
	}
}

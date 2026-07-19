package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestTier1IsRetryablePushError pins the retry classifier: transient SSH/VPN
// network failures are retryable; auth/config/non-FF failures are not.
func TestTier1IsRetryablePushError(t *testing.T) {
	retryable := []string{
		"Connection closed by 198.18.0.68 port 22",
		"ssh: connect to host github.com port 22: Operation timed out",
		"fatal: Could not resolve hostname github.com",
		"Connection reset by peer",
	}
	for _, s := range retryable {
		if !isRetryablePushError(fmt.Errorf("%s", s)) {
			t.Errorf("isRetryablePushError(%q) = false, want true", s)
		}
	}
	permanent := []string{
		"Permission denied (publickey).",
		"fatal: No configured push destination.",
		"error: failed to push some refs (non-fast-forward)",
	}
	for _, s := range permanent {
		if isRetryablePushError(fmt.Errorf("%s", s)) {
			t.Errorf("isRetryablePushError(%q) = true, want false", s)
		}
	}
}

// TestTier1PushWithRetry pins the retry policy: up to 3 attempts, 2s/4s
// backoff (mirroring gh), retry only on retryable errors.
func TestTier1PushWithRetry(t *testing.T) {
	// success on first try: 1 call, no sleep
	calls := 0
	slept := 0
	if err := pushWithRetry(func() error { calls++; return nil }, func(time.Duration) { slept++ }); err != nil || calls != 1 || slept != 0 {
		t.Fatalf("first-try success: err=%v calls=%d slept=%d", err, calls, slept)
	}
	// transient fail x2 then success: 3 calls, sleeps 2s then 4s
	calls = 0
	var sleeps []time.Duration
	err := pushWithRetry(func() error {
		calls++
		if calls < 3 {
			return fmt.Errorf("Connection closed by 198.18.0.68 port 22")
		}
		return nil
	}, func(d time.Duration) { sleeps = append(sleeps, d) })
	if err != nil {
		t.Fatalf("retry-then-success: %v", err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
	if len(sleeps) != 2 || sleeps[0] != 2*time.Second || sleeps[1] != 4*time.Second {
		t.Fatalf("sleeps = %v, want [2s 4s]", sleeps)
	}
	// permanent error: no retry, immediate return
	calls = 0
	err = pushWithRetry(func() error { calls++; return fmt.Errorf("Permission denied (publickey).") }, func(time.Duration) {})
	if err == nil || calls != 1 {
		t.Fatalf("permanent: err=%v calls=%d, want err!=nil calls=1", err, calls)
	}
	// retryable error exhausting all attempts: 3 calls, final error returned
	calls = 0
	err = pushWithRetry(func() error { calls++; return fmt.Errorf("Connection closed by peer") }, func(time.Duration) {})
	if err == nil || calls != 3 {
		t.Fatalf("exhausted: err=%v calls=%d, want err!=nil calls=3", err, calls)
	}
}

// TestTier1LandFallbackPartial: push-final-failure must NOT FF-merge into
// local main, must keep branch + worktree, and must return a note containing
// "LAND PARTIAL" and the branch name.
func TestTier1LandFallbackPartial(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	wt := filepath.Join(repo, ".loop", "worktrees", "task-p-r1")
	branch := "loop/task-p-r1"
	addWorktreeWithCommit(t, repo, wt, branch)

	pushErr := fmt.Errorf("%w: %s", ErrPushFailed, "Connection closed by 198.18.0.68 port 22")
	extra := landFallback(repo, wt, branch, pushErr, func(string, ...any) {})
	if !strings.Contains(extra, "LAND PARTIAL") {
		t.Fatalf("extra missing LAND PARTIAL: %q", extra)
	}
	if !strings.Contains(extra, branch) {
		t.Fatalf("extra missing branch name: %q", extra)
	}
	if out, err := exec.Command("git", "-C", repo, "rev-parse", "--verify", branch).CombinedOutput(); err != nil {
		t.Fatalf("branch must be preserved on push failure: %v (%s)", err, out)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("worktree must be preserved on push failure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "new.go")); !os.IsNotExist(err) {
		t.Fatalf("push failure must NOT FF-merge into local main")
	}
}

// TestTier1LandFallbackFFMerge: non-push PR failures (gh unavailable etc.)
// keep the existing local FF-merge fallback.
func TestTier1LandFallbackFFMerge(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	wt := filepath.Join(repo, ".loop", "worktrees", "task-q-r1")
	branch := "loop/task-q-r1"
	addWorktreeWithCommit(t, repo, wt, branch)

	extra := landFallback(repo, wt, branch, fmt.Errorf("gh pr create: boom"), func(string, ...any) {})
	if extra != "" {
		t.Fatalf("clean FF-merge should return empty extra, got %q", extra)
	}
	if _, err := os.Stat(filepath.Join(repo, "new.go")); err != nil {
		t.Fatalf("main missing landed file: %v", err)
	}
}

// TestTier1CreatePRWrapsPushFailure: a push failure surfaces as ErrPushFailed
// so callers can distinguish it from gh-side failures.
func TestTier1CreatePRWrapsPushFailure(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	wt := filepath.Join(repo, ".loop", "worktrees", "task-r-r1")
	branch := "loop/task-r-r1"
	addWorktreeWithCommit(t, repo, wt, branch)

	if _, err := createPR(repo, "x/y", branch, wt, "t", "b"); !errors.Is(err, ErrPushFailed) {
		t.Fatalf("createPR push failure must wrap ErrPushFailed, got %v", err)
	}
}

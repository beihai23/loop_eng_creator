// internal/cli/land_partial_test.go
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"loop-eng/internal/channel"
)

// TestPushRetryableClassifier: transient SSH/VPN/DNS failures are retryable;
// auth/config/ref-rejection failures are not (retrying those burns time for
// nothing). Mirrors the signals seen in the wild (#57: "Connection closed by
// 198.18.0.68 port 22" — a VPN TUN gateway).
func TestPushRetryableClassifier(t *testing.T) {
	if isRetryablePushError(nil) {
		t.Fatal("nil error must not be retryable")
	}
	for _, s := range []string{
		"Connection closed by 198.18.0.68 port 22",
		"ssh: connect to host github.com port 22: Operation timed out",
		"fatal: Could not resolve hostname github.com",
		"Connection reset by peer",
		"kex_exchange_identification: read: Connection timed out",
		"fatal: unable to access 'https://...': Failed to connect to github.com port 443",
	} {
		if !isRetryablePushError(fmt.Errorf("%s", s)) {
			t.Errorf("isRetryablePushError(%q) = false, want true", s)
		}
	}
	for _, s := range []string{
		"Permission denied (publickey).",
		"fatal: No configured push destination.",
		"error: failed to push some refs (non-fast-forward)",
		"remote: Repository not found.",
	} {
		if isRetryablePushError(fmt.Errorf("%s", s)) {
			t.Errorf("isRetryablePushError(%q) = true, want false", s)
		}
	}
}

// TestPushWithRetryBackoffAndExhaustion pins the retry policy (mirrors gh's
// ghRetry=3, 2s/4s backoff): success first try = 1 call no sleep; transient
// ×2 then success = 3 calls, sleeps [2s 4s]; permanent = no retry; retryable
// exhausted = 3 calls, last error returned.
func TestPushWithRetryBackoffAndExhaustion(t *testing.T) {
	calls, slept := 0, 0
	if err := pushWithRetry(func() error { calls++; return nil }, func(time.Duration) { slept++ }); err != nil || calls != 1 || slept != 0 {
		t.Fatalf("first-try success: err=%v calls=%d slept=%d", err, calls, slept)
	}

	calls = 0
	var sleeps []time.Duration
	err := pushWithRetry(func() error {
		calls++
		if calls < 3 {
			return fmt.Errorf("Connection closed by 198.18.0.68 port 22")
		}
		return nil
	}, func(d time.Duration) { sleeps = append(sleeps, d) })
	if err != nil || calls != 3 {
		t.Fatalf("retry-then-success: err=%v calls=%d, want nil/3", err, calls)
	}
	if len(sleeps) != 2 || sleeps[0] != 2*time.Second || sleeps[1] != 4*time.Second {
		t.Fatalf("sleeps = %v, want [2s 4s]", sleeps)
	}

	calls = 0
	err = pushWithRetry(func() error { calls++; return fmt.Errorf("Permission denied (publickey).") }, func(time.Duration) { slept++ })
	if err == nil || calls != 1 {
		t.Fatalf("permanent error must not retry: err=%v calls=%d, want err/1", err, calls)
	}

	calls = 0
	err = pushWithRetry(func() error { calls++; return fmt.Errorf("Connection closed by peer") }, func(time.Duration) {})
	if err == nil || calls != 3 {
		t.Fatalf("exhausted retryable: err=%v calls=%d, want err/3", err, calls)
	}
}

// TestCreatePRPushFailureMarkedPartial: with no origin configured the push
// fails permanently ("No configured push destination" — non-retryable, no
// sleep) and the error must wrap ErrPushFailed so callers can pick the partial
// path; the worktree + branch stay intact (createPR never discards on push
// failure).
func TestCreatePRPushFailureMarkedPartial(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	wt := filepath.Join(repo, ".loop", "worktrees", "task-cf-r1")
	branch := "loop/task-cf-r1"
	addWorktreeWithCommit(t, repo, wt, branch)

	if _, err := createPR(repo, "x/y", branch, wt, "t", "b"); !errors.Is(err, ErrPushFailed) {
		t.Fatalf("createPR push failure must wrap ErrPushFailed, got %v", err)
	}
	if out, err := exec.Command("git", "-C", repo, "rev-parse", "--verify", branch).CombinedOutput(); err != nil {
		t.Fatalf("branch must be preserved on push failure: %v (%s)", err, out)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("worktree must be preserved on push failure: %v", err)
	}
}

// TestLandFallbackPushFailurePartial: the core bug — push 最终失败时不得
// FF-merge 进本地 main（制造本地分叉 + operator 无感知），必须保留 branch +
// worktree 并返回 LAND PARTIAL 标记（含 branch 名与手动恢复指令）。
func TestLandFallbackPushFailurePartial(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	wt := filepath.Join(repo, ".loop", "worktrees", "task-lf-r1")
	branch := "loop/task-lf-r1"
	addWorktreeWithCommit(t, repo, wt, branch)

	var logged []string
	logf := func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	pushErr := fmt.Errorf("%w: %s", ErrPushFailed, "Connection closed by 198.18.0.68 port 22")
	extra := landFallback(repo, wt, branch, pushErr, logf)

	if !strings.Contains(extra, "LAND PARTIAL") {
		t.Fatalf("extra missing LAND PARTIAL marker: %q", extra)
	}
	if !strings.Contains(extra, branch) {
		t.Fatalf("extra missing branch name: %q", extra)
	}
	if !strings.Contains(extra, "git push -u origin "+branch) {
		t.Fatalf("extra missing manual recovery instruction: %q", extra)
	}
	if len(logged) == 0 {
		t.Fatal("partial path must log a loud warning")
	}
	// 不 merge、不 Discard
	if _, err := os.Stat(filepath.Join(repo, "new.go")); !os.IsNotExist(err) {
		t.Fatal("push failure must NOT FF-merge into local main")
	}
	if out, err := exec.Command("git", "-C", repo, "rev-parse", "--verify", branch).CombinedOutput(); err != nil {
		t.Fatalf("branch must be preserved (not Discarded): %v (%s)", err, out)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("worktree must be preserved (not Discarded): %v", err)
	}
}

// TestLandFallbackGhFailureFFMerges: 非 push 类 PR 失败（push 已成功、
// gh pr create 挂了 / 无 gh）保持既有 FF-merge 兜底 —— push 成功路径不回归。
func TestLandFallbackGhFailureFFMerges(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	wt := filepath.Join(repo, ".loop", "worktrees", "task-lg-r1")
	branch := "loop/task-lg-r1"
	addWorktreeWithCommit(t, repo, wt, branch)

	extra := landFallback(repo, wt, branch, fmt.Errorf("gh pr create: boom"), func(string, ...any) {})
	if extra != "" {
		t.Fatalf("clean FF-merge fallback should return empty extra, got %q", extra)
	}
	if _, err := os.Stat(filepath.Join(repo, "new.go")); err != nil {
		t.Fatalf("main missing landed file: %v", err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatal("worktree should be discarded after clean FF-merge")
	}
}

// TestHandlePRFailurePartialComment: 模拟 daemon/run-once 的 push 失败路径 —
// done detail 标注（返回值）与 issue 评论（channel outbox）都必须带 LAND
// PARTIAL，branch 不得被 Discard。非 push 失败则不发 partial 评论。
func TestHandlePRFailurePartialComment(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	wt := filepath.Join(repo, ".loop", "worktrees", "task-hp-r1")
	branch := "loop/task-hp-r1"
	addWorktreeWithCommit(t, repo, wt, branch)

	ch := channel.NewLocal(repo)
	pushErr := fmt.Errorf("%w: %s", ErrPushFailed, "Connection closed by 198.18.0.68 port 22")
	extra := handlePRFailure(context.Background(), ch, "42", repo, wt, branch, pushErr, func(string, ...any) {})

	if !strings.Contains(extra, "LAND PARTIAL") {
		t.Fatalf("done-detail note missing LAND PARTIAL: %q", extra)
	}
	// issue 评论带 partial 标记（operator 可见）
	comments, err := os.ReadFile(filepath.Join(repo, "outbox", "42.md"))
	if err != nil {
		t.Fatalf("partial comment must be posted to the channel: %v", err)
	}
	if !strings.Contains(string(comments), "LAND PARTIAL") {
		t.Fatalf("issue comment missing LAND PARTIAL: %q", comments)
	}
	// branch 保留供手动 push
	if out, err := exec.Command("git", "-C", repo, "rev-parse", "--verify", branch).CombinedOutput(); err != nil {
		t.Fatalf("branch must be preserved for manual push: %v (%s)", err, out)
	}

	// 非 push 失败：FF-merge 兜底成功，无 partial 评论
	repo2 := t.TempDir()
	initGitRepo(t, repo2)
	wt2 := filepath.Join(repo2, ".loop", "worktrees", "task-hp-r2")
	branch2 := "loop/task-hp-r2"
	addWorktreeWithCommit(t, repo2, wt2, branch2)
	ch2 := channel.NewLocal(repo2)
	extra2 := handlePRFailure(context.Background(), ch2, "43", repo2, wt2, branch2, fmt.Errorf("gh pr create: boom"), func(string, ...any) {})
	if extra2 != "" {
		t.Fatalf("clean FF-merge should return empty extra, got %q", extra2)
	}
	if _, err := os.Stat(filepath.Join(repo2, "outbox", "43.md")); !os.IsNotExist(err) {
		t.Fatal("non-push failure must not post a partial comment")
	}
}

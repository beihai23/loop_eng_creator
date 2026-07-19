// internal/cli/land.go
package cli

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"loop-eng/internal/channel"
	"loop-eng/internal/isolation"
)

// ErrPushFailed marks a createPR failure whose root cause is `git push origin`
// (as opposed to a gh-side failure after a successful push). Callers use
// errors.Is(err, ErrPushFailed) to pick the partial-land path: push failed
// means the code is ONLY local, so FF-merging it into a local main that can
// never be pushed (origin rejects / main diverges) silently strands the work —
// instead the branch + worktree are kept and the outcome is marked LAND
// PARTIAL. Pinned by spec docs/superpowers/specs/land-push-partial.md.
var ErrPushFailed = errors.New("git push failed")

// pushAttempts mirrors channel/github.go ghRetry: the GitHub API and SSH-over-
// VPN both fail transiently, and one blip must not abort the whole run.
const pushAttempts = 3

// pushRetryableSignals are the (lowercased) substrings of transient network
// failures seen in `git push` output — SSH/VPN blips (e.g. "Connection closed
// by 198.18.0.68 port 22", a TUN gateway), DNS hiccups, TCP timeouts. Auth
// failures ("permission denied"), config errors ("no configured push
// destination") and ref rejections ("non-fast-forward") are NOT here: retrying
// those just burns time.
var pushRetryableSignals = []string{
	"connection closed",
	"connection reset",
	"connection refused",
	"connection timed out",
	"could not resolve hostname",
	"temporary failure in name resolution",
	"network is unreachable",
	"operation timed out",
	"failed to connect",
	"ssh: connect to host",
}

// isRetryablePushError reports whether a `git push` failure looks like a
// transient network error worth retrying (SSH/VPN抖动). Pinned signature —
// see land-push-partial spec.
func isRetryablePushError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, sig := range pushRetryableSignals {
		if strings.Contains(msg, sig) {
			return true
		}
	}
	return false
}

// pushWithRetry runs push up to pushAttempts times, sleeping attempt*2s
// (2s/4s backoff — mirroring gh's ghRetry strategy) between attempts when the
// failure is retryable. Permanent errors return immediately; exhausting all
// attempts returns the last error. sleep is injectable for tests.
func pushWithRetry(push func() error, sleep func(time.Duration)) error {
	var lastErr error
	for attempt := 1; attempt <= pushAttempts; attempt++ {
		if err := push(); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if attempt < pushAttempts && isRetryablePushError(lastErr) {
			sleep(time.Duration(attempt) * 2 * time.Second)
			continue
		}
		return lastErr
	}
	return lastErr
}

// land merges a done task's committed worktree branch into the repo's current
// branch (fast-forward only — the daemon is single-active/synchronous, so the
// branch is always exactly one commit ahead of main), then removes the worktree
// and its branch. Called only on Outcome.Status=="done" with a non-empty Branch
// (SubLoop.Run commits the execute output on that branch before returning done).
//
// If the FF merge cannot apply (main moved under us — e.g. a concurrent
// commit), land returns an error and LEAVES the worktree + branch intact so the
// operator can merge manually; the done work is never lost. This is the caller
// side of the done-land fix paired with loop.SubLoop's commitWorktree.
func land(repo, wt, branch string) error {
	if err := runGit(repo, "merge", "--ff-only", branch); err != nil {
		return fmt.Errorf("git merge --ff-only %s: %w", branch, err)
	}
	if err := isolation.Discard(repo, wt); err != nil {
		return fmt.Errorf("discard worktree: %w", err)
	}
	return nil
}

// landFallback decides what to do when createPR failed, and returns the note to
// append to the done detail ("" = clean integration, nothing to surface):
//
//   - push 最终失败（errors.Is(prErr, ErrPushFailed)）：不 FF-merge 到本地
//     main —— merge 进一个 push 不上去的 main 会制造本地分叉（本地 main 领先
//     origin，下次 git pull --ff-only 失败、强推又会把这份带上去），且 operator
//     完全感知不到。改为保留 branch + worktree（不 Discard，供手动 push），
//     返回含字面量 LAND PARTIAL、branch 名、手动恢复指令与 push 错误摘要的标记串。
//   - 其他 PR 失败（push 已成功、gh pr create 挂了；或无 remote/gh 不可用）：
//     走既有 FF-merge 兜底，成功返回 ""，失败返回 "[land failed: ...]"（branch
//     保留，与 land() 的非 FF 语义一致）。
func landFallback(repo, wt, branch string, prErr error, logf func(format string, args ...any)) string {
	if errors.Is(prErr, ErrPushFailed) {
		logf("LAND PARTIAL: push origin 最终失败，不做本地 FF-merge（避免本地 main 领先 origin 制造分叉）；branch %s 与 worktree 已保留，待手动 push: %v", branch, prErr)
		return "[LAND PARTIAL: 仅本地分支 " + branch + "，push 失败——代码未上 origin、无 PR。" +
			"手动恢复: git push -u origin " + branch + "（push 成功后 gh pr create --base main --head " + branch + "）。" +
			"push 错误: " + prErr.Error() + "]"
	}
	if err := land(repo, wt, branch); err != nil {
		logf("land failed (work safe on branch %s): %v", branch, err)
		return "[land failed: " + err.Error() + "]"
	}
	logf("landed on main (FF-merge fallback, branch %s)", branch)
	return ""
}

// handlePRFailure is the shared createPR-failure path for daemon runTask and
// run-once: it runs landFallback and, on a final push failure, additionally
// posts the LAND PARTIAL note as an issue comment — the DONE battle report was
// already posted by SubLoop.report BEFORE land ran, so the partial marker can
// only reach the operator as a follow-up comment. Comment failure is logged
// but never flips the outcome (same writeback-fault tolerance as report).
// Returns the note for the caller to append to the done detail.
func handlePRFailure(ctx context.Context, ch channel.Channel, issueRef, repo, wt, branch string, prErr error, logf func(format string, args ...any)) string {
	extra := landFallback(repo, wt, branch, prErr, logf)
	if errors.Is(prErr, ErrPushFailed) && ch != nil {
		if err := ch.PostComment(ctx, issueRef, extra); err != nil {
			logf("LAND PARTIAL comment failed (marker still in done detail): %v", err)
		}
	}
	return extra
}

// createPR pushes the committed worktree branch to origin, creates a GitHub PR,
// and cleans up the worktree. Returns the PR URL on success. The push is
// retried on transient network errors (SSH/VPN blips — see #57); a push that
// still fails after pushWithRetry is wrapped in ErrPushFailed so the caller
// can take the LAND PARTIAL path instead of silently FF-merging into a local
// main that never reaches GitHub. A gh-side failure after a successful push is
// NOT ErrPushFailed — the caller falls back to land() (FF-merge to local main).
func createPR(repo, ghRepo, branch, wt, title, body string) (string, error) {
	if err := pushWithRetry(func() error {
		return runGit(repo, "push", "-u", "origin", branch)
	}, time.Sleep); err != nil {
		return "", fmt.Errorf("%w: %v", ErrPushFailed, err)
	}
	args := []string{"pr", "create", "--repo", ghRepo, "--base", "main",
		"--head", branch, "--title", title, "--body", body}
	cmd := exec.Command("gh", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("gh pr create: %s: %w", string(out), err)
	}
	prURL := strings.TrimSpace(string(out))
	if err := isolation.Discard(repo, wt); err != nil {
		return prURL, fmt.Errorf("discard worktree: %w (PR created: %s)", err, prURL)
	}
	return prURL, nil
}

// runGit runs `git -C repo <args...>` and returns the combined output + error.
// Shared by land; mirrors loop.execGit but lives in the cli package (the caller
// side, which does the main-branch merge isolation can't reach from inside the
// worktree).
func runGit(repo string, args ...string) error {
	full := append([]string{"-C", repo}, args...)
	out, err := exec.Command("git", full...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w", out, err)
	}
	return nil
}

// internal/cli/land.go
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
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

// prTitleFor computes the GitHub PR title for a done task. It prefers the issue
// title (channel.Task.Title) so a body that opens with a "# 目标"/"# 现状"
// markdown heading does not leak a mid-body bullet into the PR title (the
// #74/#76 bug). When Title is empty — a task ingested before that field existed
// or a channel with no separate title — it falls back to Description (the
// distilled body first line), preserving the pre-fix behavior. Both empty fall
// back to the synthetic ref string. Pure function so the tier-1 test can pin it.
func prTitleFor(title, description, ref string) string {
	if title != "" {
		return title
	}
	if description != "" {
		return description
	}
	return "loop-eng task #" + ref
}

// prBodyFor computes the GitHub PR body for a done task: a base of
// "Closes #<ref>\n\nauto-generated by loop-eng", with the issue title prepended
// (as its own paragraph) when present so reviewers see the headline above the
// Closes line. An empty title prepends nothing — no leading blank line. Pure
// function so the tier-1 test can pin it.
func prBodyFor(title, ref string) string {
	base := "Closes #" + ref + "\n\nauto-generated by loop-eng"
	if title == "" {
		return base
	}
	return title + "\n\n" + base
}

// LandResult is finalizeLand's decision: whether the done work is fully
// integrated, and — if not — which branch it still lives on (so the caller can
// record it for the daemon's merge-pending reconcile). Note is appended to the
// done battle-report detail (empty = clean integration, nothing to surface).
type LandResult struct {
	Integrated bool   // true = work landed on main (local FF-merge); issue closed. false = still pending (PR/partial/land-fail); issue left OPEN.
	Branch     string // when !Integrated, the branch the work lives on (recorded as land_branch); empty when Integrated (branch was FF-merged + discarded).
	Note       string // appended to the done detail; e.g. "PR 待合并：<url>" or the LAND PARTIAL marker.
}

// finalizeLand turns a createPR result into a close-vs-defer decision — the
// single place that decides whether a done task's issue closes now or stays open
// pending merge. It does NOT call createPR itself (the caller passes prURL/prErr)
// so the three-way decision is unit-testable in isolation.
//
// Decision (mirrors the DoD: PR path defers, local-FF-merge path closes):
//   - prErr == nil (PR created): issue stays OPEN; post "PR 待合并：<url>，合并后
//     自动关闭" so the operator knows a merge is expected; return Branch for the
//     caller to record (reconcile closes the issue once the PR merges).
//   - errors.Is(prErr, ErrPushFailed) (LAND PARTIAL): handlePRFailure keeps the
//     branch + worktree and posts the partial marker; issue stays OPEN; Branch
//     returned for reconcile to poll.
//   - any other gh-side failure: handlePRFailure runs the local land() FF-merge
//     fallback. extra == "" → the local merge succeeded → the work is integrated
//     → CloseIssue now (Integrated=true, regression: local direct-land still
//     closes the issue). extra != "" → land failed (non-FF) → issue stays OPEN,
//     Branch returned.
//
// ch may be nil (caller has no channel); the close/comment side-effects are then
// skipped but the decision (Integrated/Branch/Note) is still returned.
func finalizeLand(ctx context.Context, ch channel.Channel, issueRef, repo, worktree, branch, prURL string, prErr error, logf func(format string, args ...any)) LandResult {
	// PR 创建成功：工作尚未合并——issue 留开，发「待合并」评论（含 URL + 合并后自动关闭）。
	if prErr == nil {
		note := "PR 待合并：" + prURL + "，合并后自动关闭"
		if ch != nil {
			if err := ch.PostComment(ctx, issueRef, note); err != nil {
				logf("PR 待合并 comment failed for %s: %v", issueRef, err)
			}
		}
		return LandResult{Integrated: false, Branch: branch, Note: note}
	}
	// createPR 失败：LAND PARTIAL（push 失败）或本地 FF-merge 兜底，都由 handlePRFailure 统一决策。
	extra := handlePRFailure(ctx, ch, issueRef, repo, worktree, branch, prErr, logf)
	if errors.Is(prErr, ErrPushFailed) {
		// LAND PARTIAL：issue 留开，branch + worktree 保留待手动 push。
		return LandResult{Integrated: false, Branch: branch, Note: extra}
	}
	// 其他 gh 侧失败：handlePRFailure 已走 land() FF-merge 兜底。
	// extra=="" → 本地合并成功 → 集成完成 → 关 issue（本地直落回归：done 后照旧关 issue）。
	// extra!="" → land 失败 → issue 留开、branch 保留。
	if extra == "" {
		if ch != nil {
			if err := ch.CloseIssue(ctx, issueRef); err != nil {
				logf("CloseIssue after local FF-merge failed for %s: %v", issueRef, err)
			}
		}
		return LandResult{Integrated: true, Branch: "", Note: ""}
	}
	return LandResult{Integrated: false, Branch: branch, Note: extra}
}

// createPR pushes the committed worktree branch to origin, creates a GitHub PR,
// and cleans up the worktree. Returns the PR URL on success. The push is
// retried on transient network errors (SSH/VPN blips — see #57); a push that
// still fails after pushWithRetry is wrapped in ErrPushFailed so the caller
// can take the LAND PARTIAL path instead of silently FF-merging into a local
// main that never reaches GitHub. A gh-side failure after a successful push is
// NOT ErrPushFailed — the caller falls back to land() (FF-merge to local main).
// headVerifyAttempts polls the PR head this many times before giving up.
const headVerifyAttempts = 5

// headVerifyWait is the delay between head-verification polls (var so tests can
// shrink it; production sleeps real time).
var headVerifyWait = 3 * time.Second

// gitBranchTip returns the SHA of a local branch — the tip just pushed to origin
// — or "" on error. Used to confirm a created PR's head matches the pushed tip.
func gitBranchTip(repo, branch string) string {
	out, err := exec.Command("git", "-C", repo, "rev-parse", branch).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// prHeadOf returns the open PR's head SHA for a branch via `gh pr list`. The PR
// head is what GitHub (auto-merge) or a human will merge at; if it lags behind
// the pushed tip, a merge misses commits (#97/#115).
func prHeadOf(ghRepo, branch string) (string, error) {
	out, err := exec.Command("gh", "pr", "list", "--repo", ghRepo, "--head", branch,
		"--state", "open", "--json", "headRefOid", "--limit", "1").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("gh pr list --head %s: %s: %w", branch, out, err)
	}
	var rows []struct {
		HeadRefOid string `json:"headRefOid"`
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		return "", fmt.Errorf("parse gh pr list: %w", err)
	}
	if len(rows) == 0 {
		return "", nil
	}
	return rows[0].HeadRefOid, nil
}

// verifyPRHead polls fetch() until it returns wantTip (the pushed branch tip),
// confirming the PR's headRefOid converged to the right commit. Returns nil on
// convergence, an error if it won't within attempts. GitHub's PR↔branch head sync
// can lag (transient; worse during API instability), and a merge at a stale head
// misses commits pushed after PR creation (#97/#115). fetch/sleep/attempts are
// params so tests simulate lag without shelling out or really sleeping.
func verifyPRHead(wantTip string, fetch func() (string, error), sleep func(time.Duration), attempts int) error {
	var last string
	for attempt := 1; attempt <= attempts; attempt++ {
		head, err := fetch()
		if err == nil {
			last = head
			if head == wantTip {
				return nil
			}
		}
		if attempt < attempts {
			sleep(headVerifyWait)
		}
	}
	return fmt.Errorf("PR head %q != branch tip %q after %d polls — GitHub head-sync stalled; merge may miss commits (verify head==tip before merging)", last, wantTip, attempts)
}

// errNoGitHubRemote signals the code repo has no GitHub remote — no PR path.
// landFallback treats it (like any non-push PR failure) as "FF-merge directly".
var errNoGitHubRemote = errors.New("no GitHub remote on code repo: no PR path (FF-merge instead)")

// parseGitHubOwnerName extracts "owner/name" from a GitHub remote URL (SSH or
// HTTPS, with/without .git, with an embedded token), or "" if it isn't GitHub.
// Pure so the URL-shape test can pin it without shelling out.
func parseGitHubOwnerName(remoteURL string) string {
	u := strings.TrimSpace(remoteURL)
	i := strings.Index(u, "github.com")
	if i < 0 {
		return ""
	}
	rest := u[i+len("github.com"):]
	rest = strings.TrimLeft(rest, ":/") // SSH ':' (git@github.com:owner/name) or HTTPS '/'
	rest = strings.TrimSuffix(rest, ".git")
	rest = strings.TrimRight(rest, "/")
	// exactly owner/name — one '/', no spaces/colons/@ (scheme/token leftovers).
	if rest == "" || strings.Count(rest, "/") != 1 || strings.ContainsAny(rest, " :@") {
		return ""
	}
	return rest
}

// codeGitHubRepo derives the GitHub owner/name of the repo's `origin` remote —
// the CODE repo, where done work lands via PR, independent of the task channel
// (GitHub issues vs Linear vs ...). The task channel tracks task STATUS; the
// code repo is where the work merges. For a GitHub task channel these are the
// same repo; for Linear they differ — landing must target the code repo, not the
// (empty) channel repo. "" if origin isn't GitHub.
func codeGitHubRepo(repo string) string {
	out, err := exec.Command("git", "-C", repo, "remote", "get-url", "origin").Output()
	if err != nil {
		return ""
	}
	return parseGitHubOwnerName(string(out))}

func createPR(repo, ghRepo, branch, wt, title, body string) (string, error) {
	// ghRepo is the task channel's repo (GitHub channel). For a non-GitHub task
	// channel (Linear) it's empty — but the CODE still lands in this repo's GitHub
	// remote (origin), independent of where tasks come from. Derive it so landing
	// is channel-agnostic: GitHub and Linear tasks both PR into the code repo.
	if ghRepo == "" {
		ghRepo = codeGitHubRepo(repo)
	}
	if ghRepo == "" {
		// No GitHub remote → no PR path. Return without pushing so landFallback
		// FF-merges the branch directly (non-GitHub code repos; a direct-push
		// strategy is a future refinement).
		return "", errNoGitHubRemote
	}
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
	// Verify the PR's head matches the branch tip we just pushed (before Discard,
	// while the branch ref is still resolvable). GitHub's PR↔branch head sync can
	// lag, and a merge at a stale head misses commits (#97/#115). Non-fatal: the
	// PR is created + push succeeded; this surfaces a stall so the operator
	// verifies head==tip before merging. Mirrors pushWithRetry's injectable shape.
	if tip := gitBranchTip(repo, branch); tip != "" {
		if verr := verifyPRHead(tip, func() (string, error) { return prHeadOf(ghRepo, branch) }, time.Sleep, headVerifyAttempts); verr != nil {
			log.Printf("createPR: %v (PR %s created — verify head==tip before merge)", verr, prURL)
		}
	}
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

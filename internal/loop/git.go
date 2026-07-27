package loop

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

func execGit(repo string, args ...string) (string, error) {
	full := append([]string{"-C", repo}, args...)
	out, err := exec.Command("git", full...).CombinedOutput()
	return string(out), err
}

func statusOf(err error) string {
	if err != nil {
		return "fail"
	}
	return "ok"
}

func statusOf2(passed bool) string {
	if passed {
		return "ok"
	}
	return "fail"
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// worktreeDiff returns the diff of the worktree's changes vs its checked-out
// HEAD, INCLUDING newly-created (untracked) files. It stages all worktree
// changes first (`git add -A`) then diffs the staged tree vs HEAD — a plain
// `git diff HEAD` misses untracked new files (e.g. a task that creates a new
// file), which starves verify of any diff (found by M2 bootstrap smoke #1).
// Run inside the worktree (裁决 C). Assumes execute did NOT commit (the execute
// prompt forbids it); loop-eng captures the diff itself. Returns (diff, error):
// a git staging/diffing failure returns a wrapped error so callers surface the
// root cause instead of silently feeding verify an empty diff (#99).
func worktreeDiff(repo, wt string) (string, error) {
	_ = repo
	if _, err := execGit(wt, "add", "-A"); err != nil {
		return "", fmt.Errorf("git add -A in worktree: %w", err)
	}
	out, err := execGit(wt, "--no-pager", "diff", "--cached")
	if err != nil {
		return "", fmt.Errorf("git diff --cached in worktree: %w", err)
	}
	return out, nil
}

// branchName is the worktree branch isolation.Create makes for a given task
// attempt: runID = taskID+"-r"+attempt → branch "loop/"+runID. Mirrored here so
// the done-path commit and the caller's land agree on the name without
// isolation having to expose it.
func branchName(taskID string, attempt int) string {
	return "loop/" + taskID + "-r" + strconv.Itoa(attempt)
}

// commitWorktree captures a done task's execute output by committing all
// worktree changes (incl. untracked — staged first via `git add -A`, same as
// worktreeDiff) on the worktree's branch. The execute prompt forbids the model
// from committing, so loop-eng commits the work itself; the caller then
// FF-merges this branch to main + cleans up. This closes the done-worktree-
// never-landed gap that lost bootstrap work (#12, #14): a Passed verify used to
// leave the worktree uncommitted, so its diff vanished when the worktree went
// stale.
//
// Identity is pinned via `-c` so landing works in any environment (the daemon
// may run where no git identity is configured) and bot-landed commits are
// clearly attributed. An empty diff (execute produced no changes) is a no-op
// success — the branch stays at main HEAD and the caller's land is a clean
// no-op merge + worktree cleanup.
func commitWorktree(wt, branch, message string) error {
	_ = branch
	if _, err := execGit(wt, "add", "-A"); err != nil {
		return fmt.Errorf("git add -A: %w", err)
	}
	out, err := execGit(wt,
		"-c", "user.name=loop-eng",
		"-c", "user.email=loop-eng@local",
		"commit", "-m", message)
	if err != nil {
		// nothing staged → execute made no changes; nothing to land, not an error.
		if strings.Contains(out, "nothing to commit") || strings.Contains(out, "no changes") {
			return nil
		}
		return fmt.Errorf("git commit: %s: %w", out, err)
	}
	return nil
}

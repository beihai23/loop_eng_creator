package loop

import (
	"os/exec"
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
// prompt forbids it); loop-eng captures the diff itself. Empty string on error.
func worktreeDiff(repo, wt string) string {
	_ = repo
	_, _ = execGit(wt, "add", "-A")
	out, err := execGit(wt, "--no-pager", "diff", "--cached")
	if err != nil {
		return ""
	}
	return out
}

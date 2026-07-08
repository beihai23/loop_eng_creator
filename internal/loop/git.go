package loop

import (
	"os/exec"
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

var _ = strings.TrimSpace

// worktreeDiff returns the diff of the worktree's changes vs its checked-out
// HEAD. It MUST run inside the worktree: `git -C <repo> diff HEAD -- <wt>`
// cannot capture the worktree's changes from the main repo (裁决 C). The
// worktree is created from the repo HEAD, so its checked-out HEAD equals the
// repo HEAD; `git -C wt --no-pager diff HEAD` captures exactly the in-worktree
// edits. An empty string is returned on git error.
func worktreeDiff(repo, wt string) string {
	_ = repo
	out, err := execGit(wt, "--no-pager", "diff", "HEAD")
	if err != nil {
		return ""
	}
	return out
}

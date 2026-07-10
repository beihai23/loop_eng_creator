// internal/cli/land.go
package cli

import (
	"fmt"
	"os/exec"

	"loop-eng/internal/isolation"
)

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

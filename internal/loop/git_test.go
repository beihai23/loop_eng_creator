package loop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"loop-eng/internal/isolation"
)

// TestWorktreeDiffIncludesUntracked guards the M2 bootstrap-smoke fix: a
// newly-created (untracked) file in the worktree MUST appear in worktreeDiff.
// A plain `git diff HEAD` misses untracked files, which starved verify of any
// diff for tasks that create new files (smoke issue #1 was blocked this way).
func TestWorktreeDiffIncludesUntracked(t *testing.T) {
	repo := initRepo(t)
	wt, err := isolation.Create(repo, "diff-test")
	if err != nil {
		t.Fatal(err)
	}
	defer isolation.Discard(repo, wt)

	if err := os.WriteFile(filepath.Join(wt, "NEWFILE.md"), []byte("# new\nhello from loop-eng\n"), 0644); err != nil {
		t.Fatal(err)
	}
	diff, err := worktreeDiff(repo, wt)
	if err != nil {
		t.Fatalf("worktreeDiff on a valid worktree must not error: %v", err)
	}
	if !strings.Contains(diff, "NEWFILE.md") || !strings.Contains(diff, "hello from loop-eng") {
		t.Fatalf("worktreeDiff must include the untracked new file; got:\n%s", diff)
	}
}

// TestWorktreeDiffSurfacesGitError pins #99: when git can't stage/diff the
// worktree (here: wt is not a git repo), worktreeDiff returns a wrapped error
// naming the failing git step — NOT a silent empty string that would make verify
// reject with a hidden, wrong "execute made no changes" cause.
func TestWorktreeDiffSurfacesGitError(t *testing.T) {
	notARepo := t.TempDir()
	if err := os.WriteFile(filepath.Join(notARepo, "f.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := worktreeDiff(notARepo, notARepo)
	if err == nil {
		t.Fatal("worktreeDiff on a non-git dir must return an error, not a silent empty diff")
	}
	if !strings.Contains(err.Error(), "git add") {
		t.Fatalf("error must name the failing git step (git add); got: %v", err)
	}
}

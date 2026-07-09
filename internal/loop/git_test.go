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
	diff := worktreeDiff(repo, wt)
	if !strings.Contains(diff, "NEWFILE.md") || !strings.Contains(diff, "hello from loop-eng") {
		t.Fatalf("worktreeDiff must include the untracked new file; got:\n%s", diff)
	}
}

package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// addWorktreeWithCommit creates a worktree at wt on branch, writes new.go into
// it, and commits — mimicking a done task's committed execute output. The repo
// is scaffolded by the shared initGitRepo(t, dir) helper.
func addWorktreeWithCommit(t *testing.T, repo, wt, branch string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(wt), 0755)
	if out, err := exec.Command("git", "-C", repo, "worktree", "add", "-b", branch, wt, "HEAD").CombinedOutput(); err != nil {
		t.Fatalf("worktree add: %s: %v", out, err)
	}
	os.WriteFile(filepath.Join(wt, "new.go"), []byte("package x"), 0644)
	exec.Command("git", "-C", wt, "add", "-A").Run()
	exec.Command("git", "-C", wt, "commit", "-q", "-m", "work").Run()
}

// TestLandFFMergesAndRemovesWorktree: a done task's committed branch FF-merges
// into main and the worktree + branch are cleaned up. The worktree dir's
// basename equals the branch's runID suffix, matching isolation.Create (which
// uses one runID for both the dir name and the branch).
func TestLandFFMergesAndRemovesWorktree(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	wt := filepath.Join(repo, ".loop", "worktrees", "task-x-r1")
	branch := "loop/task-x-r1"
	addWorktreeWithCommit(t, repo, wt, branch)

	if err := land(repo, wt, branch); err != nil {
		t.Fatalf("land: %v", err)
	}
	// main now has the landed file
	if _, err := os.Stat(filepath.Join(repo, "new.go")); err != nil {
		t.Fatalf("main missing landed file: %v", err)
	}
	// worktree removed
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree should be removed, still exists")
	}
	// branch deleted
	if out, err := exec.Command("git", "-C", repo, "rev-parse", "--verify", branch).CombinedOutput(); err == nil {
		t.Fatalf("branch should be deleted, still exists: %s", out)
	}
}

// TestLandLeavesWorktreeWhenNotFF: if main advanced (non-FF), land must error
// and leave the worktree + branch intact — the done work is never lost.
func TestLandLeavesWorktreeWhenNotFF(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	wt := filepath.Join(repo, ".loop", "worktrees", "task-y-r1")
	branch := "loop/task-y-r1"
	addWorktreeWithCommit(t, repo, wt, branch)
	// advance main after the branch was created → FF can no longer apply
	os.WriteFile(filepath.Join(repo, "main.go"), []byte("package m"), 0644)
	exec.Command("git", "-C", repo, "add", "-A").Run()
	exec.Command("git", "-C", repo, "commit", "-q", "-m", "main moved").Run()

	if err := land(repo, wt, branch); err == nil {
		t.Fatal("land should fail on non-FF, got nil")
	}
	// worktree + branch preserved (work safe)
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("worktree must be preserved on non-FF: %v", err)
	}
	if out, err := exec.Command("git", "-C", repo, "rev-parse", "--verify", branch).CombinedOutput(); err != nil {
		t.Fatalf("branch must be preserved on non-FF: %v (%s)", err, out)
	}
}

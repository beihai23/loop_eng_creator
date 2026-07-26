package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"loop-eng/internal/channel"
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

// recordingChan records CloseIssue and PostComment calls — the two channel
// side-effects finalizeLand's close-vs-defer decision drives. It is the test
// seam for the three finalizeLand paths (Local is a no-op CloseIssue, so it
// can't prove the issue was closed).
type recordingChan struct {
	closed   []string
	comments []string
}

func (r *recordingChan) ListNewTasks(context.Context) ([]channel.Task, error) { return nil, nil }
func (r *recordingChan) ListReplies(context.Context, []string, time.Time) (map[string][]channel.Reply, error) {
	return nil, nil
}
func (r *recordingChan) PostComment(_ context.Context, _, body string) error {
	r.comments = append(r.comments, body)
	return nil
}
func (r *recordingChan) UpdateStatus(context.Context, string, string) error { return nil }
func (r *recordingChan) CloseIssue(_ context.Context, ref string) error {
	r.closed = append(r.closed, ref)
	return nil
}
func (r *recordingChan) GetTaskStates(context.Context, []string) (map[string]channel.TaskState, error) {
	return nil, nil
}

// TestFinalizeLandLocalFFMergeClosesIssue: 非 push 的 gh 侧失败 → handlePRFailure
// 走本地 FF-merge 兜底，成功 → CloseIssue 被调 + Integrated=true。
// 这是 DoD 的「本地直落路径：done 后 issue 照旧关闭」回归。
func TestFinalizeLandLocalFFMergeClosesIssue(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	wt := filepath.Join(repo, ".loop", "worktrees", "task-fl-r1")
	branch := "loop/task-fl-r1"
	addWorktreeWithCommit(t, repo, wt, branch)

	ch := &recordingChan{}
	// 非 push 失败（gh pr create 挂）：触发本地 FF-merge 兜底。
	res := finalizeLand(context.Background(), ch, "42", repo, wt, branch, "", fmt.Errorf("gh pr create: boom"), func(string, ...any) {})

	if !res.Integrated {
		t.Fatalf("local FF-merge success: Integrated=true, got false (Note=%q Branch=%q)", res.Note, res.Branch)
	}
	if res.Branch != "" {
		t.Fatalf("integrated → no pending branch, got %q", res.Branch)
	}
	if len(ch.closed) != 1 || ch.closed[0] != "42" {
		t.Fatalf("local FF-merge success must close issue #42, got %v", ch.closed)
	}
	// branch 被 land() FF-merge 进 main + worktree 清理。
	if _, err := os.Stat(filepath.Join(repo, "new.go")); err != nil {
		t.Fatalf("main missing landed file after FF-merge: %v", err)
	}
}

// TestFinalizeLandPRSuccessStaysOpen: createPR 成功（prErr=nil）→ issue 留开，
// 发「待合并」评论（含 URL），返回 Branch，Integrated=false，不调 CloseIssue。
// 这是 DoD 的「PR 路径：done 后 issue 保持 OPEN，done 战报含待合并注记」。
func TestFinalizeLandPRSuccessStaysOpen(t *testing.T) {
	ch := &recordingChan{}
	const prURL = "https://github.com/o/r/pull/99"
	// PR 成功路径不碰 git（不调 createPR/land），repo/wt 仅占位。
	res := finalizeLand(context.Background(), ch, "42", "/unused/repo", "/unused/wt", "loop/task-42-r1", prURL, nil, func(string, ...any) {})

	if res.Integrated {
		t.Fatalf("PR success must NOT be Integrated (issue stays open)")
	}
	if res.Branch != "loop/task-42-r1" {
		t.Fatalf("PR success must return the pending Branch, got %q", res.Branch)
	}
	if len(ch.closed) != 0 {
		t.Fatalf("PR success must NOT close the issue, got %v", ch.closed)
	}
	if len(ch.comments) != 1 || !strings.Contains(ch.comments[0], "待合并") || !strings.Contains(ch.comments[0], prURL) {
		t.Fatalf("PR success must post a 待合并 comment with the URL, got %+v", ch.comments)
	}
	if !strings.Contains(res.Note, "待合并") || !strings.Contains(res.Note, prURL) {
		t.Fatalf("Note must carry the 待合并 marker + URL for the done detail, got %q", res.Note)
	}
}

// TestFinalizeLandPushFailedPartialStaysOpen: push 最终失败（ErrPushFailed）→
// LAND PARTIAL：issue 留开，branch 保留，返回 Branch，Integrated=false，不调 CloseIssue。
func TestFinalizeLandPushFailedPartialStaysOpen(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	wt := filepath.Join(repo, ".loop", "worktrees", "task-pf-r1")
	branch := "loop/task-pf-r1"
	addWorktreeWithCommit(t, repo, wt, branch)

	ch := &recordingChan{}
	pushErr := fmt.Errorf("%w: %s", ErrPushFailed, "Connection closed by 198.18.0.68 port 22")
	res := finalizeLand(context.Background(), ch, "42", repo, wt, branch, "", pushErr, func(string, ...any) {})

	if res.Integrated {
		t.Fatalf("LAND PARTIAL must NOT be Integrated (issue stays open)")
	}
	if res.Branch != branch {
		t.Fatalf("LAND PARTIAL must return the preserved Branch, got %q", res.Branch)
	}
	if len(ch.closed) != 0 {
		t.Fatalf("LAND PARTIAL must NOT close the issue, got %v", ch.closed)
	}
	if !strings.Contains(res.Note, "LAND PARTIAL") {
		t.Fatalf("Note must carry the LAND PARTIAL marker, got %q", res.Note)
	}
	// branch 保留（landFallback 不 FF-merge、不 Discard）。
	if out, err := exec.Command("git", "-C", repo, "rev-parse", "--verify", branch).CombinedOutput(); err != nil {
		t.Fatalf("branch must be preserved on LAND PARTIAL: %v (%s)", err, out)
	}
}

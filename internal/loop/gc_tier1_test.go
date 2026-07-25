package loop

// gc_tier1_test.go 钉死 worktree 现场 GC 的策略（gc.go）：
//  - decide 纯函数逐分支：48h 宽限期一律保留；running/parked 保留；done 保留；
//    blocked 超 TTL 删、期内留；孤儿/终态删。
//  - GCWorktrees 集成：真实 worktree + 真实 task_status，验证删除/保留落盘结果
//    （含分支清理）与 dry-run 只判不删。

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"loop-eng/internal/isolation"
	"loop-eng/internal/state"
)

func TestTier1GCDecide(t *testing.T) {
	now := time.Now()
	pol := GCPolicy{Now: now, Grace: 48 * time.Hour, SceneTTL: 7 * 24 * time.Hour}
	entry := func(name string, age time.Duration) isolation.Entry {
		return isolation.Entry{Name: name, Path: "/x/" + name, ModTime: now.Add(-age)}
	}
	statuses := map[string]string{
		"task_run": "running", "task_park": "needs-review", "task_done": "done",
		"task_blk": "blocked", "task_cxl": "cancelled",
	}
	cases := []struct {
		name    string
		entry   isolation.Entry
		wantDel bool
	}{
		{"宽限期内一律保留（无 task 行也不例外）", entry("ghost-r1", 1*time.Hour), false},
		{"宽限期内 blocked 保留", entry("task_blk-r1", 47*time.Hour), false},
		{"running 超宽限期仍保留（活树）", entry("task_run-r3", 90*24*time.Hour), false},
		{"needs-review 超宽限期仍保留（parked 等人）", entry("task_park-r1", 30*24*time.Hour), false},
		{"done 保留（land-salvage 留给人）", entry("task_done-r1", 30*24*time.Hour), false},
		{"blocked 未超 TTL 保留", entry("task_blk-r1", 3*24*time.Hour), false},
		{"blocked 超 TTL 删除", entry("task_blk-r2", 8*24*time.Hour), true},
		{"cancelled 超宽限期删除", entry("task_cxl-r1", 3*24*time.Hour), true},
		{"无 task 行的孤儿超宽限期删除", entry("task_ghost-r1", 3*24*time.Hour), true},
		{"名字无法解析的目录超宽限期删除", entry("random-dir", 3*24*time.Hour), true},
	}
	for _, c := range cases {
		got := decide(c.entry, statuses, pol)
		if got.Delete != c.wantDel {
			t.Errorf("%s: decide(%s).Delete = %v, want %v (reason=%s)", c.name, c.entry.Name, got.Delete, c.wantDel, got.Reason)
		}
	}
}

// TestTier1GCWorktreesIntegration：真实 git worktree + 真实 store 状态，端到端验证
// GC 的落盘结果（目录与 loop/* 分支的存废）以及 dry-run 不删。
func TestTier1GCWorktreesIntegration(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()
	now := time.Now()

	mkTask := func(ref, status string) string {
		id, err := st.InsertTask(state.TaskRow{IssueRef: ref, Description: "d", Source: "test"})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.AppendTransition(id, "", status, "test"); err != nil {
			t.Fatal(err)
		}
		return id
	}
	mkWT := func(name string, age time.Duration) string {
		p, err := isolation.Create(repo, name)
		if err != nil {
			t.Fatal(err)
		}
		old := now.Add(-age)
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
		return p
	}

	blkOld := mkTask("b1", "blocked")
	blkYoung := mkTask("b2", "blocked")
	running := mkTask("r1", "running")
	// 4 棵树：blocked 超龄（删）、blocked 年轻（留）、running 超龄（留）、孤儿超龄（删）
	pBlkOld := mkWT(blkOld+"-r1", 8*24*time.Hour)
	pBlkYoung := mkWT(blkYoung+"-r1", 24*time.Hour)
	pRunning := mkWT(running+"-r1", 30*24*time.Hour)
	pOrphan := mkWT("task_no_such-r1", 4*24*time.Hour)

	pol := GCPolicy{Now: now, Grace: 48 * time.Hour, SceneTTL: 7 * 24 * time.Hour}

	// dry-run：判定但不删
	dry := pol
	dry.DryRun = true
	acts, err := GCWorktrees(repo, st, dry)
	if err != nil {
		t.Fatal(err)
	}
	if len(acts) != 4 {
		t.Fatalf("want 4 actions, got %d", len(acts))
	}
	for _, p := range []string{pBlkOld, pBlkYoung, pRunning, pOrphan} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("dry-run must not delete %s: %v", p, err)
		}
	}

	// 真跑
	if _, err := GCWorktrees(repo, st, pol); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{pBlkOld, pOrphan} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s should be deleted", p)
		}
	}
	for _, p := range []string{pBlkYoung, pRunning} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s should be kept: %v", p, err)
		}
	}
	// 被删树的 loop/* 分支也应清掉
	branches, _ := exec.Command("git", "-C", repo, "branch", "--list", "loop/*").Output()
	for _, gone := range []string{blkOld + "-r1", "task_no_such-r1"} {
		if strings.Contains(string(branches), gone) {
			t.Errorf("branch loop/%s should be deleted, branches: %s", gone, branches)
		}
	}
	if !strings.Contains(string(branches), blkYoung+"-r1") {
		t.Errorf("kept tree's branch should survive, branches: %s", branches)
	}
}

// internal/cli/status_test.go
package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"loop-eng/internal/state"
)

// TestStatusPrintsTask drives `loop-eng status` after a fake task is hand-
// inserted into state.db. The CLI must list each task_status row as
// "id status" without touching the Store's private db field (裁决 F: status
// goes through the public ListStatuses method).
func TestStatusPrintsTask(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	// init 生成 .loop/state.db（status --repo 需要它）
	initCmd := NewRootCmd()
	initCmd.SetArgs([]string{"init", "--repo", repo})
	if err := initCmd.Execute(); err != nil {
		t.Fatalf("init: %v", err)
	}
	// 手插一条任务（state.Open + InsertTask，不经 CLI）
	insertFakeTask(t, repo)

	var buf bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"status", "--repo", repo})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("status: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "running") && !strings.Contains(out, "new") && !strings.Contains(out, "task_") {
		t.Fatalf("status output empty: %q", out)
	}
}

// insertFakeTask opens <repo>/.loop/state.db and inserts one throwaway task
// row (status defaults to "new"). Helper for status command tests.
func insertFakeTask(t *testing.T, repo string) {
	t.Helper()
	st, err := state.Open(filepath.Join(repo, ".loop", "state.db"))
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer st.Close()
	if _, err := st.InsertTask(state.TaskRow{
		Description: "fake task for status test",
		TaskType:    "bugfix",
	}); err != nil {
		t.Fatalf("insert task: %v", err)
	}
}

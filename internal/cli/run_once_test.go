// internal/cli/run_once_test.go
package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRunOnceEndToEnd drives the M1 synchronous entry point end-to-end:
// init a repo → drop an inbox task → run-once --models fake → assert the
// terminal battle report landed in outbox (裁决 B1: every terminal outcome
// writes a channel report).
func TestRunOnceEndToEnd(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	// 先 init：生成 .loop/config.yaml + state.db + skills
	initCmd := NewRootCmd()
	initCmd.SetArgs([]string{"init", "--repo", repo})
	if err := initCmd.Execute(); err != nil {
		t.Fatalf("init: %v", err)
	}

	// 准备 inbox 任务
	inbox := filepath.Join(repo, "inbox")
	if err := os.MkdirAll(inbox, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inbox, "1.md"),
		[]byte("# 任务\ndo thing\ntype: bugfix\n## 验收标准\n- [ ] c"), 0644); err != nil {
		t.Fatal(err)
	}

	// run-once（用 --models=fake 注入 Fake，便于 CI）
	cmd := NewRootCmd()
	cmd.SetArgs([]string{"run-once", "--repo", repo, "--task-inbox", inbox, "--models", "fake"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	// 战报应写到 outbox（SubLoop.report → channel.PostComment → outbox/<ref>.md）
	if _, err := os.Stat(filepath.Join(repo, "outbox", "1.md")); err != nil {
		t.Fatalf("no battle report: %v", err)
	}
}

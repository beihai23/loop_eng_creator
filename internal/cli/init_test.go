// internal/cli/init_test.go
package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitScaffoldsLoopDir(t *testing.T) {
	repo := t.TempDir()
	// 需要 git 仓库（worktree 基址）
	initGitRepo(t, repo)

	cmd := NewRootCmd()
	cmd.SetArgs([]string{"init", "--repo", repo})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		".loop/config.yaml",
		".loop/state.db",
		".loop/worktrees",
		".loop/skills/triage.md",
		".loop/skills/plan.md",
		".loop/skills/verify.md",
		".loop/skills/help.md",
	} {
		if _, err := os.Stat(filepath.Join(repo, p)); err != nil {
			t.Fatalf("missing %s: %v", p, err)
		}
	}

	// 裁决 H：init 必须把 .loop/ 追加进仓库的 .gitignore（worktrees 在其下）。
	gi, err := os.ReadFile(filepath.Join(repo, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	if !strings.Contains(string(gi), ".loop/") {
		t.Fatalf(".gitignore missing .loop/ entry; got:\n%s", gi)
	}
}

func TestInitGitignoreIdempotent(t *testing.T) {
	// 已存在的 .gitignore 不应重复追加 .loop/。
	repo := t.TempDir()
	initGitRepo(t, repo)
	existing := []byte("vendor/\n.loop/\n")
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), existing, 0644); err != nil {
		t.Fatal(err)
	}
	cmd := NewRootCmd()
	cmd.SetArgs([]string{"init", "--repo", repo})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	gi, _ := os.ReadFile(filepath.Join(repo, ".gitignore"))
	if got := strings.Count(string(gi), ".loop/"); got != 1 {
		t.Fatalf("expected exactly 1 .loop/ in .gitignore, got %d:\n%s", got, gi)
	}
}

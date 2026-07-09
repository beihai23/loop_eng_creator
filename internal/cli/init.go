// internal/cli/init.go
package cli

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"loop-eng/internal/state"
)

//go:embed embed/skills/*.md
var skillFiles embed.FS

// defaultConfig is the all-claude-p baseline (triage/plan/execute/verify all
// via `claude -p`); spec §8.1 / 裁决 I. Users override per-role later in
// .loop/config.yaml.
var defaultConfig = `
models:
  triage:  { via: claude-p, binary: claude }
  plan:    { via: claude-p, binary: claude }
  execute: { via: claude-p, binary: claude, cmd: ["--dangerously-skip-permissions"] }
  verify:  { via: claude-p, binary: claude }
budget:
  per_call_tokens: 20000
  per_task_tokens: 200000
  max_retries: 3
verify:
  deterministic:
    - { label: go-test, cmd: ["go", "test", "./..."] }
  tier3_human: true
isolation: { worktree: true }
skills: { dir: .loop/skills }
channel: { provider: local }
`

// gitignoreMarker is the line init ensures is present in the repo's .gitignore
// so that .loop/worktrees/ and friends don't pollute git status (裁决 H).
const gitignoreMarker = ".loop/"

// NewInitCmd builds `loop-eng init`: scaffolds <repo>/.loop/ (config.yaml +
// copied default skills + state.db + worktrees/) and appends `.loop/` to the
// repo's .gitignore when absent.
func NewInitCmd() *cobra.Command {
	var repo string
	cmd := &cobra.Command{
		Use:   "init",
		Short: "在当前仓库生成 .loop/（配置 + skill + state.db）",
		RunE: func(cmd *cobra.Command, args []string) error {
			if repo == "" {
				repo, _ = os.Getwd()
			}
			loopDir := filepath.Join(repo, ".loop")
			for _, sub := range []string{"skills", "worktrees"} {
				if err := os.MkdirAll(filepath.Join(loopDir, sub), 0755); err != nil {
					return err
				}
			}
			if err := os.WriteFile(filepath.Join(loopDir, "config.yaml"), []byte(defaultConfig), 0644); err != nil {
				return err
			}
			entries, err := skillFiles.ReadDir("embed/skills")
			if err != nil {
				return fmt.Errorf("read embedded skills: %w", err)
			}
			for _, e := range entries {
				raw, err := skillFiles.ReadFile("embed/skills/" + e.Name())
				if err != nil {
					return fmt.Errorf("read embedded skill %s: %w", e.Name(), err)
				}
				if err := os.WriteFile(filepath.Join(loopDir, "skills", e.Name()), raw, 0644); err != nil {
					return err
				}
			}
			st, err := state.Open(filepath.Join(loopDir, "state.db"))
			if err != nil {
				return err
			}
			if err := st.Close(); err != nil {
				return err
			}
			if err := ensureGitignore(repo); err != nil {
				return err
			}
			fmt.Println("loop-eng initialized at", loopDir)
			return nil
		},
	}
	cmd.Flags().StringVar(&repo, "repo", "", "目标仓库路径（默认当前目录）")
	return cmd
}

// ensureGitignore appends `.loop/` to <repo>/.gitignore, creating the file if
// missing and skipping the append when the marker is already present (裁决 H).
func ensureGitignore(repo string) error {
	p := filepath.Join(repo, ".gitignore")
	cur, _ := os.ReadFile(p)
	for _, line := range strings.Split(string(cur), "\n") {
		if strings.TrimSpace(line) == gitignoreMarker {
			return nil
		}
	}
	body := gitignoreMarker + "\n"
	if len(cur) > 0 && !strings.HasSuffix(string(cur), "\n") {
		body = "\n" + body
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString(body); err != nil {
		return err
	}
	return nil
}

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

// defaultConfig: every role pinned to `provider: claude` (the coding-agent
// shell-out target — out-of-box `claude -p`, the same path as before #68), NO
// model name pinned (model-agnostic; no SDK, no API key, no alias drift). The
// legacy `via: claude-p` field is kept for backward compatibility (config.Load
// still parses it) but is no longer read by NewAgent — `provider` is the source
// of truth. There is NO static tier-1 script list — tier-1 acceptance scripts
// are produced per-task by the planner and run in the worktree. Per-role model
// opt-in: set `name` in .loop/config.yaml if a role needs a different model.
//
// Read-only defense-in-depth (#84): triage/plan/verify append
// `--disallowedTools Edit Write NotebookEdit` so the write tools are physically
// removed from the agent's context — their "read-only" is enforced by the
// permission layer, not just by prompt self-discipline. `--dangerously-skip-
// permissions` is KEPT on all roles (headless read-only Bash — grep/go doc/git
// ls-files — needs it to run); deny rules take precedence over bypassPermissions,
// so the two coexist and deny wins. execute is unchanged: it needs to write.
// Bash remains a potential write channel; worktree isolation (#68 P0) contains
// in-repo writes, and out-of-repo Bash actions are accepted residual risk.
var defaultConfig = `
models:
  triage:  { provider: claude, via: claude-p, binary: claude, cmd: ["--dangerously-skip-permissions", "--disallowedTools", "Edit", "Write", "NotebookEdit"] }
  plan:    { provider: claude, via: claude-p, binary: claude, cmd: ["--dangerously-skip-permissions", "--disallowedTools", "Edit", "Write", "NotebookEdit"] }
  execute: { provider: claude, via: claude-p, binary: claude, cmd: ["--dangerously-skip-permissions"] }
  verify:  { provider: claude, via: claude-p, binary: claude, cmd: ["--dangerously-skip-permissions", "--disallowedTools", "Edit", "Write", "NotebookEdit"] }
budget:
  per_call_tokens: 100000
  per_task_tokens: 1000000
  max_retries: 3
verify:
  tier3_human: true
isolation: { worktree: true }
skills: { dir: .loop/skills }
channel: { provider: local }
daemon: { poll_interval: 60s }
`

// gitignoreMarker is the line init ensures is present in the repo's .gitignore
// so that .loop/worktrees/ and friends don't pollute git status (裁决 H).
const gitignoreMarker = ".loop/"

// NewInitCmd builds `loop-eng init`: a backward-compatible alias of
// `loop-eng config` (issue: config 合并 init — 一条命令搞定脚手架 + 配置).
// It prints a deprecation hint, then runs the exact same path as config:
// scaffold .loop/ when missing, then the interactive provider walkthrough.
// Non-interactive callers (scripts/CI with stdin at EOF) get EOF at the
// provider prompt → "keep current config" → scaffold-only, exit 0 — i.e. the
// old init behavior plus one deprecation line. Do NOT delete this command:
// README/docs/scripts still reference it.
func NewInitCmd() *cobra.Command {
	var repo string
	cmd := &cobra.Command{
		Use:   "init",
		Short: "（deprecated，请改用 config）在当前仓库生成 .loop/ 并交互式配置",
		RunE: func(cmd *cobra.Command, args []string) error {
			if repo == "" {
				repo, _ = os.Getwd()
			}
			fmt.Fprintln(cmd.ErrOrStderr(), "注意：'loop-eng init' 已并入 'loop-eng config'；init 为 deprecated alias，请改用 config")
			return runConfigInteractive(cmd.InOrStdin(), cmd.OutOrStdout(), repo)
		},
	}
	cmd.Flags().StringVar(&repo, "repo", "", "目标仓库路径（默认当前目录）")
	return cmd
}

// scaffoldLoop scaffolds <repo>/.loop/ (default config.yaml + copied embedded
// skills + state.db + worktrees/) and appends `.loop/` to the repo's
// .gitignore when absent (裁决 H). The success banner is printed by callers
// (runConfigInteractive), not here.
func scaffoldLoop(repo string) error {
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
	return ensureGitignore(repo)
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

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
// Airtight read-only roles (#84 → #airtight): triage/plan/verify carry
// `readonly: true` plus `--disallowedTools Edit Write NotebookEdit`. The claude
// provider factory turns `readonly: true` into its read-only profile
// (model.claudeReadOnlyProfile): it strips any bypass and injects
// `--permission-mode plan`. plan mode blocks EVERY write at the permission layer
// — including Bash writes both inside (`echo > file`, `sed -i`) and OUTSIDE the
// repo (an absolute-path `echo > /tmp/x` is rejected and the file is not created;
// verified against real claude). Read-only Bash still runs normally (grep / go
// doc / git ls-files), so exploration is unaffected.
//
// `--dangerously-skip-permissions` (bypassPermissions) is therefore REMOVED from
// the read-only roles: it is mutually exclusive with plan mode, and only one can
// win — read-only must win. execute KEEPS bypass and sets no `readonly` (it must
// write), so execute is entirely unaffected by this change.
//
// Defense-in-depth, not replacement: the worktree (#68, option 2 — already in
// place) remains plan/verify's exploration context and a second containment
// layer — any write that somehow slipped plan mode would land on the disposable
// tree, not the main repo. The two layers are independent: plan mode makes the
// write impossible; the worktree makes its consequence disposable.
var defaultConfig = `
models:
  triage:  { provider: claude, via: claude-p, binary: claude, cmd: ["--disallowedTools", "Edit", "Write", "NotebookEdit"], readonly: true }
  plan:    { provider: claude, via: claude-p, binary: claude, cmd: ["--disallowedTools", "Edit", "Write", "NotebookEdit"], readonly: true }
  execute: { provider: claude, via: claude-p, binary: claude, cmd: ["--dangerously-skip-permissions"] }
  verify:  { provider: claude, via: claude-p, binary: claude, cmd: ["--disallowedTools", "Edit", "Write", "NotebookEdit"], readonly: true }
budget:
  per_call_tokens: 200000
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

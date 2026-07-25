// internal/cli/config.go
package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"loop-eng/internal/config"
	"loop-eng/internal/model"
)

// NewConfigCmd builds `loop-eng config`: the primary setup command — scaffolds
// .loop/ when missing (same as the old init), then interactively walks the
// user through picking a channel provider and that provider's required
// fields, and writes the result back to .loop/config.yaml. No new deps: all
// interaction is plain bufio line reads (number or name to pick, string to
// fill a field), so it stays scriptable via piped stdin.
func NewConfigCmd() *cobra.Command {
	var repo string
	cmd := &cobra.Command{
		Use:   "config",
		Short: "交互式配置 loop-eng（脚手架 + channel provider 引导）",
		RunE: func(cmd *cobra.Command, args []string) error {
			if repo == "" {
				repo, _ = os.Getwd()
			}
			return runConfigInteractive(cmd.InOrStdin(), cmd.OutOrStdout(), repo)
		},
	}
	cmd.Flags().StringVar(&repo, "repo", "", "目标仓库路径（默认当前目录）")
	return cmd
}

// runConfigInteractive drives the whole setup flow against injectable I/O:
//  1. scaffold .loop/ when missing
//  2. pick provider (local/github/linear); empty input / EOF keeps the
//     current config untouched (this is what makes `init` non-interactive-safe)
//  3. per-provider prompts (see below), then config.Save
//
// A single bufio.Reader wraps `in` and is threaded through every prompt —
// constructing a fresh reader per prompt would buffer-swallow the rest of
// stdin.
func runConfigInteractive(in io.Reader, out io.Writer, repo string) error {
	loopDir := filepath.Join(repo, ".loop")
	if _, err := os.Stat(loopDir); os.IsNotExist(err) {
		fmt.Fprintf(out, "未发现 %s，先生成脚手架（config.yaml + skills + state.db + worktrees）...\n", loopDir)
		if err := scaffoldLoop(repo); err != nil {
			return err
		}
		fmt.Fprintln(out, "loop-eng initialized at", loopDir)
	}
	cfgPath := filepath.Join(loopDir, "config.yaml")
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}

	r := bufio.NewReader(in)
	provider, err := pickProvider(r, out)
	if err != nil {
		return err
	}
	if provider == "" {
		fmt.Fprintln(out, "保持当前配置，未做修改")
		return nil
	}
	cfg.Channel.Provider = provider

	switch provider {
	case "github":
		// gh 检测只影响提示、不阻塞、不消费 stdin。
		if !ghAvailable() {
			fmt.Fprint(out, ghInstallInstructions(runtime.GOOS))
		} else if !ghAuthed() {
			fmt.Fprintln(out, "检测到 gh 未登录，请先执行: gh auth login")
		}
		repoDef := cfg.Channel.Repo
		repoName, err := promptLine(r, out, "GitHub repo (owner/name)", repoDef)
		if err != nil {
			return err
		}
		labelDef := cfg.Channel.TaskLabel
		if labelDef == "" {
			labelDef = "loop:task"
		}
		label, err := promptLine(r, out, "task_label", labelDef)
		if err != nil {
			return err
		}
		if repoName == "" {
			return fmt.Errorf("github provider 需要 repo（owner/name）")
		}
		cfg.Channel.Repo = repoName
		cfg.Channel.TaskLabel = label

	case "linear":
		choice, err := promptLine(r, out, "API key 存放方式 (1=env 指令, 2=写 .loop/linear.key)", "1")
		if err != nil {
			return err
		}
		key, err := promptLine(r, out, "Linear API key（Linear → Settings → Security & access → Personal API keys 生成）", "")
		if err != nil {
			return err
		}
		projDef, teamDef := "", ""
		if cfg.Channel.Linear != nil {
			projDef = cfg.Channel.Linear.Project
			teamDef = cfg.Channel.Linear.Team
		}
		project, err := promptLine(r, out, "Linear project (name 或 uuid)", projDef)
		if err != nil {
			return err
		}
		team, err := promptLine(r, out, "Linear team key（可空）", teamDef)
		if err != nil {
			return err
		}
		statusMapIn, err := promptLine(r, out, "status_map（type=state 逗号串，空=默认）", "")
		if err != nil {
			return err
		}
		// key 永不进 config.yaml（#24 决定 A）：选 1 打印 env 指令；选 2 写
		// gitignore 内的 .loop/linear.key（0600）并提示 source。
		if choice == "2" {
			if key == "" {
				fmt.Fprintln(out, "未输入 API key，跳过写 .loop/linear.key")
			} else {
				keyPath := filepath.Join(loopDir, "linear.key")
				if err := os.WriteFile(keyPath, []byte(key+"\n"), 0600); err != nil {
					return err
				}
				fmt.Fprintf(out, "已写入 %s（0600；.loop/ 整体已在 .gitignore）。使用前请：\n  export LOOP_ENG_LINEAR_API_KEY=$(cat %s)\n", keyPath, keyPath)
			}
		} else {
			fmt.Fprintf(out, "key 不进 config.yaml。请把下面这行加进 shell rc（~/.zshrc 等）：\n  export LOOP_ENG_LINEAR_API_KEY=%s\n", key)
		}
		if project == "" {
			return fmt.Errorf("linear provider 需要 project（name 或 uuid）")
		}
		statusMap := defaultLinearStatusMap()
		if parsed := parseStatusMap(statusMapIn); len(parsed) > 0 {
			statusMap = parsed
		}
		cfg.Channel.Linear = &config.LinearChannel{Project: project, Team: team, StatusMap: statusMap}

	case "local":
		inboxDef := cfg.Channel.Inbox
		if inboxDef == "" {
			inboxDef = "inbox"
		}
		inbox, err := promptLine(r, out, "inbox 路径", inboxDef)
		if err != nil {
			return err
		}
		cfg.Channel.Inbox = inbox
	}

	// coding-agent provider 步：channel 配好后、Save 前引导配置
	// models.<role>.provider（triage/plan/execute/verify 的 shell-out 目标）。该步
	// 复用 pickProvider 的 non-interactive-safe 语义——scope 行空/EOF（含 piped
	// stdin 在 channel 步用尽后的尾部 EOF）→ 保持现状、继续 Save，故现有 piped
	// stdin 脚本零回归。channel provider 为空时已在上方提前 return，根本不触达这里。
	scope, err := promptLine(r, out, "coding-agent provider 配置方式 (1=全局, 2=逐角色)", "")
	if err != nil {
		return err
	}
	roles := []string{"triage", "plan", "execute", "verify"}
	switch scope {
	case "1":
		// 全局一把梭：选一个 provider 写进四个角色。
		prov, err := pickAgentProvider(r, out)
		if err != nil {
			return err
		}
		if prov != "" {
			for _, role := range roles {
				setRoleProvider(cfg, role, prov)
			}
		}
	case "2":
		// 逐角色：每个角色独立选，空=保持该角色现状。
		for _, role := range roles {
			fmt.Fprintf(out, "当前 %s provider: %s\n", role, roleProvider(cfg, role))
			prov, err := pickAgentProvider(r, out)
			if err != nil {
				return err
			}
			if prov != "" {
				setRoleProvider(cfg, role, prov)
			}
		}
	}

	if err := config.Save(cfgPath, cfg); err != nil {
		return err
	}
	fmt.Fprintf(out, "配置已写入 %s（provider=%s）\n", cfgPath, provider)
	return nil
}

// pickProvider prints the provider menu and reads one line. Accepts
// "1"/"local", "2"/"github", "3"/"linear" (case-insensitive, trimmed). Empty
// line or EOF returns ("", nil) — the caller treats that as "keep current
// config". Invalid input reprints the menu and reads again.
func pickProvider(r *bufio.Reader, out io.Writer) (string, error) {
	for {
		fmt.Fprintln(out, "选择 channel provider:")
		fmt.Fprintln(out, "  1) local   — 本地 inbox/ 目录，零外部依赖")
		fmt.Fprintln(out, "  2) github  — GitHub Issues（经 gh CLI）")
		fmt.Fprintln(out, "  3) linear  — Linear（GraphQL API）")
		fmt.Fprint(out, "> ")
		line, err := r.ReadString('\n')
		if err != nil && err != io.EOF {
			return "", err
		}
		s := strings.ToLower(strings.TrimSpace(line))
		if s == "" {
			return "", nil
		}
		switch s {
		case "1", "local":
			return "local", nil
		case "2", "github":
			return "github", nil
		case "3", "linear":
			return "linear", nil
		}
		fmt.Fprintf(out, "无法识别的输入 %q，请重新选择\n", s)
	}
}

// pickAgentProvider prints the coding-agent provider menu (drawn dynamically
// from the model registry) and reads one line. Accepts a 1-based number OR the
// provider name, case-insensitively and trimmed. Empty line or EOF returns
// ("", nil) — the caller treats that as "keep current provider" (the same
// non-interactive-safe contract as pickProvider, so piped stdin stays
// scriptable and empty input is a no-op). Invalid input reprints the menu and
// reads again. The menu is built over model.RegisteredProviders(), so a newly
// registered provider appears here automatically — adding a provider is a
// one-line registry edit (single source of truth).
func pickAgentProvider(r *bufio.Reader, out io.Writer) (string, error) {
	names := model.RegisteredProviders()
	for {
		fmt.Fprintln(out, "选择 coding-agent provider:")
		for i, n := range names {
			fmt.Fprintf(out, "  %d) %s\n", i+1, n)
		}
		fmt.Fprint(out, "> ")
		line, err := r.ReadString('\n')
		if err != nil && err != io.EOF {
			return "", err
		}
		s := strings.ToLower(strings.TrimSpace(line))
		if s == "" {
			return "", nil
		}
		// number match (1-based index into the sorted registry)
		if num, err := strconv.Atoi(s); err == nil && num >= 1 && num <= len(names) {
			return names[num-1], nil
		}
		// name match (case-insensitive — return the registry's canonical casing)
		for _, n := range names {
			if s == strings.ToLower(n) {
				return n, nil
			}
		}
		fmt.Fprintf(out, "无法识别的输入 %q，请重新选择\n", s)
	}
}

// setRoleProvider writes provider into the Models field for role (one of
// triage/plan/execute/verify). Unknown roles are a no-op — the only callers
// iterate the fixed four-role list, so this never receives anything else.
func setRoleProvider(cfg *config.Config, role, provider string) {
	switch role {
	case "triage":
		cfg.Models.Triage.Provider = provider
	case "plan":
		cfg.Models.Plan.Provider = provider
	case "execute":
		cfg.Models.Execute.Provider = provider
	case "verify":
		cfg.Models.Verify.Provider = provider
	}
}

// roleProvider returns the current provider for role (one of
// triage/plan/execute/verify); "" for an unknown role. The read counterpart to
// setRoleProvider, used to show "当前 <role> provider: ..." context in the
// per-role walkthrough.
func roleProvider(cfg *config.Config, role string) string {
	switch role {
	case "triage":
		return cfg.Models.Triage.Provider
	case "plan":
		return cfg.Models.Plan.Provider
	case "execute":
		return cfg.Models.Execute.Provider
	case "verify":
		return cfg.Models.Verify.Provider
	}
	return ""
}

// promptLine prints "<label> [<def>]: " (or "<label>: " when def is empty),
// reads one line, and returns the trimmed input — or def on an empty line or
// EOF (including a partial-but-empty line at EOF).
func promptLine(r *bufio.Reader, out io.Writer, label, def string) (string, error) {
	if def != "" {
		fmt.Fprintf(out, "%s [%s]: ", label, def)
	} else {
		fmt.Fprintf(out, "%s: ", label)
	}
	line, err := r.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	s := strings.TrimSpace(line)
	if s == "" {
		return def, nil
	}
	return s, nil
}

// ghAvailable reports whether the gh CLI is on PATH.
func ghAvailable() bool {
	_, err := exec.LookPath("gh")
	return err == nil
}

// ghAuthed runs `gh auth status`; call only when ghAvailable.
func ghAuthed() bool {
	return exec.Command("gh", "auth", "status").Run() == nil
}

// ghInstallInstructions returns the gh-CLI install blurb for the given GOOS:
// the official link plus a copy-pasteable command per platform (brew on
// macOS, winget on Windows, apt/curl on Linux).
func ghInstallInstructions(goos string) string {
	var b strings.Builder
	b.WriteString("未检测到 GitHub CLI（gh）。请先安装——官方说明: https://cli.github.com/\n")
	switch goos {
	case "darwin":
		b.WriteString("macOS 可直接执行:\n  brew install gh\n")
	case "windows":
		b.WriteString("Windows 可直接执行:\n  winget install GitHub.cli\n")
	case "linux":
		b.WriteString("Debian/Ubuntu 可直接执行:\n  sudo apt install gh\n")
		b.WriteString("其他发行版可用 curl 下载二进制，见官方安装页。\n")
	default:
		b.WriteString("请按官方安装页为你的平台安装 gh。\n")
	}
	return b.String()
}

// defaultLinearStatusMap is the by-type identity fallback (see
// linear-channel-mapping.md §5.2): each Linear WorkflowState type maps to
// itself, so loop-eng can bucket states by type without hard-coded names.
func defaultLinearStatusMap() map[string]string {
	return map[string]string{
		"backlog":   "backlog",
		"unstarted": "unstarted",
		"started":   "started",
		"completed": "completed",
		"canceled":  "canceled",
	}
}

// parseStatusMap parses a "type=state,type=state" comma string into a map.
// Malformed entries are skipped; empty input yields an empty map (caller
// falls back to defaultLinearStatusMap).
func parseStatusMap(s string) map[string]string {
	m := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if k == "" || v == "" {
			continue
		}
		m[k] = v
	}
	return m
}

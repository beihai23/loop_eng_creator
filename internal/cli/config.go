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

	"github.com/mattn/go-isatty"
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
		Short: "交互式配置向导（任务来源 + 干活引擎；脚手架 + 逐步讲解）",
		RunE: func(cmd *cobra.Command, args []string) error {
			if repo == "" {
				repo, _ = os.Getwd()
			}
			// TTY → 全屏向导（bubbletea，alt-screen）；管道/脚本 → 行模式
			// （保持 piped-stdin 脚本兼容，config_test.go 的输入契约不变）。
			if isatty.IsTerminal(os.Stdin.Fd()) && isatty.IsTerminal(os.Stdout.Fd()) {
				return runConfigWizard(repo)
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

	// 开场定向：小白用户最需要的是「我接下来要配什么、为什么」的全景图，
	// 而不是一上来就被追问一堆看不懂的字段。
	fmt.Fprintln(out)
	fmt.Fprintln(out, "loop-eng 配置向导 —— 只需要配好两件事：")
	fmt.Fprintln(out, "  ① 任务来源（channel）：loop-eng 从哪里领任务、把战报写回哪里（GitHub Issue / Linear / 本地目录）")
	fmt.Fprintln(out, "  ② 干活引擎（coding agent）：实际思考和改代码的 AI CLI（claude / codex / kimi …）")
	fmt.Fprintln(out, "提示：[] 里是默认值，直接回车即采用；Ctrl-C 可随时退出。")
	fmt.Fprintln(out)

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
		fmt.Fprintln(out, "GitHub 仓库（owner/名称）：loop-eng 从这个仓库的 Issues 领任务，")
		fmt.Fprintln(out, "并把进度、验证结果和战报评论写回同一个仓库。")
		repoDef := cfg.Channel.Repo
		repoName, err := promptLine(r, out, "GitHub repo (owner/name)", repoDef)
		if err != nil {
			return err
		}
		labelDef := cfg.Channel.TaskLabel
		if labelDef == "" {
			labelDef = "loop:task"
		}
		// task_label 是小白最容易卡住的字段——必须说清三件事：它是干什么的
		// （派发给 AI 的开关）、默认值是什么、配套状态标签谁来检查。
		fmt.Fprintln(out, "任务标签（task_label）：loop-eng 只处理带这个标签的 issue。")
		fmt.Fprintln(out, "想交给 AI 的任务就打上它；没打的 issue 永远不会被自动执行——")
		fmt.Fprintln(out, "这是你控制「哪些任务交给 AI」的总开关。")
		fmt.Fprintln(out, "运行时还会用到 loop:running / loop:done / loop:blocked 等状态标签；")
		fmt.Fprintln(out, "daemon 启动时会自动体检（preflight）并列出缺失标签的创建清单。")
		fmt.Fprintln(out, "没有特殊需求的话，直接回车用默认即可。")
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
		fmt.Fprintln(out, "Linear 项目：loop-eng 从这个项目领任务（issue 带上任务标签，默认同 GitHub 流程）。")
		project, err := promptLine(r, out, "Linear project (name 或 uuid)", projDef)
		if err != nil {
			return err
		}
		team, err := promptLine(r, out, "Linear team key（可空）", teamDef)
		if err != nil {
			return err
		}
		fmt.Fprintln(out, "状态映射（status_map）：loop-eng 的内部状态（running/done/blocked 等）")
		fmt.Fprintln(out, "要对应到你 team 里的 WorkflowState 名称。格式 type=state，逗号分隔；")
		fmt.Fprintln(out, "留空 = 按状态类型自动对应，大多数 team 不需要改。")
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
		fmt.Fprintln(out, "inbox 目录：把任务写成 markdown 文件放进这个目录，loop-eng 会轮询领取——")
		fmt.Fprintln(out, "适合先本地试用，不碰任何外部服务。")
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
	fmt.Fprintln(out)
	fmt.Fprintln(out, "── 第 2 步 · 干活引擎（coding agent）──")
	fmt.Fprintln(out, "loop-eng 自己不思考，它调用 AI CLI 干活。内部分四个环节：")
	fmt.Fprintln(out, "  triage（任务分诊）→ plan（规划方案）→ execute（动手改代码）→ verify（独立验证）")
	fmt.Fprintln(out, "可以四个环节用同一个引擎（简单），也可以分开指定（进阶：比如 verify 用")
	fmt.Fprintln(out, "不同厂商的引擎做交叉验证，独立性更强）。")
	scope, err := promptLine(r, out, "coding-agent provider 配置方式 (1=全局同一个, 2=逐角色分别选)", "")
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
			fmt.Fprintf(out, "── %s（%s）· 当前 provider: %s ──\n", role, roleMeaning(role), roleProvider(cfg, role))
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
	// 收尾：告诉用户配置落在哪、生效了什么、接下来干什么——当前流程在一句
	// 「已写入」后戛然而止，小白不知道下一步。
	fmt.Fprintln(out)
	fmt.Fprintf(out, "配置已写入 %s（channel=%s）\n", cfgPath, provider)
	fmt.Fprintln(out, "接下来：")
	fmt.Fprintln(out, "  1. loop-eng doctor   —— 体检：检查各引擎 CLI 是否装好登录、任务/状态标签是否齐全")
	fmt.Fprintln(out, "  2. loop-eng daemon   —— 启动常驻循环，开始自动领任务")
	switch provider {
	case "github":
		fmt.Fprintf(out, "  3. 给想交给 AI 的 issue 打上 %s 标签，daemon 下一个 tick 就会领走\n", cfg.Channel.TaskLabel)
	case "local":
		fmt.Fprintf(out, "  3. 把任务写成 markdown 文件放进 %s/，daemon 下一个 tick 就会领走\n", cfg.Channel.Inbox)
	case "linear":
		fmt.Fprintln(out, "  3. 在 Linear 项目里给任务打上配置的任务标签，daemon 下一个 tick 就会领走")
	}
	return nil
}

// pickProvider prints the provider menu and reads one line. Accepts
// "1"/"local", "2"/"github", "3"/"linear" (case-insensitive, trimmed). Empty
// line or EOF returns ("", nil) — the caller treats that as "keep current
// config". Invalid input reprints the menu and reads again.
func pickProvider(r *bufio.Reader, out io.Writer) (string, error) {
	for {
		fmt.Fprintln(out, "── 第 1 步 · 任务来源（channel）──")
		fmt.Fprintln(out, "loop-eng 是一个自动干活的循环：领任务 → 规划 → 改代码 → 验证 → 汇报。")
		fmt.Fprintln(out, "「任务来源」就是它领任务、写回战报的地方。选择 channel provider:")
		fmt.Fprintln(out, "  1) local   — 本地 inbox/ 目录：任务写成文件放进去即可，零外部依赖，适合先试用")
		fmt.Fprintln(out, "  2) github  — GitHub Issues：给 issue 打个标签就派发，战报写回 issue 评论（需要 gh CLI）")
		fmt.Fprintln(out, "  3) linear  — Linear：从 Linear 项目领任务（需要 API key）")
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

// agentProviderDesc annotates each registered coding-agent provider with a
// one-line novice-facing description for the config menu. Presentation copy
// lives here (cli layer); the provider SET stays single-sourced in
// model.RegisteredProviders — a new provider appears in the menu as its bare
// name until someone writes its one-liner here.
var agentProviderDesc = map[string]string{
	"claude":   "Anthropic Claude Code（默认；最成熟的接入路径）",
	"codex":    "OpenAI Codex CLI",
	"kimi":     "Kimi Code CLI",
	"kilo":     "Kilo Code CLI",
	"opencode": "opencode（开源，可接多家模型）",
}

// roleMeaning returns the novice-facing one-liner for a SubLoop role, used by
// the per-role provider walkthrough so the user knows what they are picking an
// engine FOR.
func roleMeaning(role string) string {
	switch role {
	case "triage":
		return "任务分诊：判断任务信息够不够、适不适合自动做"
	case "plan":
		return "规划：读代码、定实现方案和验收脚本"
	case "execute":
		return "执行：在隔离 worktree 里动手改代码"
	case "verify":
		return "验证：独立复核改动是否满足验收标准"
	}
	return ""
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
		fmt.Fprintln(out, "选择 coding-agent provider（引擎 CLI 需已安装并登录）：")
		for i, n := range names {
			if desc := agentProviderDesc[n]; desc != "" {
				fmt.Fprintf(out, "  %d) %s — %s\n", i+1, n, desc)
			} else {
				fmt.Fprintf(out, "  %d) %s\n", i+1, n)
			}
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

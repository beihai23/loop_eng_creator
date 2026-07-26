// internal/cli/config_wizard.go — the TTY-mode config wizard (bubbletea).
//
// `loop-eng config` dispatches here when stdin AND stdout are both terminals
// (see config.go); piped stdin / scripts keep the line-based fallback
// (runConfigInteractive). The wizard is a full-screen alt-screen program — it
// never pollutes scrollback — with lipgloss styling: section titles, dim
// explanation text, highlighted selection, colored status lines.
//
// The model is a pure state machine: Update() never touches the terminal, so
// tests drive it with synthetic tea.KeyMsg and assert on state + the saved
// config (see config_wizard_test.go). Rendering (View) is a pure function of
// state.
package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"loop-eng/internal/channel"
	"loop-eng/internal/config"
	"loop-eng/internal/model"
)

// ---- styles (lipgloss; degrade gracefully on dumb terminals) ----

var (
	wzTitle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12")).MarginTop(1)
	wzStep    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6")).MarginTop(1)
	wzDim     = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	wzText    = lipgloss.NewStyle()
	wzSel     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("10"))
	wzOpt     = lipgloss.NewStyle().Foreground(lipgloss.Color("7"))
	wzWarn    = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	wzOK      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("10"))
	wzErr     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("9"))
	wzHelp    = lipgloss.NewStyle().Foreground(lipgloss.Color("8")).MarginTop(1)
	wzInputV  = lipgloss.NewStyle().Foreground(lipgloss.Color("15"))
	wzConfirm = lipgloss.NewStyle().MarginTop(1)
)

// wizStep is the wizard's screen. Transitions depend on answers (see advance).
type wizStep int

const (
	stepWelcome wizStep = iota
	stepChannel
	stepGitHubRepo
	stepGitHubLabel // select: 用默认 / 自定义
	stepGitHubLabelCustom
	stepGitHubLabels // 一键创建任务标签 + loop:* 状态标签（可选步骤）
	stepLinearKeyChoice
	stepLinearKey
	stepLinearProject
	stepLinearTeam
	stepLinearStatusMap
	stepLinearStatusMapCustom
	stepLocalInbox
	stepScope
	stepAgentGlobal
	stepAgentRole
	stepConfirm
	stepDone
)

// wzOption is one selectable row in a menu step.
type wzOption struct {
	Value string // answer value stored on choose
	Label string // headline
	Desc  string // one-line dim explanation
}

// wizard is the tea.Model for the config wizard.
type wizard struct {
	repo   string
	cfg    *config.Config
	cfgDir string // .loop dir (scaffolded by caller)

	step   wizStep
	cursor int           // selected option index in menu steps
	input  []rune        // text-input buffer in input steps
	errMsg string        // inline validation error (red), cleared on next keystroke
	roleIx int           // per-role agent picking: which role we're on
	roles  []string      // fixed role order for stepAgentRole
	agents []wzOption    // agent menu built from the registry

	// answers
	provider    string
	repoName    string
	taskLabel   string
	labelPrefix string // 状态标签族前缀（空 = "loop:"；自定义时 taskLabel=prefix+"task"）
	labelNote   string // 标签创建步骤的结果说明（confirm/done 页展示）
	inbox       string
	keyChoice string // "1"=env 指令, "2"=写 .loop/linear.key
	linearKey string
	project   string
	team      string
	statusMap string
	scope     string // "1"=全局, "2"=逐角色
	agentPick map[string]string // role → provider (全局时全部同值)

	saved bool
	quit  bool // ctrl+c before save
}

// newWizard builds the wizard over an already-scaffolded .loop and loaded cfg.
func newWizard(repo string, cfg *config.Config) *wizard {
	agents := make([]wzOption, 0)
	for _, n := range model.RegisteredProviders() {
		agents = append(agents, wzOption{Value: n, Label: n, Desc: agentProviderDesc[n]})
	}
	return &wizard{
		repo:      repo,
		cfg:       cfg,
		cfgDir:    filepath.Join(repo, ".loop"),
		step:      stepWelcome,
		roles:     []string{"triage", "plan", "execute", "verify"},
		agents:    agents,
		agentPick: map[string]string{},
		// 再配置场景的「保持现状」基线：labelPrefix 从现有 config 预填——选
		// 「用默认标签」路径不动它（只改 taskLabel），否则重跑向导会把已有的
		// 自定义前缀静默抹回 "loop:"（task_label 与 label_prefix 配对撕裂）。
		labelPrefix: cfg.Channel.LabelPrefix,
	}
}

// runConfigWizard scaffolds .loop when missing, loads config, and runs the
// full-screen wizard. Called only when stdin+stdout are both terminals.
func runConfigWizard(repo string) error {
	loopDir := filepath.Join(repo, ".loop")
	if _, err := os.Stat(loopDir); os.IsNotExist(err) {
		fmt.Printf("未发现 %s，先生成脚手架...\n", loopDir)
		if err := scaffoldLoop(repo); err != nil {
			return err
		}
	}
	cfg, err := loadConfig(filepath.Join(loopDir, "config.yaml"))
	if err != nil {
		return err
	}
	w := newWizard(repo, cfg)
	if _, err := tea.NewProgram(w, tea.WithAltScreen()).Run(); err != nil {
		return err
	}
	if w.quit && !w.saved {
		fmt.Println("已退出，未修改配置。")
	}
	return nil
}

// Init implements tea.Model.
func (w *wizard) Init() tea.Cmd { return nil }

// ---- state machine ----

// keySelect moves the cursor in a menu step; returns true if the key was consumed.
func (w *wizard) keySelect(msg tea.KeyMsg, n int) bool {
	switch msg.String() {
	case "up", "k":
		if w.cursor > 0 {
			w.cursor--
		}
	case "down", "j":
		if w.cursor < n-1 {
			w.cursor++
		}
	default:
		return false
	}
	w.errMsg = ""
	return true
}

// jumpSelect maps digit keys ("1".."9") onto menu indices; -1 when not a digit.
func jumpSelect(msg tea.KeyMsg, n int) int {
	s := msg.String()
	if len(s) != 1 || s[0] < '1' || s[0] > '9' {
		return -1
	}
	i := int(s[0] - '1')
	if i >= n {
		return -1
	}
	return i
}

// keyInput edits the text buffer; enter is NOT consumed here (the step handles it).
func (w *wizard) keyInput(msg tea.KeyMsg) {
	switch msg.Type {
	case tea.KeyBackspace:
		if len(w.input) > 0 {
			w.input = w.input[:len(w.input)-1]
		}
	case tea.KeyRunes:
		w.input = append(w.input, msg.Runes...)
	case tea.KeySpace:
		w.input = append(w.input, ' ')
	}
	w.errMsg = ""
}

// inputText returns the trimmed input, or def when the buffer is empty.
func (w *wizard) inputText(def string) string {
	if s := strings.TrimSpace(string(w.input)); s != "" {
		return s
	}
	return def
}

// Update implements tea.Model — the wizard state machine.
func (w *wizard) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return w, nil
	}
	if key.String() == "ctrl+c" {
		w.quit = true
		return w, tea.Quit
	}
	switch w.step {
	case stepWelcome:
		if key.String() == "enter" {
			w.step = stepChannel
			w.cursor = 0
		}
	case stepDone:
		if key.String() == "enter" || key.String() == "q" {
			w.quit = true
			return w, tea.Quit
		}
	case stepChannel:
		opts := w.channelOptions()
		if w.keySelect(key, len(opts)) {
			return w, nil
		}
		if i := jumpSelect(key, len(opts)); i >= 0 {
			w.cursor = i
		}
		if key.String() == "enter" {
			w.provider = opts[w.cursor].Value
			w.advance()
		}
	case stepGitHubRepo:
		if key.String() == "enter" {
			w.repoName = w.inputText(w.cfg.Channel.Repo)
			if w.repoName == "" {
				w.errMsg = "repo 必填（owner/name）"
				return w, nil
			}
			w.advance()
			return w, nil
		}
		w.keyInput(key)
	case stepGitHubLabel:
		opts := []wzOption{
			{Value: "default", Label: "用默认标签（推荐）", Desc: w.defaultLabel() + " —— 绝大多数用户的选择"},
			{Value: "custom", Label: "自定义标签前缀", Desc: "多实例共存同一仓库、或组织命名规范时：ai: → ai:task + ai:running + ai:done…"},
		}
		if w.keySelect(key, len(opts)) {
			return w, nil
		}
		if i := jumpSelect(key, len(opts)); i >= 0 {
			w.cursor = i
		}
		if key.String() == "enter" {
			if opts[w.cursor].Value == "default" {
				w.taskLabel = w.defaultLabel()
				w.advance()
			} else {
				w.step = stepGitHubLabelCustom
				w.input = nil
			}
		}
	case stepGitHubLabelCustom:
		if key.String() == "enter" {
			raw := strings.TrimSpace(string(w.input))
			if raw == "" {
				// 留空回车 = 采用默认/保持现状（与上一步选「用默认标签」等价）——
				// 自定义页不是死胡同。注意不动 labelPrefix：再配置场景下预填的
				// 现状值就是「默认」的语义载体（见 newWizard 的预填注释）。
				w.taskLabel = w.defaultLabel()
				w.advance()
				return w, nil
			}
			prefix := normalizeLabelPrefix(raw)
			w.labelPrefix = prefix
			w.taskLabel = prefix + "task" // 前缀派生身份标签：ai: → ai:task
			w.advance()
			return w, nil
		}
		w.keyInput(key)
	case stepGitHubLabels:
		opts := []wzOption{
			{Value: "create", Label: "现在就创建（推荐）", Desc: "gh label create --force，幂等；已存在的标签不受影响"},
			{Value: "skip", Label: "跳过，稍后自己补", Desc: "daemon 启动的 preflight 会列出缺失清单，照单创建即可"},
		}
		if w.keySelect(key, len(opts)) {
			return w, nil
		}
		if i := jumpSelect(key, len(opts)); i >= 0 {
			w.cursor = i
		}
		if key.String() == "enter" {
			if opts[w.cursor].Value == "create" {
				w.labelNote = w.createGitHubLabels()
			} else {
				w.labelNote = "已跳过标签创建——daemon 启动的 preflight 会检查并列出缺失清单"
			}
			w.advance()
		}
	case stepLinearKeyChoice:
		opts := []wzOption{
			{Value: "1", Label: "打印 env 指令（推荐）", Desc: "key 不落盘，你自己把 export 加进 shell rc"},
			{Value: "2", Label: "写入 .loop/linear.key", Desc: "0600 权限；.loop/ 已在 .gitignore"},
		}
		if w.keySelect(key, len(opts)) {
			return w, nil
		}
		if i := jumpSelect(key, len(opts)); i >= 0 {
			w.cursor = i
		}
		if key.String() == "enter" {
			w.keyChoice = opts[w.cursor].Value
			w.step = stepLinearKey
			w.input = nil
		}
	case stepLinearKey:
		if key.String() == "enter" {
			w.linearKey = w.inputText("")
			w.step = stepLinearProject
			w.input = nil
			return w, nil
		}
		w.keyInput(key)
	case stepLinearProject:
		if key.String() == "enter" {
			def := ""
			if w.cfg.Channel.Linear != nil {
				def = w.cfg.Channel.Linear.Project
			}
			w.project = w.inputText(def)
			if w.project == "" {
				w.errMsg = "project 必填（name 或 uuid）"
				return w, nil
			}
			w.step = stepLinearTeam
			w.input = nil
			return w, nil
		}
		w.keyInput(key)
	case stepLinearTeam:
		if key.String() == "enter" {
			def := ""
			if w.cfg.Channel.Linear != nil {
				def = w.cfg.Channel.Linear.Team
			}
			w.team = w.inputText(def)
			w.step = stepLinearStatusMap
			w.input = nil
			return w, nil
		}
		w.keyInput(key)
	case stepLinearStatusMap:
		opts := []wzOption{
			{Value: "auto", Label: "自动映射（推荐）", Desc: "按状态类型对应 WorkflowState，大多数 team 不用改"},
			{Value: "custom", Label: "自定义映射", Desc: "type=state 逗号串，如 running=In Progress,done=Done"},
		}
		if w.keySelect(key, len(opts)) {
			return w, nil
		}
		if i := jumpSelect(key, len(opts)); i >= 0 {
			w.cursor = i
		}
		if key.String() == "enter" {
			if opts[w.cursor].Value == "auto" {
				w.statusMap = ""
				w.advance()
			} else {
				w.step = stepLinearStatusMapCustom
				w.input = nil
			}
		}
	case stepLinearStatusMapCustom:
		if key.String() == "enter" {
			w.statusMap = w.inputText("")
			w.advance()
			return w, nil
		}
		w.keyInput(key)
	case stepLocalInbox:
		if key.String() == "enter" {
			def := w.cfg.Channel.Inbox
			if def == "" {
				def = "inbox"
			}
			w.inbox = w.inputText(def)
			w.advance()
			return w, nil
		}
		w.keyInput(key)
	case stepScope:
		opts := []wzOption{
			{Value: "1", Label: "全局同一个（简单，推荐先用这个）", Desc: "四个环节共用一个引擎"},
			{Value: "2", Label: "逐角色分别选（进阶）", Desc: "如 verify 换一家厂商做交叉验证"},
		}
		if w.keySelect(key, len(opts)) {
			return w, nil
		}
		if i := jumpSelect(key, len(opts)); i >= 0 {
			w.cursor = i
		}
		if key.String() == "enter" {
			w.scope = opts[w.cursor].Value
			w.cursor = 0
			w.advance()
		}
	case stepAgentGlobal:
		if w.keySelect(key, len(w.agents)) {
			return w, nil
		}
		if i := jumpSelect(key, len(w.agents)); i >= 0 {
			w.cursor = i
		}
		if key.String() == "enter" {
			p := w.agents[w.cursor].Value
			for _, r := range w.roles {
				w.agentPick[r] = p
			}
			w.advance()
		}
	case stepAgentRole:
		if w.keySelect(key, len(w.agents)) {
			return w, nil
		}
		if i := jumpSelect(key, len(w.agents)); i >= 0 {
			w.cursor = i
		}
		if key.String() == "enter" {
			role := w.roles[w.roleIx]
			w.agentPick[role] = w.agents[w.cursor].Value
			w.roleIx++
			w.cursor = 0
			if w.roleIx >= len(w.roles) {
				w.advance()
			}
		}
	case stepConfirm:
		opts := []wzOption{
			{Value: "save", Label: "保存配置", Desc: "写入 .loop/config.yaml"},
			{Value: "cancel", Label: "放弃修改", Desc: "不写盘，直接退出"},
		}
		if w.keySelect(key, len(opts)) {
			return w, nil
		}
		if key.String() == "enter" {
			if opts[w.cursor].Value == "save" {
				if err := w.applyAndSave(); err != nil {
					w.errMsg = err.Error()
					return w, nil
				}
				w.saved = true
				w.step = stepDone
			} else {
				w.quit = true
				return w, tea.Quit
			}
		}
	}
	return w, nil
}

// ghLabelCreate creates (or --force-updates) one repo label. Package var for
// test injection — the real implementation shells out to gh.
var ghLabelCreate = func(repo, name string) error {
	out, err := exec.Command("gh", "label", "create", name, "--repo", repo, "--force").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// createGitHubLabels creates the full required label set (task label + the
// loop:<status> family — the same set channel.RequiredGitHubLabels/Preflight
// checks) in the chosen repo, and returns the human-readable result note shown
// on the confirm/done screens. Best-effort per label: failures are collected
// and reported, not fatal — preflight will re-list any gap at daemon start.
func (w *wizard) createGitHubLabels() string {
	names := channel.RequiredGitHubLabels(w.labelPrefix, w.taskLabel)
	var failed []string
	for _, n := range names {
		if err := ghLabelCreate(w.repoName, n); err != nil {
			failed = append(failed, n)
		}
	}
	if len(failed) == 0 {
		return fmt.Sprintf("✓ 已在 %s 创建/确认 %d 个标签（任务标签 + loop:* 状态标签）", w.repoName, len(names))
	}
	return fmt.Sprintf("⚠ %d/%d 个标签创建失败（%s）——daemon 启动前用 loop-eng doctor 复查并照清单补齐",
		len(failed), len(names), strings.Join(failed, ", "))
}

// normalizeLabelPrefix trims the user's prefix input and ensures a trailing
// ":" — people type "ai" and mean "ai:"; the normalized form is what label
// construction (prefix+"task", prefix+"running") concatenates onto.
func normalizeLabelPrefix(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if !strings.HasSuffix(s, ":") {
		s += ":"
	}
	return s
}

// defaultLabel resolves the effective task label (current config or loop:task).
func (w *wizard) defaultLabel() string {
	if w.cfg.Channel.TaskLabel != "" {
		return w.cfg.Channel.TaskLabel
	}
	return "loop:task"
}

// advance computes the next step after a milestone answer.
func (w *wizard) advance() {
	w.cursor = 0
	w.input = nil
	w.errMsg = ""
	switch w.step {
	case stepChannel:
		switch w.provider {
		case "github":
			w.step = stepGitHubRepo
			if w.cfg.Channel.Repo != "" {
				w.input = []rune(w.cfg.Channel.Repo)
			}
		case "linear":
			w.step = stepLinearKeyChoice
		default: // local
			w.step = stepLocalInbox
			def := w.cfg.Channel.Inbox
			if def == "" {
				def = "inbox"
			}
			w.input = []rune(def)
		}
	case stepGitHubRepo:
		w.step = stepGitHubLabel
	case stepGitHubLabel, stepGitHubLabelCustom:
		w.step = stepGitHubLabels
	case stepGitHubLabels, stepLinearStatusMap, stepLinearStatusMapCustom, stepLocalInbox:
		w.step = stepScope
	case stepScope:
		if w.scope == "2" {
			w.step = stepAgentRole
			w.roleIx = 0
		} else {
			w.step = stepAgentGlobal
		}
	case stepAgentGlobal, stepAgentRole:
		w.step = stepConfirm
	}
}

// channelOptions is the step-1 menu.
func (w *wizard) channelOptions() []wzOption {
	return []wzOption{
		{Value: "local", Label: "local — 本地 inbox/ 目录", Desc: "任务写成 markdown 文件放进去即可；零外部依赖，适合先试用"},
		{Value: "github", Label: "github — GitHub Issues", Desc: "给 issue 打个标签就派发任务，战报写回 issue 评论（需要 gh CLI）"},
		{Value: "linear", Label: "linear — Linear", Desc: "从 Linear 项目领任务（需要 API key）"},
	}
}

// applyAndSave writes the wizard's answers into cfg and saves (plus the
// linear.key side file when chosen) — the same semantics as the line-mode flow.
func (w *wizard) applyAndSave() error {
	cfg := w.cfg
	cfg.Channel.Provider = w.provider
	switch w.provider {
	case "github":
		cfg.Channel.Repo = w.repoName
		cfg.Channel.TaskLabel = w.taskLabel
		cfg.Channel.LabelPrefix = w.labelPrefix // 空 = 默认 "loop:"
	case "linear":
		statusMap := defaultLinearStatusMap()
		if parsed := parseStatusMap(w.statusMap); len(parsed) > 0 {
			statusMap = parsed
		}
		cfg.Channel.Linear = &config.LinearChannel{Project: w.project, Team: w.team, StatusMap: statusMap}
		if w.keyChoice == "2" && w.linearKey != "" {
			keyPath := filepath.Join(w.cfgDir, "linear.key")
			if err := os.WriteFile(keyPath, []byte(w.linearKey+"\n"), 0600); err != nil {
				return err
			}
		}
	case "local":
		cfg.Channel.Inbox = w.inbox
	}
	for role, p := range w.agentPick {
		setRoleProvider(cfg, role, p)
	}
	return config.Save(filepath.Join(w.cfgDir, "config.yaml"), cfg)
}

// ---- rendering (pure function of state) ----

const wzFooterKeys = "↑/↓ 或 j/k 移动 · 数字键直选 · enter 确认 · ctrl+c 退出"

// renderMenu renders a menu step: title + dim explanation lines + options with
// the cursor row highlighted.
func renderMenu(title string, desc []string, opts []wzOption, cursor int) string {
	var b strings.Builder
	b.WriteString(wzStep.Render(title) + "\n\n")
	for _, d := range desc {
		b.WriteString(wzDim.Render(d) + "\n")
	}
	if len(desc) > 0 {
		b.WriteString("\n")
	}
	for i, o := range opts {
		if i == cursor {
			b.WriteString(wzSel.Render("❯ "+o.Label) + "\n")
			if o.Desc != "" {
				b.WriteString(wzDim.Render("    "+o.Desc) + "\n")
			}
		} else {
			b.WriteString(wzOpt.Render("  "+o.Label) + "\n")
			if o.Desc != "" {
				b.WriteString(wzDim.Render("    "+o.Desc) + "\n")
			}
		}
	}
	return b.String()
}

// renderInput renders a text-input step: title + dim explanation + the input
// line with a block cursor at the end.
func renderInput(title string, desc []string, input []rune) string {
	var b strings.Builder
	b.WriteString(wzStep.Render(title) + "\n\n")
	for _, d := range desc {
		b.WriteString(wzDim.Render(d) + "\n")
	}
	if len(desc) > 0 {
		b.WriteString("\n")
	}
	b.WriteString("❯ " + wzInputV.Render(string(input)) + wzSel.Render("█") + "\n")
	return b.String()
}

// View implements tea.Model.
func (w *wizard) View() string {
	var b strings.Builder
	b.WriteString(wzTitle.Render("loop-eng 配置向导") + "\n")

	switch w.step {
	case stepWelcome:
		b.WriteString("\n只需要配好两件事：\n\n")
		b.WriteString(wzText.Render("  ① 任务来源（channel）") + wzDim.Render(" —— loop-eng 从哪里领任务、把战报写回哪里") + "\n")
		b.WriteString(wzText.Render("  ② 干活引擎（coding agent）") + wzDim.Render(" —— 实际思考和改代码的 AI CLI（claude / codex / kimi …）") + "\n")
		b.WriteString("\n" + wzDim.Render("loop-eng 是一个自动干活的循环：领任务 → 规划 → 改代码 → 验证 → 汇报。") + "\n")
		b.WriteString(wzHelp.Render("enter 开始 · ctrl+c 退出"))

	case stepChannel:
		b.WriteString(renderMenu("第 1 步 · 任务来源（channel）",
			[]string{"它领任务的工单系统，也是战报写回的地方。"},
			w.channelOptions(), w.cursor))

	case stepGitHubRepo:
		desc := []string{
			"loop-eng 从这个仓库的 Issues 领任务，并把进度、验证结果和战报评论写回同一个仓库。",
			"格式 owner/名称，直接编辑后回车。",
		}
		if !ghAvailable() {
			desc = append(desc, wzWarn.Render("⚠ 未检测到 gh CLI，使用前请先安装：https://cli.github.com/"))
		} else if !ghAuthed() {
			desc = append(desc, wzWarn.Render("⚠ gh 已安装但未登录，使用前请先执行：gh auth login"))
		}
		b.WriteString(renderInput("GitHub 仓库（owner/name）", desc, w.input))

	case stepGitHubLabel:
		b.WriteString(renderMenu("任务标签（task_label）",
			[]string{
				"loop-eng 只处理带这个标签的 issue——打上标签 = 把任务交给 AI，",
				"不打的永远不会被自动执行。这是你控制「哪些任务交给 AI」的总开关。",
				"运行时还会用到 loop:running / loop:done / loop:blocked 等状态标签，",
				"daemon 启动时会自动体检（preflight）并列出缺失标签的创建清单。",
			},
			[]wzOption{
				{Value: "default", Label: "用默认标签（推荐）", Desc: w.defaultLabel() + " —— 绝大多数用户的选择"},
				{Value: "custom", Label: "自定义标签", Desc: "多个 loop-eng 实例共用同一仓库、或组织另有约定时才需要"},
			}, w.cursor))

	case stepGitHubLabelCustom:
		b.WriteString(renderInput("自定义标签前缀",
			[]string{"输入前缀（如 ai 或 ai:），loop-eng 会派生整组标签：",
				"  <前缀>task（任务开关）· <前缀>running / <前缀>done / <前缀>blocked …（状态族）",
				"下一步可以直接帮你在仓库里创建整组。",
				"拿不准就留空回车——直接采用默认（loop:），和选「用默认标签」一样。"}, w.input))

	case stepGitHubLabels:
		statusFamily := "loop:*"
		if w.labelPrefix != "" {
			statusFamily = w.labelPrefix + "*"
		}
		b.WriteString(renderMenu("在仓库里创建所需标签？",
			[]string{
				"loop-eng 运行需要这些标签真实存在于 " + w.repoName + "：",
				"任务标签 " + w.taskLabel + "（派发开关）+ 全套 " + statusFamily + " 状态标签（进度展示）。",
				"不创建的话 daemon 会拒绝启动并列出缺失清单。",
			},
			[]wzOption{
				{Value: "create", Label: "现在就创建（推荐）", Desc: "gh label create --force，幂等；已存在的标签不受影响"},
				{Value: "skip", Label: "跳过，稍后自己补", Desc: "daemon 启动的 preflight 会列出缺失清单，照单创建即可"},
			}, w.cursor))

	case stepLinearKeyChoice:
		b.WriteString(renderMenu("Linear API key 存放方式",
			[]string{"key 永远不会写进 config.yaml（安全约定）。生成位置：",
				"Linear → Settings → Security & access → Personal API keys。"},
			[]wzOption{
				{Value: "1", Label: "打印 env 指令（推荐）", Desc: "key 不落盘，你自己把 export 加进 shell rc"},
				{Value: "2", Label: "写入 .loop/linear.key", Desc: "0600 权限；.loop/ 已在 .gitignore"},
			}, w.cursor))

	case stepLinearKey:
		b.WriteString(renderInput("Linear API key", []string{"粘贴 key 后回车（输入内容会显示——注意周围没有人偷看屏幕）。"}, w.input))

	case stepLinearProject:
		b.WriteString(renderInput("Linear 项目", []string{"loop-eng 从这个项目领任务。填项目名或 uuid。"}, w.input))

	case stepLinearTeam:
		b.WriteString(renderInput("Linear team key（可空）", []string{"限定到某个 team；留空回车 = 不限制。"}, w.input))

	case stepLinearStatusMap:
		b.WriteString(renderMenu("状态映射（status_map）",
			[]string{"loop-eng 的内部状态（running/done/blocked 等）要对应到你 team 的 WorkflowState。"},
			[]wzOption{
				{Value: "auto", Label: "自动映射（推荐）", Desc: "按状态类型对应 WorkflowState，大多数 team 不用改"},
				{Value: "custom", Label: "自定义映射", Desc: "type=state 逗号串，如 running=In Progress,done=Done"},
			}, w.cursor))

	case stepLinearStatusMapCustom:
		b.WriteString(renderInput("自定义状态映射", []string{"格式 type=state，逗号分隔，如 running=In Progress,done=Done。"}, w.input))

	case stepLocalInbox:
		b.WriteString(renderInput("inbox 目录",
			[]string{"把任务写成 markdown 文件放进这个目录，loop-eng 会轮询领取——", "适合先本地试用，不碰任何外部服务。"}, w.input))

	case stepScope:
		b.WriteString(renderMenu("第 2 步 · 干活引擎（coding agent）",
			[]string{
				"loop-eng 自己不思考，它调用 AI CLI 干活。内部分四个环节：",
				"  triage（任务分诊）→ plan（规划方案）→ execute（动手改代码）→ verify（独立验证）",
			},
			[]wzOption{
				{Value: "1", Label: "全局同一个（简单，推荐先用这个）", Desc: "四个环节共用一个引擎"},
				{Value: "2", Label: "逐角色分别选（进阶）", Desc: "如 verify 换一家厂商做交叉验证"},
			}, w.cursor))

	case stepAgentGlobal:
		b.WriteString(renderMenu("选择引擎（四个环节共用）",
			[]string{"对应的 CLI 需要已安装并登录。"}, w.agents, w.cursor))

	case stepAgentRole:
		role := w.roles[w.roleIx]
		b.WriteString(renderMenu(
			fmt.Sprintf("逐角色选引擎（%d/%d）：%s", w.roleIx+1, len(w.roles), role),
			[]string{roleMeaning(role), "直接回车选高亮项；对应的 CLI 需要已安装并登录。"},
			w.agents, w.cursor))

	case stepConfirm:
		b.WriteString(wzStep.Render("确认配置") + "\n\n")
		for _, line := range w.summaryLines() {
			b.WriteString(wzText.Render("  "+line) + "\n")
		}
		if w.labelNote != "" {
			b.WriteString(wzDim.Render("  "+w.labelNote) + "\n")
		}
		b.WriteString("\n")
		b.WriteString(renderMenu("", nil, []wzOption{
			{Value: "save", Label: "保存配置", Desc: "写入 .loop/config.yaml"},
			{Value: "cancel", Label: "放弃修改", Desc: "不写盘，直接退出"},
		}, w.cursor))

	case stepDone:
		b.WriteString("\n" + wzOK.Render("✓ 配置已写入 "+filepath.Join(w.cfgDir, "config.yaml")) + "\n")
		if w.labelNote != "" {
			b.WriteString(wzDim.Render("  "+w.labelNote) + "\n")
		}
		b.WriteString("\n")
		b.WriteString(wzText.Render("接下来：") + "\n")
		b.WriteString(wzDim.Render("  1. loop-eng doctor   —— 体检：检查各引擎 CLI 是否装好登录、任务/状态标签是否齐全") + "\n")
		b.WriteString(wzDim.Render("  2. loop-eng daemon   —— 启动常驻循环，开始自动领任务") + "\n")
		switch w.provider {
		case "github":
			b.WriteString(wzDim.Render("  3. 给想交给 AI 的 issue 打上 "+w.taskLabel+" 标签，daemon 下一个 tick 就会领走") + "\n")
		case "local":
			b.WriteString(wzDim.Render("  3. 把任务写成 markdown 文件放进 "+w.inbox+"/，daemon 下一个 tick 就会领走") + "\n")
		case "linear":
			if w.keyChoice != "2" {
				b.WriteString(wzDim.Render("  3. 先把 key 加进 shell rc：export LOOP_ENG_LINEAR_API_KEY=<你的 key>") + "\n")
			} else {
				b.WriteString(wzDim.Render("  3. 在 Linear 项目里给任务打上任务标签，daemon 下一个 tick 就会领走") + "\n")
			}
		}
		b.WriteString(wzHelp.Render("enter 退出"))
	}

	if w.errMsg != "" {
		b.WriteString("\n" + wzErr.Render("✗ "+w.errMsg) + "\n")
	}
	if w.step != stepWelcome && w.step != stepDone {
		b.WriteString(wzHelp.Render(wzFooterKeys))
	}
	return b.String()
}

// summaryLines renders the confirm step's review block from the answers.
func (w *wizard) summaryLines() []string {
	var lines []string
	switch w.provider {
	case "github":
		lines = append(lines, "任务来源: github — "+w.repoName, "任务标签: "+w.taskLabel)
		if w.labelPrefix != "" && w.labelPrefix != "loop:" {
			lines = append(lines, "标签前缀: "+w.labelPrefix+"（状态族 "+w.labelPrefix+"running / "+w.labelPrefix+"done …）")
		}
	case "linear":
		lines = append(lines, "任务来源: linear — "+w.project)
		if w.team != "" {
			lines = append(lines, "team: "+w.team)
		}
		if w.keyChoice == "2" {
			lines = append(lines, "API key: 写入 .loop/linear.key（0600）")
		} else {
			lines = append(lines, "API key: 不落盘（稍后自行 export LOOP_ENG_LINEAR_API_KEY）")
		}
	case "local":
		lines = append(lines, "任务来源: local — inbox 目录 "+w.inbox)
	}
	for _, role := range w.roles {
		lines = append(lines, fmt.Sprintf("%s（%s）: %s", role, roleMeaning(role), w.agentPick[role]))
	}
	return lines
}

// Package tui 实现 loop-eng dashboard 交互式看板（spec §3/§5）：三 Tab +
// 真彩色 + 进行中呼吸灯 + resume/cancel 轻量操控。bubbletea 程序，和 daemon
// 共享同一个 SQLite；数据 tick ~2s 重读，动画 tick ~60ms 重绘呼吸灯。
package tui

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"loop-eng/internal/config"
	"loop-eng/internal/state"
)

// tab 枚举：总览 / 详情 / 轨迹
type tab int

const (
	tabOverview tab = iota
	tabDetail
	tabTrace
)

// Model 是 dashboard 的 bubbletea Model。B1 只放骨架字段；B7 会加 selIdx/selTask/snap/animPhase。
type Model struct {
	store         *state.Store
	cfg           *config.Config
	tab           tab
	width, height int
	quit          bool
}

// New 构造 Model。store 由 dashboard 命令开好（读写在同一条连接：读快照 + 写
// commands；modernc.org/sqlite 短事务可承受与 daemon 的低竞争）。
func New(st *state.Store, cfg *config.Config) Model {
	return Model{store: st, cfg: cfg, tab: tabOverview}
}

// Init 启动命令；B1 无定时器，B7 加 data/anim tick。
func (m Model) Init() tea.Cmd { return nil }

// Update 处理按键 / 窗口尺寸。B1 仅 q / ctrl+c 退出 + 记录终端尺寸。
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			m.quit = true
			return m, tea.Quit
		}
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	}
	return m, nil
}

// View 渲染当前帧。B1 只渲染标题 + 退出提示；B2 起填三 Tab 内容。
func (m Model) View() string {
	if m.quit {
		return ""
	}
	title := lipgloss.NewStyle().Bold(true).Render("loop-eng dashboard")
	hint := lipgloss.NewStyle().Faint(true).Render("（骨架）q 退出")
	return fmt.Sprintf("%s\n%s\n", title, hint)
}

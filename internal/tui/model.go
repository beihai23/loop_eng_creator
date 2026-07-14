// Package tui 实现 loop-eng dashboard 交互式看板（spec §3/§5）：三 Tab +
// 真彩色 + 进行中呼吸灯 + resume/cancel 轻量操控。bubbletea 程序，和 daemon
// 共享同一个 SQLite；数据 tick ~2s 重读，动画 tick ~60ms 重绘呼吸灯。
package tui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
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

// Model 是 dashboard 的 bubbletea Model。B1 只放骨架字段；B7 加 selIdx/selTask/snap/animPhase。
type Model struct {
	store         *state.Store
	cfg           *config.Config
	tab           tab
	width, height int
	quit          bool
	selIdx        int       // 总览列表选中索引
	selTask       string    // 当前选中的 task id（详情/轨迹用）
	snap          *Snapshot // 数据 tick 刷新
	animPhase     float64   // 动画相位（动画 tick 推进）
}

// New 构造 Model。store 由 dashboard 命令开好（读写在同一条连接：读快照 + 写
// commands；modernc.org/sqlite 短事务可承受与 daemon 的低竞争）。
func New(st *state.Store, cfg *config.Config) Model {
	return Model{store: st, cfg: cfg, tab: tabOverview}
}

// 两个 tick 的消息：dataTick 重读快照，animTick 推进呼吸灯相位。
type dataTickMsg struct{}
type animTickMsg struct{}

const (
	dataTickInterval = 2 * time.Second       // 数据 tick：~2s 重读 SQLite
	animTickInterval = 60 * time.Millisecond // 动画 tick：~60ms 重绘呼吸灯
)

// dataTick 产生一次数据 tick 命令（到期后返回 dataTickMsg）。
func dataTick() tea.Cmd {
	return tea.Tick(dataTickInterval, func(time.Time) tea.Msg { return dataTickMsg{} })
}

// animTick 产生一次动画 tick 命令（到期后返回 animTickMsg）。
func animTick() tea.Cmd {
	return tea.Tick(animTickInterval, func(time.Time) tea.Msg { return animTickMsg{} })
}

// Init 启动双 tick（数据 + 动画）。
func (m Model) Init() tea.Cmd {
	return tea.Batch(dataTick(), animTick())
}

// Update 处理双 tick / 按键 / 窗口尺寸 / 鼠标。
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height

	case dataTickMsg:
		snap, _ := ReadSnapshot(m.store, m.cfg)
		m.snap = snap
		return m, dataTick() // 继续

	case animTickMsg:
		m.animPhase += 0.15
		return m, animTick()

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			m.quit = true
			return m, tea.Quit
		case "1":
			m.tab = tabOverview
		case "2":
			m.tab = tabDetail
		case "3", "t":
			m.tab = tabTrace
		case "esc":
			m.tab = tabOverview
		case "up", "k":
			if m.selIdx > 0 {
				m.selIdx--
			}
		case "down", "j":
			if m.snap != nil && m.selIdx < len(m.snap.Tasks)-1 {
				m.selIdx++
			}
		case "enter":
			if m.snap != nil && m.selIdx < len(m.snap.Tasks) {
				m.selTask = m.snap.Tasks[m.selIdx].ID
				m.tab = tabDetail
			}
		case "r":
			if m.selTask != "" {
				_ = issueResume(m.store, m.selTask, "")
			}
		case "x":
			if m.selTask != "" {
				_ = issueCancel(m.store, m.selTask)
			}
		}

	case tea.MouseMsg:
		// v1：鼠标点击总览行 = 选中（简化：用 Y 坐标粗映射 selIdx）
		// 完整鼠标 hit-test 留后续；此处至少不崩。
	}
	return m, nil
}

// View 按当前 tab 渲染一帧。首屏数据未到时先读一次。
func (m Model) View() string {
	if m.quit {
		return ""
	}
	// 首屏数据未到：先读一次
	if m.snap == nil {
		m.snap, _ = ReadSnapshot(m.store, m.cfg)
	}
	switch m.tab {
	case tabOverview:
		return RenderOverview(m.snap, m.selIdx, m.animPhase, m.width)
	case tabDetail:
		if m.selTask == "" {
			return "（未选中任务）\n"
		}
		return RenderDetail(m.store, m.cfg, m.selTask)
	case tabTrace:
		if m.selTask == "" {
			return "（未选中任务）\n"
		}
		return RenderTrace(m.store, m.selTask)
	}
	return ""
}

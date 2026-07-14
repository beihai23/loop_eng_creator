package tui

import "github.com/charmbracelet/lipgloss"

// statusSymbol 状态→符号（spec §6）。NO_COLOR 时 lipgloss 自动降级为纯文本。
func statusSymbol(status string) string {
	switch status {
	case "new":
		return "◌"
	case "running":
		return "●"
	case "needs-review":
		return "⏸"
	case "needs-info":
		return "ℹ"
	case "blocked":
		return "✗"
	case "done":
		return "✓"
	case "cancelled":
		return "✘"
	default:
		return "·"
	}
}

// statusStyle 状态→颜色（spec §6）：new=暗灰, running=亮绿(+呼吸灯),
// needs-review=琥珀黄, needs-info=蓝, blocked=红, done=暗青, cancelled=暗灰。
func statusStyle(status string) lipgloss.Style {
	switch status {
	case "running":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("10")) // 亮绿
	case "needs-review":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("11")) // 琥珀黄
	case "needs-info":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("12")) // 蓝
	case "blocked":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("9")) // 红
	case "done":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("14")) // 暗青
	default:
		return lipgloss.NewStyle().Faint(true) // 暗灰（new / cancelled / 未知）
	}
}

// brightnessStyle 在 B4 之前用稳态（phase 不影响），B4 替换为真实明度插值。
// 占位：直接返回 base，使进行中任务先以稳态亮绿渲染。
func brightnessStyle(base lipgloss.Style, phase float64) lipgloss.Style { return base }

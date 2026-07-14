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

// brightnessStyle 按呼吸相位在亮绿与暗绿之间插值（spec §6 呼吸灯）。
func brightnessStyle(base lipgloss.Style, phase float64) lipgloss.Style {
	b := breathBrightness(phase)
	// 在暗绿(22)与亮绿(10)之间按 b 取色
	var c lipgloss.Color
	if b > 0.66 {
		c = lipgloss.Color("10") // 亮绿
	} else if b > 0.33 {
		c = lipgloss.Color("2")  // 中绿
	} else {
		c = lipgloss.Color("22") // 暗绿
	}
	return base.Foreground(c)
}

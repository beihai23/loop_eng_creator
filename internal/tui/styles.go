package tui

import (
	"os"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// init 在包加载时检测 NO_COLOR / 非 TTY，把 lipgloss 全局 color profile 切到
// termenv.Ascii（spec §6：关闭颜色与动画，退化为「符号 + 纯文本」）。
// Ascii profile 会剥离所有 ANSI 转义（颜色/bold/faint），状态符号 ●/◌/▸ 保留。
// 生产路径：dashboard 进程启动时本 init() 自动生效。测试不依赖本 init（init 在
// 测试进程启动时已跑，后续 os.Setenv 不会重触发），改为直接 SetColorProfile。
func init() {
	if os.Getenv("NO_COLOR") != "" || !isTTY() {
		lipgloss.SetColorProfile(termenv.Ascii)
	}
}

// isTTY 判断 stdout 是否为终端（字符设备）。非 TTY（管道/重定向）→ false。
func isTTY() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// IsTTY 导出 TTY 判断供 cli 包复用（dashboard 非 TTY 时不进 alt-screen）。
func IsTTY() bool { return isTTY() }

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
	case "needs-human-decision":
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
	case "needs-info", "needs-human-decision":
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
		c = lipgloss.Color("2") // 中绿
	} else {
		c = lipgloss.Color("22") // 暗绿
	}
	return base.Foreground(c)
}

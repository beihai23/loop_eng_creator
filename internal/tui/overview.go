package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// 列宽（显示单元格）。num/status 列固定宽，description 吃剩余宽度。
// marker 列宽 2（▶ / 空格），num 列容 "#999"，status 列容 "● needs-review"。
const (
	ovMarkerW = 2
	ovNumW    = 6
	ovStatusW = 14
)

// ovOverheadRows 是总览一帧里除任务行之外的固定行数：
// 标题 1 + 计数条 1 + 空行 1 + 表格上边框 1 + 列头 1 + 分隔线 1 + 下边框 1 +
// 空行 1 + 底栏按键提示 1 = 9。可视任务行数 = 终端高 - ovOverheadRows。
const ovOverheadRows = 9

// visibleRowsFor 按终端高度算可视任务行数（至少 1，保证选中行总能被渲染）。
func visibleRowsFor(height int) int {
	v := height - ovOverheadRows
	if v < 1 {
		v = 1
	}
	return v
}

// adjustOffset 调整滚动窗口起点，让 selIdx 落在 [offset, offset+visible) 内：
// selIdx 越过窗口上沿则上滚到 selIdx，越过下沿则下滚到 selIdx 贴底；
// 最后 clamp 到 [0, max(0, total-visible)]。纯函数，供 Update/View 共用。
func adjustOffset(selIdx, offset, visible, total int) int {
	if visible < 1 {
		visible = 1
	}
	if selIdx < offset {
		offset = selIdx
	}
	if selIdx >= offset+visible {
		offset = selIdx - visible + 1
	}
	maxOffset := total - visible
	if maxOffset < 0 {
		maxOffset = 0
	}
	if offset > maxOffset {
		offset = maxOffset
	}
	if offset < 0 {
		offset = 0
	}
	return offset
}

// RenderOverview 渲染 [1] 总览：标题 + 状态计数（符号即 legend）+ 边框表格
// （列头 №/状态/任务 + 分隔线 + 任务行）。纯函数（无 Store/终端/真时间）。
//
// 设计取舍（spec §6 + 可读性）：颜色只是锦上添花——边框/列头/分隔线/▶ 选中标记
// 这些**结构性**元素在 NO_COLOR/非 TTY 降级（Ascii profile）下也立得住，这才是
// 让「表格看起来可交互、字段不靠猜」的根。selIdx 是光标所在行（全局索引）；
// animPhase 进行中呼吸灯；w 终端宽度（列宽/盒宽/截断据此对齐）。
//
// 滚动：只渲染可视窗 snap.Tasks[offset:offset+visibleRows]（visibleRows 由
// visibleRowsFor(m.height) 算出）；offset/visibleRows 越界时在此兜底 clamp，
// 保证不传「渲染不出来」的参数也能得到合法窗口。
func RenderOverview(snap *Snapshot, selIdx, offset, visibleRows int, animPhase float64, w int) string {
	if w < 40 {
		w = 40 // 极窄终端兜底，保证列头不挤
	}
	innerW := w - 2 // 边框表格内容宽（左右各 1 个 │）；分隔线宽 = innerW 让 lipgloss 据此定盒宽
	descW := innerW - ovMarkerW - ovNumW - ovStatusW
	if descW < 8 {
		descW = 8
	}

	var b strings.Builder

	// ── 标题行 ──
	b.WriteString(lipgloss.NewStyle().Bold(true).Render("loop-eng dashboard"))
	b.WriteString("\n")

	// ── 状态计数行：每个段 = 符号 + 中文标签 + 计数，按状态着色；符号同时充当行的 legend ──
	b.WriteString(strings.Join([]string{
		countSegment("◌", "待处理", snap.Counts["new"], statusStyle("new")),
		countSegment("●", "进行中", snap.Counts["running"], statusStyle("running")),
		countSegment("⏸", "等人审", snap.Counts["needs-review"], statusStyle("needs-review")),
		countSegment("✗", "阻塞", snap.Counts["blocked"], statusStyle("blocked")),
		countSegment("✓", "完成", snap.Counts["done"], statusStyle("done")),
		countSegment("✘", "已取消", snap.Counts["cancelled"], statusStyle("cancelled")),
	}, "   "))
	b.WriteString("\n\n")

	// ── 边框表格：列头 + 分隔线 + 任务行 ──
	// 列头（bold，与行同列宽对齐：num/status 用 lipgloss.Width 按「显示宽度」补齐，
	// 兼容 CJK「状态」二字）。
	hdr := "  " +
		lipgloss.NewStyle().Bold(true).Width(ovNumW).Render("№") +
		lipgloss.NewStyle().Bold(true).Width(ovStatusW).Render("状态") +
		lipgloss.NewStyle().Bold(true).Render("任务")

	// 分隔线：宽 = innerW，是表格里最宽的行 → lipgloss 据此把整盒定到 innerW+2 = w。
	sep := strings.Repeat("─", innerW)

	// 可视窗：clamp 后只渲染 tasks[offset:end]（i 仍是全局索引，选中判定不受滚动影响）。
	total := len(snap.Tasks)
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}
	end := offset + visibleRows
	if end > total {
		end = total
	}
	if end < offset {
		end = offset
	}

	var rows []string
	for i := offset; i < end; i++ {
		t := snap.Tasks[i]
		sel := i == selIdx
		marker := strings.Repeat(" ", ovMarkerW)
		if sel {
			marker = "▶ " // 实心三角，比 ▸ 更显眼；Ascii 降级下仍保留
		}
		sym := statusSymbol(t.Status)
		numCol := fmt.Sprintf("%-*s", ovNumW, t.IssueRef)
		statusCol := fmt.Sprintf("%s %-*s", sym, ovStatusW-2, t.Status)
		// 描述列：最末列，按显示宽度截断（MaxWidth 兼顾 CJK，不会劈开双宽字符）。
		descCol := lipgloss.NewStyle().MaxWidth(descW).Render(t.Description)

		content := marker + numCol + statusCol + descCol
		// 整行按状态着色；running 整行呼吸（呼吸灯在「运行中的任务」上，不在顶部计数条）。
		rowSt := statusStyle(t.Status)
		if t.Status == "running" {
			rowSt = brightnessStyle(statusStyle("running"), animPhase)
		}
		line := rowSt.Render(content)
		if sel {
			// 整行反白：彩色 profile 下最显眼的「这一行被选中」信号；
			// Ascii 降级下 Reverse 被剥离，退回 ▶ 标记兜底。
			line = lipgloss.NewStyle().Reverse(true).Render(line)
		}
		rows = append(rows, line)
	}

	content := hdr + "\n" + sep
	if len(rows) > 0 {
		content += "\n" + strings.Join(rows, "\n")
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("8")).
		Render(content)
	b.WriteString(box)
	b.WriteString("\n\n")

	// ── 底栏：按键提示（键 bold + 描述 faint） ──
	b.WriteString(hintsLine())
	b.WriteString("\n")
	return b.String()
}

// countSegment 渲染计数条的一个段：符号 + 中文标签 + 计数，按给定样式（状态色）着色。
func countSegment(sym, label string, n int, st lipgloss.Style) string {
	return st.Render(fmt.Sprintf("%s %s %d", sym, label, n))
}

// hintsLine 渲染底栏按键提示：键名 bold + 描述 faint，段间分隔。
func hintsLine() string {
	pairs := []struct{ key, desc string }{
		{"↑↓", "选择"},
		{"Enter", "详情"},
		{"t", "轨迹"},
		{"r", "恢复"},
		{"x", "取消"},
		{"q", "退出"},
	}
	var segs []string
	for _, p := range pairs {
		segs = append(segs,
			lipgloss.NewStyle().Bold(true).Render(p.key)+
				lipgloss.NewStyle().Faint(true).Render(" "+p.desc))
	}
	return strings.Join(segs, "  ")
}

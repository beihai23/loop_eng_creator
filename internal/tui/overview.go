package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"loop-eng/internal/state"
)

// RenderOverview 渲染 [1] 总览：计数条 + 任务列表。纯函数（无 Store/终端/真时间）。
// animPhase 用于进行中任务的呼吸灯明度（B4 animate 计算后传入；此处接参但走稳态）。
// selIdx 是当前光标所在行；w 为终端宽度（预留截断用）。
func RenderOverview(snap *Snapshot, selIdx int, animPhase float64, w int) string {
	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Bold(true).Render("loop-eng dashboard"))
	b.WriteString("\n")

	// 计数条：各状态计数一览
	bar := fmt.Sprintf("待处理 %d · 进行中 %d · 等人审 %d · 阻塞 %d · 完成 %d · 已取消 %d",
		snap.Counts["new"], snap.Counts["running"], snap.Counts["needs-review"],
		snap.Counts["blocked"], snap.Counts["done"], snap.Counts["cancelled"])
	b.WriteString(lipgloss.NewStyle().Faint(true).Render(bar))
	b.WriteString("\n\n")

	// 任务列表：每行 = 选中标记 + 状态符号 + IssueRef + status + 描述
	for i, t := range snap.Tasks {
		sym := statusSymbol(t.Status)
		marker := "  "
		if i == selIdx {
			marker = "▸ "
		}
		line := fmt.Sprintf("%s%s %-12s %s  %s", marker, sym, t.IssueRef, t.Status, t.Description)
		st := statusStyle(t.Status)
		if t.Status == "running" {
			// 呼吸灯：animPhase 调亮度的变体（B4 提供 brightnessStyle 真实插值）
			st = brightnessStyle(st, animPhase)
		}
		b.WriteString(st.Render(line))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(lipgloss.NewStyle().Faint(true).Render("↑↓ 选  Enter 详情  t 轨迹  r resume  x cancel  q 退出"))
	b.WriteString("\n")
	_ = state.TaskView{}
	_ = w
	return b.String()
}

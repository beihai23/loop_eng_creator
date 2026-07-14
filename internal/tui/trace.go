package tui

import (
	"fmt"
	"strings"

	"loop-eng/internal/state"
)

// RenderTrace 渲染 [3] 轨迹：按 run 分组，组内 transitions + steps 分列。纯读 Store。
func RenderTrace(st *state.Store, taskID string) string {
	runs, _ := st.RunsOfTask(taskID)
	trans, _ := st.Transitions(taskID)
	var b strings.Builder
	b.WriteString(lipglossBold.Render("轨迹 #" + shortID(taskID)))
	b.WriteString("\n\n")

	for i, r := range runs {
		b.WriteString(fmt.Sprintf("run %d (%s → %s)\n", i+1, r.StartedAt, r.Outcome))
		// 该 run 段头下的 transitions（按 at）。v1 简化：transitions 跨 run，
		// 全部列在每个 run 段头下（spec §5[3] 接受；后续可按时间归入对应 run）。
		for _, tr := range trans {
			b.WriteString(fmt.Sprintf("  %s  %s → %s   %s\n", timeOnly(tr.At), tr.From, tr.To, tr.Reason))
		}
		// 该 run 的 steps（Replay 已带 At，B0）
		steps, _ := st.Replay(r.ID)
		for _, s := range steps {
			b.WriteString(fmt.Sprintf("  %s  ▸ %-8s %-10s %s\n", timeOnly(s.At), s.Role, s.Skill, s.Status))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// timeOnly 从 RFC3339 时间串里截出 HH:MM:SS 段（spec §5[3] trace 行首时间）。
func timeOnly(iso string) string {
	if len(iso) >= 19 {
		return iso[11:19] // RFC3339 的 HH:MM:SS
	}
	return iso
}

// shortID 截 id 前 8 字符用于显示（轨迹标题等）。
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

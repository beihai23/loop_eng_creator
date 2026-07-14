package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"loop-eng/internal/state"
)

// RenderTrace 渲染 [3] 轨迹：按 run 分组列 steps（plan/execute/verify …），末尾附
// 状态流转。纯读 Store。
//
// 双路径（修「runs 表为空时整页空白」的 bug）：
//   - runs 有记录（正常路径，走 StartRun/EndRun）：按 RunsOfTask 的每个 run 分组，
//     Replay(run.ID) 取该 run 的 steps，段头带 started → outcome。
//   - runs 为空（旧数据 / 未走 StartRun）：v1 约定 run_id==task_id，直接 Replay(taskID)
//     按 steps 重建轨迹。否则一个 step 都显示不出来——历史上自举的那批任务就栽在这。
//
// transitions 只在末尾列一次（修原版「每个 run 段头下重复列全部 transitions」的重复 bug）。
func RenderTrace(st *state.Store, taskID string) string {
	runs, _ := st.RunsOfTask(taskID)
	trans, _ := st.Transitions(taskID)

	type group struct {
		label string
		meta  *state.RunRow // nil = 无 runs 记录，按 steps 重建
		steps []state.StepRow
	}
	var groups []group
	if len(runs) > 0 {
		for i, r := range runs {
			steps, _ := st.Replay(r.ID)
			rr := r
			groups = append(groups, group{label: fmt.Sprintf("run %d", i+1), meta: &rr, steps: steps})
		}
	} else {
		// runs 表为空：按 v1 约定 run_id==task_id，直接捞 steps 重建。
		steps, _ := st.Replay(taskID)
		if len(steps) > 0 || len(trans) > 0 {
			groups = append(groups, group{label: "run · " + shortID(taskID), meta: nil, steps: steps})
		}
	}

	var b strings.Builder
	b.WriteString(lipglossBold.Render("轨迹 #" + shortID(taskID)))
	b.WriteString("\n\n")

	if len(groups) == 0 {
		b.WriteString(lipgloss.NewStyle().Faint(true).Render("（无轨迹记录）"))
		b.WriteString("\n")
		return b.String()
	}

	for _, g := range groups {
		// 按落盘时间排序：Replay 按 seq，但多次重试的 seq 会交错（03:xx→14:xx→03:xx），
		// 按 at 排才是一条可读的时间线。
		sort.SliceStable(g.steps, func(i, j int) bool { return g.steps[i].At < g.steps[j].At })
		var hdr string
		if g.meta != nil {
			hdr = fmt.Sprintf("%s（%s → %s）", g.label, timeOnly(g.meta.StartedAt), g.meta.Outcome)
		} else {
			hdr = g.label + "  （runs 表为空，按 steps 重建）"
		}
		b.WriteString(lipgloss.NewStyle().Faint(true).Render(hdr))
		b.WriteString("\n")
		for _, s := range g.steps {
			b.WriteString(stepLine(s))
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	// 状态流转：只列一次（不再每个 run 重复）。
	if len(trans) > 0 {
		b.WriteString(lipgloss.NewStyle().Bold(true).Render(fmt.Sprintf("状态流转（%d 次）", len(trans))))
		b.WriteString("\n")
		for _, tr := range trans {
			reason := ""
			if strings.TrimSpace(tr.Reason) != "" {
				reason = "   " + tr.Reason
			}
			line := fmt.Sprintf("  %s  %s → %s%s", timeOnly(tr.At), tr.From, tr.To, reason)
			b.WriteString(lipgloss.NewStyle().Faint(true).Render(line))
			b.WriteString("\n")
		}
	}
	return b.String()
}

// stepLine 渲染一行 step：时间 + ▸ role（·skill）+ 状态（按状态着色）。
func stepLine(s state.StepRow) string {
	role := strings.TrimSpace(s.Role)
	if sk := strings.TrimSpace(s.Skill); sk != "" {
		role = role + "·" + sk
	}
	left := fmt.Sprintf("  %s  ▸ %-12s ", timeOnly(s.At), role)
	return left + stepStatusStyle(s.Status).Render(s.Status)
}

// stepStatusStyle step 状态→颜色：ok=绿, fail/error=红, 其它=暗灰。
func stepStatusStyle(status string) lipgloss.Style {
	switch status {
	case "ok":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	case "fail", "error":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	default:
		return lipgloss.NewStyle().Faint(true)
	}
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

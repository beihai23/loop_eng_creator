package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"loop-eng/internal/state"
)

// RenderTrace 渲染 [3] 轨迹：结局头条 + 按「轮次（每次重试）」分卡片。
// 纯读 Store。目的是让人一眼看懂一个任务「最后怎么了、怎么走到这一步」——
// 不是 dump 原始事件日志。
//
// 数据来源（双路径，修「runs 表为空时整页空白」）：
//   - runs 有记录：每条 run = 一轮，steps 来自 Replay(run.ID)。
//   - runs 为空（旧数据 / 未走 StartRun）：v1 约定 run_id==task_id，按 transitions
//     里的「派发（→running）」切轮，steps 来自 Replay(taskID)。
//
// 转场去重：daemon 与 subloop 会双写同一次状态变更（reason="ran" 的回声、
// from="" 的派发回声），dedupeTransitions 去掉这些，并把长原因压成人话。
func RenderTrace(st *state.Store, taskID string) string {
	task, _ := st.GetTask(taskID)
	runs, _ := st.RunsOfTask(taskID)
	trans := dedupeTransitions(mustTransitions(st, taskID))

	// runs 非空：每条 run 的 steps 直接从 Replay(run.ID) 取（不做时间窗过滤，
	// 避开旧测试 step 无 At 的问题）。runs 为空：按 v1 约定 run_id==task_id，
	// 取全部 steps 再按派发时间窗分轮。
	runSteps := map[string][]state.StepRow{}
	for _, r := range runs {
		s, _ := st.Replay(r.ID)
		sort.SliceStable(s, func(i, j int) bool { return s[i].At < s[j].At })
		runSteps[r.ID] = s
	}
	globalSteps, _ := st.Replay(taskID)
	sort.SliceStable(globalSteps, func(i, j int) bool { return globalSteps[i].At < globalSteps[j].At })

	rounds := inferRounds(runs, trans, runSteps, globalSteps)

	var b strings.Builder
	b.WriteString(renderTraceHeadline(task, taskID, trans, rounds))
	b.WriteString("\n\n")

	if len(rounds) == 0 {
		b.WriteString(lipgloss.NewStyle().Faint(true).Render("（无运行记录）"))
		b.WriteString("\n")
		return b.String()
	}

	for i, r := range rounds {
		b.WriteString(renderRound(i+1, r))
		b.WriteString("\n")
	}
	return b.String()
}

// traceRound 是一次重试（一轮）的可展示视图。
type traceRound struct {
	startAt, endAt, outcomeAt string // 起止/结局时间（RFC3339）；endAt=下轮起点（用于 step 落窗），outcomeAt=真正结局时刻（用于显示）
	outcome                   string // 本轮结局态：done / blocked / error / needs-review / ...
	reason                    string // 结局原因（原始，渲染时压人话）；空=无
	steps                     []state.StepRow
}

// mustTransitions 读 transitions，容忍错误（返回空）——轨迹是只读展示，不应因读失败整体崩。
func mustTransitions(st *state.Store, taskID string) []state.TransitionRow {
	t, err := st.Transitions(taskID)
	if err != nil {
		return nil
	}
	return t
}

// dedupeTransitions 去掉 daemon/subloop 双写的重复转场：
//   - reason=="ran"：daemon 在 subloop 跑完补的回声（subloop 已用真因写过同一条）。
//   - reason=="dispatched" 且 from 为空：subloop 重复 daemon 的派发（真那条 from=new）。
//
// 再去掉与上一条完全相同的相邻重复（belt-and-suspenders）。
func dedupeTransitions(trans []state.TransitionRow) []state.TransitionRow {
	var out []state.TransitionRow
	for _, t := range trans {
		if t.Reason == "ran" {
			continue
		}
		if t.Reason == "dispatched" && strings.TrimSpace(t.From) == "" {
			continue
		}
		if len(out) > 0 {
			p := out[len(out)-1]
			if p.From == t.From && p.To == t.To && p.Reason == t.Reason {
				continue
			}
		}
		out = append(out, t)
	}
	return out
}

// inferRounds 推断轮次：runs 非空按 run 切（steps 用 runSteps[run.ID]，直接对应）；
// 否则按 transitions 的「派发（→running）」切，steps 按派发时间窗从 globalSteps 取。
// 每轮结局态/原因取该窗内 from=running 的那条转场。
func inferRounds(runs []state.RunRow, trans []state.TransitionRow, runSteps map[string][]state.StepRow, globalSteps []state.StepRow) []traceRound {
	if len(runs) > 0 {
		sorted := append([]state.RunRow(nil), runs...)
		sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].StartedAt < sorted[j].StartedAt })
		var rs []traceRound
		for _, r := range sorted {
			oc, reason, at := outcomeInWindow(trans, r.StartedAt, r.EndedAt)
			outcome := r.Outcome
			if outcome == "" {
				outcome = oc
			}
			oat := at
			if oat == "" {
				oat = r.EndedAt
			}
			rs = append(rs, traceRound{
				startAt:   r.StartedAt,
				endAt:     r.EndedAt,
				outcomeAt: oat,
				outcome:   outcome,
				reason:    reason,
				steps:     runSteps[r.ID],
			})
		}
		return rs
	}

	// runs 为空：按「→running」派发切轮。
	var starts []state.TransitionRow
	for _, t := range trans {
		if t.To == "running" && strings.TrimSpace(t.From) != "" {
			starts = append(starts, t)
		}
	}
	if len(starts) == 0 {
		return nil
	}
	var rs []traceRound
	for i, s := range starts {
		end := ""
		if i+1 < len(starts) {
			end = starts[i+1].At
		}
		oc, reason, at := outcomeInWindow(trans, s.At, end)
		rs = append(rs, traceRound{
			startAt:   s.At,
			endAt:     end,
			outcomeAt: at,
			outcome:   oc,
			reason:    reason,
			steps:     stepsInWindow(globalSteps, s.At, end),
		})
	}
	return rs
}

// stepsInWindow 返回 at ∈ [lo, hi) 的 steps（hi 空=上界无穷）。
func stepsInWindow(steps []state.StepRow, lo, hi string) []state.StepRow {
	var out []state.StepRow
	for _, s := range steps {
		if s.At < lo {
			continue
		}
		if hi != "" && s.At >= hi {
			continue
		}
		out = append(out, s)
	}
	return out
}

// outcomeInWindow 在 [lo,hi) 内找 from=running 的结局转场，返回 (结局态, 原始原因, 该转场 at)。
func outcomeInWindow(trans []state.TransitionRow, lo, hi string) (status, reason, at string) {
	for _, t := range trans {
		if t.At < lo {
			continue
		}
		if hi != "" && t.At >= hi {
			continue
		}
		if t.From == "running" && t.To != "running" {
			return t.To, t.Reason, t.At
		}
	}
	return "", "", ""
}

// ---- 渲染 ----

func renderTraceHeadline(task state.TaskRow, taskID string, trans []state.TransitionRow, rounds []traceRound) string {
	var b strings.Builder
	// 标题行：#issue_ref  描述
	ref := task.IssueRef
	if ref == "" {
		ref = shortID(taskID)
	}
	if !strings.HasPrefix(ref, "#") {
		ref = "#" + ref
	}
	desc := strings.TrimSpace(task.Description)
	title := ref
	if desc != "" {
		title += "  " + truncate(desc, 48)
	}
	b.WriteString(lipglossBold.Render(title))
	b.WriteString("\n")

	// 结局行：状态符号+中文 · N 轮 · 时间范围 · 阻塞时长
	status := currentStatus(trans, rounds)
	parts := []string{
		statusSymbol(status) + " " + statusVerbCN(status),
		fmt.Sprintf("%d 轮", len(rounds)),
	}
	if lo, hi, ok := span(rounds, trans); ok {
		parts = append(parts, fmt.Sprintf("%s → %s", clk(lo), clk(hi)))
		if d, ok := blockedDuration(trans); ok && d > 0 {
			parts = append(parts, "中途阻塞 "+durCN(d))
		}
	}
	b.WriteString(lipgloss.NewStyle().Faint(true).Render(strings.Join(parts, "  ·  ")))
	return b.String()
}

func renderRound(n int, r traceRound) string {
	var b strings.Builder
	// 段头：第 N 轮  HH:MM–HH:MM（显示用 outcomeAt=真正结局时刻，非下轮起点）
	end := r.outcomeAt
	if end == "" {
		end = r.endAt
	}
	hdr := fmt.Sprintf("第 %d 轮  %s", n, rangeClock(r.startAt, end))
	b.WriteString(lipgloss.NewStyle().Bold(true).Render(hdr))
	b.WriteString("\n")

	// step tokens：plan✓ execute✓ verify✗ …
	tokens := stepTokens(r.steps)
	if tokens != "" {
		b.WriteString("  ")
		b.WriteString(tokens)
	}
	// 结局：→ 状态：人话原因
	tail := "  → " + statusVerbCN(r.outcome)
	if r.outcome == "" {
		tail = "  → 进行中"
	}
	if reason := summarizeReason(r.reason, r.outcome); reason != "" && reason != statusVerbCN(r.outcome) {
		tail += "：" + reason
	}
	if r.outcome == "blocked" || r.outcome == "error" {
		b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Render(tail))
	} else if r.outcome == "done" {
		b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Render(tail))
	} else {
		b.WriteString(lipgloss.NewStyle().Faint(true).Render(tail))
	}
	b.WriteString("\n")
	return b.String()
}

// stepTokens 把一轮的 steps 渲染成 "plan✓ execute✓ verify✗" 序列（按时间序，状态着色）。
func stepTokens(steps []state.StepRow) string {
	var segs []string
	for _, s := range steps {
		role := strings.TrimSpace(s.Role)
		if role == "" {
			continue
		}
		mark, st := stepMark(s.Status)
		segs = append(segs, st.Render(role+mark))
	}
	return strings.Join(segs, " ")
}

func stepMark(status string) (string, lipgloss.Style) {
	switch status {
	case "ok":
		return "✓", lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	case "fail", "error":
		return "✗", lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	default:
		return "·", lipgloss.NewStyle().Faint(true)
	}
}

// summarizeReason 把原始 reason 压成人话短句：已知前缀精准映射，未知截断。
func summarizeReason(reason, toStatus string) string {
	r := strings.TrimSpace(reason)
	if r == "" || r == "ran" {
		return ""
	}
	low := strings.ToLower(r)
	switch {
	case strings.Contains(low, "401") || strings.Contains(low, "authenticat"):
		return "模型 401 认证失败"
	case strings.Contains(low, "429") || strings.Contains(low, "529") || strings.Contains(low, "访问量过大") || strings.Contains(low, "rate limit"):
		return "模型限流，稍后重试"
	case strings.Contains(low, "context canceled"):
		return "模型调用被取消"
	case strings.Contains(low, "retries exhausted"):
		return "重试耗尽"
	case strings.Contains(low, "tier3 auto-pass") || strings.Contains(low, "auto-pass"):
		return "tier-3 自动放行"
	case strings.Contains(low, "transient infra"):
		return "瞬时故障，已重排队"
	case strings.Contains(low, "resumed"):
		return "人工恢复"
	case strings.Contains(low, "reconcile"):
		return "对账调整"
	case strings.Contains(low, "exit status"):
		return "模型调用异常退出"
	}
	return truncate(r, 30)
}

// statusVerbCN 状态→中文动词短语（用于头条/卡片结局）。
func statusVerbCN(status string) string {
	switch status {
	case "new":
		return "待处理"
	case "running":
		return "进行中"
	case "needs-review":
		return "待人审"
	case "needs-info":
		return "待补充信息"
	case "needs-human-decision":
		return "待人工裁决"
	case "blocked":
		return "阻塞"
	case "done":
		return "完成"
	case "cancelled":
		return "已取消"
	case "error":
		return "出错"
	default:
		return status
	}
}

// currentStatus 取任务当前态：优先最后一条转场的 to；否则回落最后一轮的 outcome；
// 都没有则 new。（task_status 不在 TaskRow 上，无法直接取。）
func currentStatus(trans []state.TransitionRow, rounds []traceRound) string {
	if len(trans) > 0 {
		return trans[len(trans)-1].To
	}
	if len(rounds) > 0 {
		return rounds[len(rounds)-1].outcome
	}
	return "new"
}

// span 取整体时间范围（首条派发 at → 末条转场 at）。
func span(rounds []traceRound, trans []state.TransitionRow) (lo, hi string, ok bool) {
	if len(trans) == 0 {
		if len(rounds) > 0 && rounds[0].startAt != "" {
			return rounds[0].startAt, rounds[0].endAt, true
		}
		return "", "", false
	}
	lo = trans[0].At
	hi = trans[len(trans)-1].At
	return lo, hi, true
}

// blockedDuration 累计「阻塞」时长：每条 running→blocked 到下一条 from=blocked 之间的间隔。
func blockedDuration(trans []state.TransitionRow) (time.Duration, bool) {
	var total time.Duration
	for i, t := range trans {
		if t.From != "running" || t.To != "blocked" {
			continue
		}
		tb, ok1 := parseAt(t.At)
		if !ok1 {
			continue
		}
		for _, u := range trans[i+1:] {
			if u.From == "blocked" {
				if tr, ok2 := parseAt(u.At); ok2 {
					total += tr.Sub(tb)
				}
				break
			}
		}
	}
	return total, true
}

// ---- 时间小工具 ----

func parseAt(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, s)
	return t, err == nil
}

// clk 把 RFC3339 戳截成 HH:MM（不同天则带 MM-DD）。
func clk(iso string) string {
	t, ok := parseAt(iso)
	if !ok {
		return timeOnly(iso)
	}
	return t.Format("15:04")
}

// rangeClock 渲染 HH:MM–HH:MM（起止可能为空）。
func rangeClock(lo, hi string) string {
	if lo == "" && hi == "" {
		return ""
	}
	if hi == "" {
		return clk(lo) + "–…"
	}
	if lo == "" {
		return "…" + clk(hi)
	}
	return clk(lo) + "–" + clk(hi)
}

// durCN 把时长格式化成人话（~9h5m / ~5m / ~30s）。
func durCN(d time.Duration) string {
	d = d.Round(time.Minute)
	if d <= 0 {
		return ""
	}
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	switch {
	case h > 0 && m > 0:
		return fmt.Sprintf("~%dh%dm", h, m)
	case h > 0:
		return fmt.Sprintf("~%dh", h)
	default:
		return fmt.Sprintf("~%dm", m)
	}
}

// timeOnly 从 RFC3339 时间串里截出 HH:MM:SS 段（clk parse 失败时兜底用）。
func timeOnly(iso string) string {
	if len(iso) >= 19 {
		return iso[11:19]
	}
	return iso
}

// shortID 截 id 前 8 字符用于显示。
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

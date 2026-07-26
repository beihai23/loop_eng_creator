package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"loop-eng/internal/config"
	"loop-eng/internal/state"
)

// RenderDetail 渲染 [2] 详情（全屏）。读 Store 组装字段后渲染。
// 验收方式：tier-1 标签取 tier=1 verification 行 Detail 冒号前的真值（plan 产出）；tier-2 = cfg.Models.Verify.Name；tier-3 由 cfg.Verify.Tier3Human 开关。
// 逐 tier 状态来自 verifications 表（按 run+tier）。
// 预算/retry 从 budget_ledger 派生（runs 列恒为 0，Phase A 终审 I2）。
// M3：ActiveRun/VerificationsByRun/BudgetLedger 每帧各查一次（顶部捕获，逐 tier 扫内存切片）。
// run_id 取最新 run：优先 active run（in-flight），空时兜底 RunsOfTask 末尾——done/blocked 也
// 能展示逐 tier 状态与预算。
// I3：选中任务恰为 in-flight 时附 phase；run 有 retry 行时附 retry <次数>/<MaxRetries>。
func RenderDetail(st *state.Store, cfg *config.Config, taskID string) string {
	t, err := st.GetTask(taskID)
	if err != nil {
		return fmt.Sprintf("（任务 %s 不存在）\n", taskID)
	}

	// M3：顶部一次性捕获 run-scoped 数据，避免逐 tier 重复查询。
	// run_id：优先 active run（in-flight）；done/blocked 无 active run 时兜底到最新 run
	// （RunsOfTask 末尾），让验证/预算有数据可展。activeOK 单独记，避免给已结束 run 渲染
	// 「已运行」递增计时——elapsed 行只对真正的 active run 显示。
	runID, startedAt, activeOK, _ := st.ActiveRun(taskID)
	runOK := activeOK
	if !runOK {
		if runs, e := st.RunsOfTask(taskID); e == nil && len(runs) > 0 {
			last := runs[len(runs)-1]
			runID = last.ID
			startedAt = last.StartedAt
			runOK = true
		}
	}
	var vers []state.VerificationRow
	var budgetRows []state.BudgetRow
	if runOK {
		vers, _ = st.VerificationsByRun(runID)
		budgetRows, _ = st.BudgetLedger(runID)
	}

	var b strings.Builder
	b.WriteString(lipglossBold.Render(fmt.Sprintf("%s %s", t.IssueRef, t.Description)))
	b.WriteString("\n")

	// 状态行（I3：选中任务恰为 in-flight 时附 phase）
	statusLine := fmt.Sprintf("type: %s   状态: %s", t.TaskType, statusOfTask(st, taskID))
	if ifl, iflOK, _ := st.InFlight(); iflOK && ifl.TaskID == taskID {
		statusLine += "   phase: " + ifl.Phase
	}
	b.WriteString(statusLine + "\n")

	// 启动时间 / 运行时长：仅 in-flight 的 active run 才显示——已结束 run（done/blocked 兜底）
	// 不渲染「已运行」递增计时，避免对一个不再推进的 run 误读。
	if activeOK {
		b.WriteString(fmt.Sprintf("启动: %s · 已运行 %s\n", startedAt, elapsedSince(startedAt)))
	}

	// 初始提示词（最近一次启动 = runID 所指 run 的首轮 plan/execute 输入）。
	// 内容可能很长（含战报/issue 评论），详情 tab 支持 j/k 滚动查看（model.go）。
	if runOK {
		planPrompt, execPrompt, _ := st.InitialPrompts(runID)
		b.WriteString("\n" + detailHeader("初始提示词（最近一次启动）:") + "\n")
		// [plan]/[execute] 子标签 cyan+bold：在大量提示词墙内提供颜色锚点，定位两段输入边界。
		b.WriteString("  " + lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6")).Render("[plan]") + "\n")
		b.WriteString(indentBlock(planPrompt))
		b.WriteString("  " + lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6")).Render("[execute]") + "\n")
		b.WriteString(indentBlock(execPrompt))
	}

	// 验收方式（逐 tier 状态扫描内存切片 vers）
	// 列对齐用 lipgloss Width（tier 号 / label 固定宽截断 / 状态末列），不再空格凑——
	// CJK label（人审 / plan 未产出）也能对齐；状态按 ✓绿/✗红/—faint 着色（spec §6）。
	b.WriteString("\n" + detailHeader("验收方式:") + "\n")
	for _, tv := range verifyTiers(cfg, vers) {
		tierCol := lipgloss.NewStyle().Width(detailTierW).Render(fmt.Sprintf("tier-%d", tv.Tier))
		labelCol := lipgloss.NewStyle().Width(detailLabelW).Render(tv.Label)
		statusCol := tierStatusStyle(tv.Status).Render(tv.Status)
		b.WriteString("  " + tierCol + labelCol + statusCol + "\n")
	}

	// 验收标准
	b.WriteString("\n" + detailHeader("验收标准:") + "\n")
	for _, c := range t.Criteria {
		b.WriteString("  • " + c + "\n")
	}

	// retry（I3：budget_ledger 最后一条 retry 行；amount=当前次数，limit=MaxRetries）
	if runOK {
		for i := len(budgetRows) - 1; i >= 0; i-- {
			if budgetRows[i].Kind == "retry" {
				retryLimit := 0
				if cfg != nil {
					retryLimit = cfg.Budget.MaxRetries
				}
				b.WriteString("\n" + lipglossBold.Render(fmt.Sprintf("retry: %d/%d", budgetRows[i].Amount, retryLimit)) + "\n")
				break
			}
		}
	}

	// 预算（I2：used 从 budget_ledger 派生，limit = cfg.Budget.PerTaskTokens；
	// 接近上限时黄/红提示——见 budgetLine）
	used, limit := budgetUsed(st, cfg, taskID, runID)
	b.WriteString("\n" + budgetLine(used, limit) + "\n")
	// 按键提示已从 body 移除——由 model.View() 作 footer（detailHints）钉底，内容再长也
	// 无需滚到末尾才看到（issue 第 2 点）。
	return b.String()
}

// —— 辅助（同文件）——

var lipglossBold = lipglossNewBold()

// detailAccent 是详情页章节标题的 cyan accent（左侧 ▍ 色条）：TrueColor 下渲染
// \x1b[36m（与既有 Color("2")→\x1b[32m、Color("9")→\x1b[91m 同源映射）。它作为独立
// styled run 只渲染色条，标题本身仍走 lipglossBold——故 \x1b[1m 紧贴标题文本的既有契约
// 保留；Ascii 降级下颜色剥离、▍ 字符与标题文本不丢，大量文字中章节仍可辨识（issue 第 1 点）。
var detailAccent = lipgloss.NewStyle().Foreground(lipgloss.Color("6")) // cyan

// detailHeader 渲染章节标题：cyan accent 色条 + bold 标题。色条与标题是两个独立
// styled run，故 \x1b[1m 紧贴标题文本（detail_aligncolor_tier1 / detail_render 的
// bold-header 断言不变）。
func detailHeader(title string) string {
	return detailAccent.Render("▍ ") + lipglossBold.Render(title)
}

// detailHints 渲染详情页底栏按键提示（键 bold + 描述 faint，复刻 overview hintsLine 样式）。
// 独立于 RenderDetail 的滚动 body——View() 把它作 footer 钉在第 height 行，内容再长也
// 无需滚到末尾才看到（issue 第 2 点）。
func detailHints() string {
	pairs := []struct{ key, desc string }{
		{"j/k", "滚动"},
		{"r", "resume"},
		{"x", "cancel"},
		{"t", "轨迹"},
		{"Esc", "总览"},
	}
	var segs []string
	for _, p := range pairs {
		segs = append(segs,
			lipgloss.NewStyle().Bold(true).Render(p.key)+
				lipgloss.NewStyle().Faint(true).Render(" "+p.desc))
	}
	return strings.Join(segs, "  ")
}

// 验收方式 tier 行列宽（显示单元格）：tier 号 / label 固定宽（CJK 友好，超长按显示
// 宽截断）/ status 为末列不固定宽。用 lipgloss Width 对齐，不再空格凑（spec §6）。
const (
	detailTierW  = 8  // "tier-1".."tier-3"
	detailLabelW = 24 // label 列：plan 产出标签 / 模型名 / 人审
)

// indentBlock 把多行文本逐行缩进两格（对齐「验收标准」的 bullet 风格）；
// 空串（该 run 未落此 prompt）渲染占位符。
func indentBlock(s string) string {
	if s == "" {
		return "    （无记录）\n"
	}
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		b.WriteString("  " + line + "\n")
	}
	return b.String()
}

func statusOfTask(st *state.Store, taskID string) string {
	for _, r := range mustList(st) {
		if r.ID == taskID {
			return r.Status
		}
	}
	return "?"
}

type tierView struct {
	Tier   int
	Label  string
	Status string // "✓ passed" / "✗ <原因>" / "—"（未触发）
}

func verifyTiers(cfg *config.Config, vers []state.VerificationRow) []tierView {
	var out []tierView
	// tier-1 标签取 tier=1 verification 行 Detail 的真值（冒号前）；无 tier=1 行 → (plan 未产出)
	out = append(out, tierView{1, tier1Label(vers), symbolForTier(vers, 1)})
	// tier-2
	name := "LLM"
	if cfg != nil && cfg.Models.Verify.Name != "" {
		name = cfg.Models.Verify.Name
	}
	out = append(out, tierView{2, name + " (LLM diff)", symbolForTier(vers, 2)})
	// tier-3
	if cfg != nil && cfg.Verify.Tier3Human {
		out = append(out, tierView{3, "人审 (issue 评论)", symbolForTier(vers, 3)})
	}
	return out
}

// symbolForTier 扫描已捕获的内存切片 vers，按 tier 返回状态符号。
func symbolForTier(vers []state.VerificationRow, tier int) string {
	for _, v := range vers {
		if v.Tier == tier {
			if v.Passed {
				return "✓ passed"
			}
			return "✗ " + truncate(v.Detail, 40)
		}
	}
	return "—" // 未触发
}

// tierStatusStyle 按 tier 状态着色（与 overview statusStyle 同源调色板，spec §6）：
// ✓ passed → 绿、✗ <原因> → 红（同 blocked）、— 未触发 → faint（同 new/cancelled）。
// Ascii profile 下颜色剥离，符号 + 文本保留，结构/对齐不受影响。
func tierStatusStyle(status string) lipgloss.Style {
	switch {
	case strings.HasPrefix(status, "✓"):
		return lipgloss.NewStyle().Foreground(lipgloss.Color("2")) // 绿
	case strings.HasPrefix(status, "✗"):
		return lipgloss.NewStyle().Foreground(lipgloss.Color("9")) // 红（同 blocked）
	default:
		return lipgloss.NewStyle().Faint(true) // 未触发（同 new/cancelled）
	}
}

// budgetLine 渲染预算行：标签 bold；used/limit 按接近上限着色——≥90% 红、≥75% 琥珀黄、
// 否则默认（同 overview 调色板）。limit=0（未配上限）不着色。Ascii 下颜色剥离，数字保留。
func budgetLine(used, limit int) string {
	val := fmt.Sprintf(" %d / %d tokens", used, limit)
	st := lipgloss.NewStyle()
	if limit > 0 {
		ratio := float64(used) / float64(limit)
		switch {
		case ratio >= 0.9:
			st = st.Foreground(lipgloss.Color("9")) // 红
		case ratio >= 0.75:
			st = st.Foreground(lipgloss.Color("11")) // 琥珀黄
		}
	}
	return lipglossBold.Render("预算:") + st.Render(val)
}

// tier1Label 取 tier=1 verification 行 Detail 冒号前的真值——plan 产出的验收标签
// （deterministic.Check 落盘形如 "<label>: ok" / "<label>: <output>"）。无 tier=1 行
// （plan 未产出验收脚本）→ "(plan 未产出)"。
func tier1Label(vers []state.VerificationRow) string {
	for _, v := range vers {
		if v.Tier == 1 {
			return strings.TrimSpace(strings.SplitN(v.Detail, ":", 2)[0])
		}
	}
	return "(plan 未产出)"
}

// budgetUsed 累加 run 的 token 行得到 used；limit 取 cfg.Budget.PerTaskTokens（I2）。
func budgetUsed(st *state.Store, cfg *config.Config, taskID, runID string) (used, limit int) {
	if runID == "" {
		return 0, 0
	}
	rows, _ := st.BudgetLedger(runID)
	for _, r := range rows {
		if r.Kind == "tokens" {
			used += r.Amount
		}
	}
	limit = 0
	if cfg != nil {
		limit = cfg.Budget.PerTaskTokens
	}
	return used, limit
}

func elapsedSince(iso string) string {
	t, err := time.Parse(time.RFC3339Nano, iso)
	if err != nil {
		return "?"
	}
	return time.Since(t).Truncate(time.Second).String()
}

func truncate(s string, n int) string {
	rs := []rune(s)
	if len(rs) > n {
		return string(rs[:n]) + "…"
	}
	return s
}

// 把 lipgloss 调用收口（便于 B8 降级时一处改）
func lipglossNewBold() lipgloss.Style            { return lipgloss.NewStyle().Bold(true) }
func mustList(st *state.Store) []state.StatusRow { r, _ := st.ListStatuses(); return r }

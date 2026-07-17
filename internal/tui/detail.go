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

	// 验收方式（逐 tier 状态扫描内存切片 vers）
	b.WriteString("\n验收方式:\n")
	for _, tv := range verifyTiers(cfg, vers) {
		b.WriteString(fmt.Sprintf("  tier-%d  %-20s %s\n", tv.Tier, tv.Label, tv.StatusSym))
	}

	// 验收标准
	b.WriteString("\n验收标准:\n")
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
				b.WriteString(fmt.Sprintf("\nretry: %d/%d\n", budgetRows[i].Amount, retryLimit))
				break
			}
		}
	}

	// 预算（I2：used 从 budget_ledger 派生，limit = cfg.Budget.PerTaskTokens）
	used, limit := budgetUsed(st, cfg, taskID, runID)
	b.WriteString(fmt.Sprintf("\n预算: %d / %d tokens\n", used, limit))
	b.WriteString("\n[r] resume   [x] cancel   [t] 看轨迹   [Esc] 回总览\n")
	return b.String()
}

// —— 辅助（同文件）——

var lipglossBold = lipglossNewBold()

func statusOfTask(st *state.Store, taskID string) string {
	for _, r := range mustList(st) {
		if r.ID == taskID {
			return r.Status
		}
	}
	return "?"
}

type tierView struct {
	Tier      int
	Label     string
	StatusSym string
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

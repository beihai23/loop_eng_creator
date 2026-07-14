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
// 验收方式：tier-1 固定 go test；tier-2 = cfg.Models.Verify.Name；tier-3 由 cfg.Verify.Tier3Human 开关。
// 逐 tier 状态来自 verifications 表（按 run+tier）。
// 预算/retry 从 budget_ledger 派生（runs 列恒为 0，Phase A 终审 I2）。
// M3：ActiveRun/VerificationsByRun/BudgetLedger 每帧各查一次（顶部捕获，逐 tier 扫内存切片）。
// I3：选中任务恰为 in-flight 时附 phase；run 有 retry 行时附 retry <次数>/<MaxRetries>。
func RenderDetail(st *state.Store, cfg *config.Config, taskID string) string {
	t, err := st.GetTask(taskID)
	if err != nil {
		return fmt.Sprintf("（任务 %s 不存在）\n", taskID)
	}

	// M3：顶部一次性捕获 run-scoped 数据，避免逐 tier 重复查询。
	runID, startedAt, runOK, _ := st.ActiveRun(taskID)
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

	// 启动时间 / 运行时长（从 runs）
	if runOK {
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
	// tier-1 固定
	out = append(out, tierView{1, "go test ./...", symbolForTier(vers, 1)})
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

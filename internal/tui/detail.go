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
func RenderDetail(st *state.Store, cfg *config.Config, taskID string) string {
	t, err := st.GetTask(taskID)
	if err != nil {
		return fmt.Sprintf("（任务 %s 不存在）\n", taskID)
	}
	var b strings.Builder
	b.WriteString(lipglossBold.Render(fmt.Sprintf("#%s %s", t.IssueRef, t.Description)))
	b.WriteString("\n")
	b.WriteString(fmt.Sprintf("type: %s   状态: %s\n", t.TaskType, statusOfTask(st, taskID)))

	// 启动时间 / 运行时长（从 runs）
	if rid, started, ok, _ := st.ActiveRun(taskID); ok {
		b.WriteString(fmt.Sprintf("启动: %s · 已运行 %s\n", started, elapsedSince(started)))
		_ = rid
	}

	// 验收方式
	b.WriteString("\n验收方式:\n")
	tiers := verifyTiers(cfg, st, activeRunID(st, taskID))
	for _, tv := range tiers {
		b.WriteString(fmt.Sprintf("  tier-%d  %-20s %s\n", tv.Tier, tv.Label, tv.StatusSym))
	}

	// 验收标准
	b.WriteString("\n验收标准:\n")
	for _, c := range t.Criteria {
		b.WriteString("  • " + c + "\n")
	}

	// 预算（从 budget_ledger 派生）
	used, limit := budgetUsed(st, taskID)
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

func activeRunID(st *state.Store, taskID string) string {
	if rid, _, ok, _ := st.ActiveRun(taskID); ok {
		return rid
	}
	return ""
}

type tierView struct {
	Tier      int
	Label     string
	StatusSym string
}

func verifyTiers(cfg *config.Config, st *state.Store, runID string) []tierView {
	var out []tierView
	// tier-1 固定
	out = append(out, tierView{1, "go test ./...", symbolForTier(st, runID, 1)})
	// tier-2
	name := "LLM"
	if cfg != nil && cfg.Models.Verify.Name != "" {
		name = cfg.Models.Verify.Name
	}
	out = append(out, tierView{2, name + " (LLM diff)", symbolForTier(st, runID, 2)})
	// tier-3
	if cfg != nil && cfg.Verify.Tier3Human {
		out = append(out, tierView{3, "人审 (issue 评论)", symbolForTier(st, runID, 3)})
	}
	return out
}

func symbolForTier(st *state.Store, runID string, tier int) string {
	if runID == "" {
		return "—"
	}
	vs, _ := st.VerificationsByRun(runID)
	for _, v := range vs {
		if v.Tier == tier {
			if v.Passed {
				return "✓ passed"
			}
			return "✗ " + truncate(v.Detail, 40)
		}
	}
	return "—" // 未触发
}

func budgetUsed(st *state.Store, taskID string) (used, limit int) {
	rid := activeRunID(st, taskID)
	if rid == "" {
		return 0, 0
	}
	rows, _ := st.BudgetLedger(rid)
	for _, r := range rows {
		if r.Kind == "tokens" {
			used += r.Amount
		}
		if r.Kind == "retry" {
			limit = r.Limit // 复用 limit 字段放 retry 上限（展示另算）
		}
	}
	return used, 0 // per-call limit 不直接是总上限；展示用 used
}

func elapsedSince(iso string) string {
	t, err := time.Parse(time.RFC3339Nano, iso)
	if err != nil {
		return "?"
	}
	return time.Since(t).Truncate(time.Second).String()
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// 把 lipgloss 调用收口（便于 B8 降级时一处改）
func lipglossNewBold() lipgloss.Style            { return lipgloss.NewStyle().Bold(true) }
func mustList(st *state.Store) []state.StatusRow { r, _ := st.ListStatuses(); return r }

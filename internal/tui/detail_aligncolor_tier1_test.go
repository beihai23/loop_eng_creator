package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"loop-eng/internal/config"
	"loop-eng/internal/state"
)

// TestTier1DetailAlignColor 黑盒断言 RenderDetail 的 tier 配色 + 列对齐 + NO_COLOR 降级契约。
// 不引用 detailTone/tonePass 等内部符号（只断言对外输出），避免上轮 test 引用未实现符号致 build failed。
func TestTier1DetailAlignColor(t *testing.T) {
	st, _ := state.Open(t.TempDir() + "/state.db")
	defer st.Close()
	tid, _ := st.InsertTask(state.TaskRow{IssueRef: "#5", Description: "detail align+color", TaskType: "feature", Criteria: []string{"c1"}})
	rid, _ := st.StartRun(tid)
	_ = st.AppendVerification(rid, 2, false, "glm-5.2: diff unrelated")
	_ = st.AppendVerification(rid, 3, true, "human: ok")
	// 无 tier-1 行 → tier-1 渲染 — (neutral)，三种 tone 全覆盖
	cfg := &config.Config{}
	cfg.Models.Verify = config.ModelRef{Name: "glm-5.2"}
	cfg.Verify.Tier3Human = true

	prev := lipgloss.ColorProfile()
	defer lipgloss.SetColorProfile(prev)

	// 彩色：tier 行着色 → 含 ANSI 转义。
	lipgloss.SetColorProfile(termenv.TrueColor)
	colored := RenderDetail(st, cfg, tid)
	if !strings.Contains(colored, "\x1b[") || !strings.Contains(colored, "✓ passed") {
		t.Fatalf("TrueColor tier 行应着色且含 ✓ passed: %q", colored)
	}

	// Ascii 降级：颜色剥离，符号/文本仍在。
	lipgloss.SetColorProfile(termenv.Ascii)
	out := RenderDetail(st, cfg, tid)
	if strings.Contains(out, "\x1b[") {
		t.Fatalf("Ascii 下不应含 ANSI: %q", out)
	}
	for _, want := range []string{"✓ passed", "✗", "—"} {
		if !strings.Contains(out, want) {
			t.Fatalf("Ascii 下 %q 丢失: %q", want, out)
		}
	}

	// 列对齐不变量：每个 tier 行状态符号所在『显示列』相同（CJK label 也对齐）。
	var cols []int
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "tier-") {
			continue
		}
		idx := -1
		for _, g := range []string{"✓", "✗", "—"} {
			if j := strings.Index(line, g); j >= 0 && (idx < 0 || j < idx) {
				idx = j
			}
		}
		if idx < 0 {
			t.Fatalf("tier 行缺状态符号: %q", line)
		}
		cols = append(cols, lipgloss.Width(line[:idx]))
	}
	if len(cols) < 2 {
		t.Fatalf("tier 行不足: %v", cols)
	}
	for i, c := range cols {
		if c != cols[0] {
			t.Fatalf("tier 状态符号未列对齐 cols=%v 行%d 列%d≠%d: %q", cols, i, c, cols[0], out)
		}
	}
}

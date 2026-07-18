package tui

import (
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"loop-eng/internal/config"
	"loop-eng/internal/state"
)

// buildDetailFixture 灌一个 done 任务 + 三层验证（tier-1/3 过、tier-2 挂），
// 让「验收方式」三行齐全（含 CJK 的 tier-3 人审 label）供渲染断言。纯函数注入：
// RenderDetail 只吃 Store + Config，无终端/真时间依赖。
func buildDetailFixture(t *testing.T) (*state.Store, *config.Config, string) {
	t.Helper()
	st, _ := state.Open(t.TempDir() + "/state.db")
	t.Cleanup(func() { st.Close() })
	tid, _ := st.InsertTask(state.TaskRow{
		IssueRef:    "#18",
		Description: "给 budget 加硬上限",
		TaskType:    "feature",
		Criteria:    []string{"命中上限立即停", "落盘 budget_ledger"},
	})
	rid, _ := st.StartRun(tid)
	_ = st.AppendVerification(rid, 1, true, "go-test: ok")
	_ = st.AppendVerification(rid, 2, false, "glm-5.2: diff unrelated to criteria")
	_ = st.AppendVerification(rid, 3, true, "人审: ok")
	if err := st.EndRun(rid, "done"); err != nil {
		t.Fatalf("EndRun: %v", err)
	}
	cfg := &config.Config{}
	cfg.Models.Verify = config.ModelRef{Name: "glm-5.2"}
	cfg.Verify.Tier3Human = true
	cfg.Budget.PerTaskTokens = 100000
	return st, cfg, tid
}

// tierLines 抽出「验收方式」段的 tier 行（"  tier-N ..."）。
func tierLines(out string) []string {
	var ls []string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "tier-") {
			ls = append(ls, l)
		}
	}
	return ls
}

// statusSymDispCol 返回 tier 行内状态符号（✓/✗/—）所在的显示列宽。
// 用 lipgloss.Width 量前缀，CJK label 也按显示宽计入——对齐的正确性据此断言。
func statusSymDispCol(line string) int {
	for _, sym := range []string{"✗", "✓", "—"} {
		if i := strings.Index(line, sym); i >= 0 {
			return lipgloss.Width(line[:i])
		}
	}
	return -1
}

// TestRenderDetailVerifyColumnsAlign 钉死验收方式 tier 行的列对齐：tier 号 / label
// 固定宽 / 状态末列，用 lipgloss Width 不再空格凑。关键：CJK label（tier-3 人审）
// 与纯 ASCII label（tier-1 go-test）状态符号必须落在同一显示列。
func TestRenderDetailVerifyColumnsAlign(t *testing.T) {
	st, cfg, tid := buildDetailFixture(t)
	out := RenderDetail(st, cfg, tid) // 默认 Ascii profile（测试进程非 TTY）

	ls := tierLines(out)
	if len(ls) != 3 {
		t.Fatalf("want 3 tier lines, got %d: %q", len(ls), ls)
	}
	wantCol := 2 + detailTierW + detailLabelW // 缩进 2 + tier 列 + label 列
	for _, l := range ls {
		got := statusSymDispCol(l)
		if got != wantCol {
			t.Fatalf("tier 行状态符号未对齐: want 显示列 %d, got %d\n%s", wantCol, got, l)
		}
	}
	// label 列固定宽 → tier-2 的长 reason 不影响后续行对齐（已被 Ascii 快照印证）
}

// TestRenderDetailColorsTrueColor TrueColor profile 下：状态按 ✓绿 / ✗红 着色，
// 段标题 bold，且三层 tier 状态彼此颜色可区分（绿 ≠ 红）。
func TestRenderDetailColorsTrueColor(t *testing.T) {
	st, cfg, tid := buildDetailFixture(t)
	prev := lipgloss.ColorProfile()
	defer lipgloss.SetColorProfile(prev)
	lipgloss.SetColorProfile(termenv.TrueColor)

	out := RenderDetail(st, cfg, tid)

	// 彩色 profile 下确有 ANSI 转义（降级前的 RED 状态控制）
	if !strings.Contains(out, "\x1b[") {
		t.Fatalf("TrueColor 下应含 ANSI 转义: %q", out)
	}
	// 段标题 bold（验收方式/验收标准）
	for _, h := range []string{"\x1b[1m验收方式", "\x1b[1m验收标准"} {
		if !strings.Contains(out, h) {
			t.Fatalf("want bold header %q in:\n%s", h, out)
		}
	}
	// ✓-tier 用绿（\x1b[32m）、✗-tier 用红（\x1b[91m）——与 overview 调色板一致
	var passLine, failLine string
	for _, l := range tierLines(out) {
		switch {
		case strings.Contains(l, "✓"):
			passLine = l
		case strings.Contains(l, "✗"):
			failLine = l
		}
	}
	if passLine == "" || failLine == "" {
		t.Fatalf("缺 ✓/✗ tier 行:\n%s", out)
	}
	if !strings.Contains(passLine, "\x1b[32m") {
		t.Fatalf("✓ passed 应为绿(\\x1b[32m): %q", passLine)
	}
	if !strings.Contains(failLine, "\x1b[91m") {
		t.Fatalf("✗ <原因> 应为红(\\x1b[91m): %q", failLine)
	}
}

// TestRenderDetailPendingTierFaint tier 启用但无 verification 行（未触发）→ 「—」，
// 着色既非绿也非红（faint 暗灰，同 new/cancelled）。
func TestRenderDetailPendingTierFaint(t *testing.T) {
	st, _ := state.Open(t.TempDir() + "/state.db")
	t.Cleanup(func() { st.Close() })
	tid, _ := st.InsertTask(state.TaskRow{IssueRef: "#7", Description: "tier 未触发", TaskType: "bug"})
	rid, _ := st.StartRun(tid)
	_ = st.AppendVerification(rid, 1, true, "go-test: ok") // 仅 tier-1 落盘
	_ = st.EndRun(rid, "done")
	cfg := &config.Config{}
	cfg.Models.Verify = config.ModelRef{Name: "glm-5.2"}
	cfg.Verify.Tier3Human = true // tier-2/tier-3 启用但无行 → —

	prev := lipgloss.ColorProfile()
	defer lipgloss.SetColorProfile(prev)
	lipgloss.SetColorProfile(termenv.TrueColor)

	out := RenderDetail(st, cfg, tid)
	pending := 0
	for _, l := range tierLines(out) {
		if !strings.Contains(l, "—") {
			continue
		}
		pending++
		if strings.Contains(l, "\x1b[32m") || strings.Contains(l, "\x1b[91m") {
			t.Fatalf("「—」未触发行不应绿/红: %q", l)
		}
	}
	if pending == 0 {
		t.Fatalf("缺未触发(—) tier 行:\n%s", out)
	}
}

// TestRenderDetailNoColorDegrades Ascii（NO_COLOR/非 TTY）降级契约：剥离全部 ANSI，
// 但符号（✓/✗/—）+ 文本 + 列对齐结构仍在（spec §6）。
func TestRenderDetailNoColorDegrades(t *testing.T) {
	st, cfg, tid := buildDetailFixture(t)
	prev := lipgloss.ColorProfile()
	defer lipgloss.SetColorProfile(prev)
	lipgloss.SetColorProfile(termenv.Ascii)

	out := RenderDetail(st, cfg, tid)
	if strings.Contains(out, "\x1b[") {
		t.Fatalf("Ascii 下仍含 ANSI 转义: %q", out)
	}
	// 符号 + 关键文本保留（本 fixture 三层均已落盘 → ✓/✗；「—」由 PendingTierFaint 覆盖）
	for _, want := range []string{"✓ passed", "✗", "人审", "go-test", "验收方式:", "验收标准:"} {
		if !strings.Contains(out, want) {
			t.Fatalf("Ascii 下 %q 丢失:\n%s", want, out)
		}
	}
	// 结构 + 对齐仍在：状态符号同列
	wantCol := 2 + detailTierW + detailLabelW
	for _, l := range tierLines(out) {
		if got := statusSymDispCol(l); got != wantCol {
			t.Fatalf("Ascii 下对齐破坏: want %d got %d\n%s", wantCol, got, l)
		}
	}
}

// TestRenderDetailSnapshot 验收方式段的关键串快照：三层 label + 状态符号 + 段标题齐备。
func TestRenderDetailSnapshot(t *testing.T) {
	st, cfg, tid := buildDetailFixture(t)
	out := RenderDetail(st, cfg, tid)
	for _, want := range []string{
		"验收方式:", "tier-1", "go-test", "✓ passed",
		"tier-2", "glm-5.2 (LLM diff)", "✗",
		"tier-3", "人审 (issue 评论)",
		"验收标准:", "命中上限立即停", "落盘 budget_ledger",
		"预算:", "100000",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("snapshot 缺 %q:\n%s", want, out)
		}
	}
}

// TestRenderDetailBudgetNearLimitColor 预算接近上限的黄/红提示（可选功能）：
// ≥90% 红、≥75% 琥珀黄、否则默认不着色。termenv TrueColor 把调色板索引映射到亮色档
// （9→\x1b[91m 亮红、11→\x1b[93m 亮黄，与 overview statusStyle 同源）。
func TestRenderDetailBudgetNearLimitColor(t *testing.T) {
	prev := lipgloss.ColorProfile()
	defer lipgloss.SetColorProfile(prev)
	lipgloss.SetColorProfile(termenv.TrueColor)

	cases := []struct {
		used    int
		wantSeq string // 期望出现的 ANSI 前景序列（亮黄/亮红）；空 → 不应有警告色
	}{
		{95000, "\x1b[91m"}, // 95% → 红
		{80000, "\x1b[93m"}, // 80% → 琥珀黄
		{1000, ""},          // 1% → 默认无警告色
	}
	for _, c := range cases {
		st, _ := state.Open(t.TempDir() + "/state.db")
		tid, _ := st.InsertTask(state.TaskRow{IssueRef: "#b", Description: "budget", TaskType: "feature"})
		rid, _ := st.StartRun(tid) // active run 即可展示预算，无需 EndRun
		_ = st.AppendBudget(rid, "call", "tokens", c.used, 100000)
		cfg := &config.Config{}
		cfg.Budget.PerTaskTokens = 100000

		out := RenderDetail(st, cfg, tid)
		// 预算数字始终在（着色包裹不影响子串匹配）
		if !strings.Contains(out, "100000") || !strings.Contains(out, strconv.Itoa(c.used)) {
			t.Fatalf("预算数字丢失 used=%d:\n%s", c.used, out)
		}
		if c.wantSeq == "" {
			// 低占比：预算行不应带亮黄/亮红
			if strings.Contains(out, "\x1b[91m") || strings.Contains(out, "\x1b[93m") {
				t.Fatalf("低占比预算不应着警告色 used=%d:\n%s", c.used, out)
			}
			continue
		}
		if !strings.Contains(out, c.wantSeq) {
			t.Fatalf("预算 used=%d 应含 %q:\n%s", c.used, c.wantSeq, out)
		}
		st.Close()
	}
}

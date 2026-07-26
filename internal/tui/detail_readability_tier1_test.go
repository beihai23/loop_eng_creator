package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"loop-eng/internal/config"
	"loop-eng/internal/state"
)

// TestTier1DetailHintsPinned 钉死 issue 第 2 点：按键提示钉底。
// RenderDetail 的 body 不再含 hints（拆到独立 footer detailHints）；
// 内容远高于窗口时，View() 滚到底仍渲染 footer——无需滚到末尾才看到。
func TestTier1DetailHintsPinned(t *testing.T) {
	st, _ := state.Open(t.TempDir() + "/state.db")
	defer st.Close()
	tid, _ := st.InsertTask(state.TaskRow{IssueRef: "#h", Description: "tall detail", TaskType: "feature", Criteria: []string{"c1"}})
	rid, _ := st.StartRun(tid)
	var plines []string
	for i := 0; i < 60; i++ {
		plines = append(plines, "prompt line")
	}
	_ = st.AppendStep(state.StepRow{RunID: rid, Seq: 11, Role: "plan", Status: "ok", InputJSON: strings.Join(plines, "\n")})

	body := RenderDetail(st, &config.Config{}, tid)
	if strings.Contains(body, "滚动") || strings.Contains(body, "回总览") {
		t.Fatalf("RenderDetail body 不应再含 hints")
	}
	hints := detailHints()
	if !strings.Contains(hints, "滚动") || !strings.Contains(hints, "Esc") {
		t.Fatalf("detailHints 缺 滚动/Esc: %q", hints)
	}

	m := New(st, nil)
	m.selTask = tid
	m.tab = tabDetail
	m.height = 10
	key := func(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }
	var cur tea.Model = m
	for i := 0; i < 200; i++ {
		cur, _ = cur.(Model).Update(key("j"))
	}
	view := cur.(Model).View()
	if !strings.Contains(view, "滚动") || !strings.Contains(view, "Esc") {
		t.Fatalf("滚到底 View() 仍应钉底按键提示")
	}
}

// TestTier1DetailSectionAccent 钉死 issue 第 1 点：章节颜色 + 明暗区分。
// TrueColor 下章节标题行带 cyan accent（\x1b[36m），且既有 bold 紧贴标题契约保留；
// Ascii 降级下颜色剥离、标题文本不丢——大量文字下章节仍可辨识。
func TestTier1DetailSectionAccent(t *testing.T) {
	st, cfg, tid := buildDetailFixture(t)
	prev := lipgloss.ColorProfile()
	defer lipgloss.SetColorProfile(prev)
	esc := string(rune(27))

	lipgloss.SetColorProfile(termenv.TrueColor)
	colored := RenderDetail(st, cfg, tid)
	for _, h := range []string{"验收方式", "验收标准"} {
		var line string
		for _, l := range strings.Split(colored, "\n") {
			if strings.Contains(l, h) && !strings.Contains(l, "tier-") {
				line = l
				break
			}
		}
		if line == "" {
			t.Fatalf("TrueColor 缺章节标题 %q", h)
		}
		if !strings.Contains(line, esc+"[1m"+h) {
			t.Fatalf("标题 %q 应保留 bold 紧贴: %q", h, line)
		}
		if !strings.Contains(line, esc+"[36m") {
			t.Fatalf("标题 %q 行缺 cyan accent: %q", h, line)
		}
	}

	lipgloss.SetColorProfile(termenv.Ascii)
	asc := RenderDetail(st, cfg, tid)
	if strings.Contains(asc, esc+"[") {
		t.Fatalf("Ascii 不应含 ANSI")
	}
	for _, want := range []string{"验收方式:", "验收标准:"} {
		if !strings.Contains(asc, want) {
			t.Fatalf("Ascii 缺 %q", want)
		}
	}
}

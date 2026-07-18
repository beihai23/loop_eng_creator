package tui

import (
	"strings"
	"testing"

	"loop-eng/internal/state"
)

// tier-1: TasksByStatus 同状态组内按 created_at 倒序（新 → 旧），与入库序、
// issue 号都无关。
func TestTier1TasksByStatusNewestFirst(t *testing.T) {
	s, err := state.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// 故意按「既非时间序也非 issue 号序」的顺序入库。
	inserts := []struct{ ref, createdAt string }{
		{"11", "2026-07-17T09:00:00Z"},
		{"10", "2026-07-18T08:00:00Z"}, // 最新
		{"14", "2026-07-16T09:00:00Z"}, // 最旧
		{"13", "2026-07-17T10:00:00Z"},
	}
	for _, in := range inserts {
		if _, err := s.InsertTask(state.TaskRow{IssueRef: in.ref, Description: "d" + in.ref, CreatedAt: in.createdAt}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.TasksByStatus()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"10", "13", "11", "14"} // 新 → 旧
	if len(got) != len(want) {
		t.Fatalf("len=%d want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].IssueRef != w {
			t.Fatalf("order[%d]=%q want %q (full: %+v)", i, got[i].IssueRef, w, got)
		}
	}
}

// tier-1: visibleRowsFor 按终端高度算可视行数（overhead 常量 9，钳下限 1）。
func TestTier1VisibleRowsFor(t *testing.T) {
	if got := visibleRowsFor(20); got != 11 {
		t.Fatalf("visibleRowsFor(20)=%d want 11", got)
	}
	if got := visibleRowsFor(0); got != 1 {
		t.Fatalf("visibleRowsFor(0)=%d want 1 (clamped)", got)
	}
	if got := visibleRowsFor(5); got != 1 {
		t.Fatalf("visibleRowsFor(5)=%d want 1 (clamped)", got)
	}
}

// tier-1: adjustOffset 让 selIdx 始终落在可视窗内。
func TestTier1AdjustOffset(t *testing.T) {
	if got := adjustOffset(15, 0, 11, 30); got != 5 {
		t.Fatalf("down past bottom: adjustOffset(15,0,11,30)=%d want 5", got)
	}
	if got := adjustOffset(3, 5, 11, 30); got != 3 {
		t.Fatalf("up past top: adjustOffset(3,5,11,30)=%d want 3", got)
	}
	if got := adjustOffset(5, 5, 11, 30); got != 5 {
		t.Fatalf("in view: adjustOffset(5,5,11,30)=%d want 5 (unchanged)", got)
	}
	if got := adjustOffset(29, 5, 11, 30); got != 19 {
		t.Fatalf("last row: adjustOffset(29,5,11,30)=%d want 19 (=total-visibleRows)", got)
	}
	if got := adjustOffset(2, 0, 11, 3); got != 0 {
		t.Fatalf("total<visibleRows: adjustOffset(2,0,11,3)=%d want 0", got)
	}
}

// tier-1: Model.View 只渲染可视窗，选中行越界时自动滚动、选中行可见。
func TestTier1OverviewViewScrolls(t *testing.T) {
	tasks := make([]state.TaskView, 30)
	for i := range tasks {
		tasks[i] = state.TaskView{ID: "id", IssueRef: strings.TrimSpace(strings.Repeat(" ", 0)) + itoa(i), Description: "task-" + pad2(i), Status: "new"}
	}
	m := Model{
		tab:    tabOverview,
		width:  80,
		height: 20, // visibleRows = 11
		selIdx: 25,
		offset: 0, // 故意留一个过期 offset，View 应自行纠正
		snap:   &Snapshot{Tasks: tasks, Counts: map[string]int{"new": 30}},
	}
	out := m.View()
	if !strings.Contains(out, "task-25") {
		t.Fatalf("selected row task-25 must be visible:\n%s", out)
	}
	if strings.Contains(out, "task-00") || strings.Contains(out, "task-01") {
		t.Fatalf("rows above window must not render:\n%s", out)
	}
	if strings.Contains(out, "task-26") || strings.Contains(out, "task-29") {
		t.Fatalf("rows below window must not render:\n%s", out)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [8]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	return string(b[p:])
}

func pad2(i int) string {
	s := itoa(i)
	if len(s) == 1 {
		return "0" + s
	}
	return s
}

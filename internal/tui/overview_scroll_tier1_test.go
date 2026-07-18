package tui

import (
	"strings"
	"testing"

	"loop-eng/internal/state"
)

// tier-1: TasksByStatus 组内按 issue 号数值序（不是入库序）。
func TestTier1TasksByStatusNumericOrder(t *testing.T) {
	s, err := state.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, ref := range []string{"11", "10", "14", "13"} {
		if _, err := s.InsertTask(state.TaskRow{IssueRef: ref, Description: "d" + ref}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.TasksByStatus()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"10", "11", "13", "14"}
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

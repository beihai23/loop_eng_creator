package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"loop-eng/internal/state"
)

// TestVisibleRowsFor 校验可视行数 = 终端高 - 固定开销（ovOverheadRows），
// 且极小终端（高度不足开销）兜底为 1，保证选中行总能被渲染。
func TestVisibleRowsFor(t *testing.T) {
	if got := visibleRowsFor(ovOverheadRows + 5); got != 5 {
		t.Fatalf("visibleRowsFor(%d)=%d want 5", ovOverheadRows+5, got)
	}
	if got := visibleRowsFor(ovOverheadRows); got != 1 {
		t.Fatalf("visibleRowsFor(%d)=%d want 1（兜底）", ovOverheadRows, got)
	}
	if got := visibleRowsFor(0); got != 1 {
		t.Fatalf("visibleRowsFor(0)=%d want 1（兜底）", got)
	}
}

// TestAdjustOffset 校验 selIdx 越出可视窗时 offset 的滚动规则：
// 越下沿 → 下滚到 selIdx 贴窗口底；越上沿 → 上滚到 selIdx 贴窗口顶；
// 窗口内不动；末尾 clamp 到 max(0, total-visible)。
func TestAdjustOffset(t *testing.T) {
	cases := []struct {
		name                           string
		selIdx, offset, visible, total int
		want                           int
	}{
		{"窗口内不动", 4, 2, 5, 20, 2},
		{"越下沿下滚", 9, 0, 5, 20, 5},       // selIdx 9 → offset = 9-5+1
		{"越上沿上滚", 2, 5, 5, 20, 2},       // selIdx 2 < offset 5 → 贴顶
		{"贴底 clamp", 19, 30, 5, 20, 15}, // offset 最大 = total-visible
		{"列表短于窗口归零", 2, 3, 10, 5, 0},
		{"负 offset 归零", 0, -3, 5, 20, 0},
	}
	for _, c := range cases {
		if got := adjustOffset(c.selIdx, c.offset, c.visible, c.total); got != c.want {
			t.Fatalf("%s: adjustOffset(%d,%d,%d,%d)=%d want %d",
				c.name, c.selIdx, c.offset, c.visible, c.total, got, c.want)
		}
	}
}

// scrollSnap 造 n 个 new 任务的快照（IssueRef/描述带序号，便于断言可见性）。
func scrollSnap(n int) *Snapshot {
	tasks := make([]state.TaskView, n)
	for i := range tasks {
		tasks[i] = state.TaskView{
			ID:          fmt.Sprintf("t%02d", i),
			IssueRef:    fmt.Sprintf("%d", i+1),
			Description: fmt.Sprintf("task-%02d", i),
			Status:      "new",
		}
	}
	return &Snapshot{Tasks: tasks, Counts: map[string]int{"new": n}}
}

// TestRenderOverviewWindowsTasks 校验只渲染可视窗 tasks[offset:offset+visibleRows]：
// 窗口内的描述出现、窗口外的不出现；选中行（全局索引）在窗口内带 ▶ 且仅一次。
func TestRenderOverviewWindowsTasks(t *testing.T) {
	snap := scrollSnap(20)
	// 窗口 = tasks[7:12]，选中全局 idx 9（task-09）
	out := RenderOverview(snap, 9, 7, 5, 0.0, 100)

	for _, d := range []string{"task-07", "task-08", "task-09", "task-10", "task-11"} {
		if !strings.Contains(out, d) {
			t.Fatalf("窗口内 %s 未渲染:\n%s", d, out)
		}
	}
	for _, d := range []string{"task-00", "task-06", "task-12", "task-19"} {
		if strings.Contains(out, d) {
			t.Fatalf("窗口外 %s 不应渲染:\n%s", d, out)
		}
	}
	if strings.Count(out, "▶") != 1 {
		t.Fatalf("选中标记应恰好一次, got %d:\n%s", strings.Count(out, "▶"), out)
	}

	// offset/visibleRows 越界兜底：窗口超尾部时 clamp 到列表末尾（tasks[18:20]），不崩
	out2 := RenderOverview(snap, 19, 18, 10, 0.0, 100)
	if !strings.Contains(out2, "task-19") || !strings.Contains(out2, "task-18") {
		t.Fatalf("尾部窗口渲染错误:\n%s", out2)
	}
	if strings.Contains(out2, "task-17") {
		t.Fatalf("clamp 后窗口外的 task-17 不应渲染:\n%s", out2)
	}
}

// TestOverviewScrollsWithSelection 端到端：任务数（30）远超可视行数（5）时，
// 持续 ↓ 能让 selIdx 走到底、offset 跟随滚动、选中行始终出现在 View 输出里；
// 再持续 ↑ 回到顶，offset 归零。
func TestOverviewScrollsWithSelection(t *testing.T) {
	st, err := state.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for i := 0; i < 30; i++ {
		if _, err := st.InsertTask(state.TaskRow{
			IssueRef:    fmt.Sprintf("%d", i+1),
			Description: fmt.Sprintf("task-%02d", i),
			// 总览组内按 created_at 倒序（新 → 旧）：让 task-00 最新、task-29
			// 最旧，展示序 = 入库序，下面的下标断言才有确定性（created_at 相同
			// 时按 rowid 倒序兜底，会把列表整个反过来）。
			CreatedAt: fmt.Sprintf("2026-07-%02dT00:00:00Z", 30-i),
		}); err != nil {
			t.Fatal(err)
		}
	}

	m := New(st, nil)
	m.width = 100
	m.height = ovOverheadRows + 5 // 可视 5 行
	m.snap, _ = ReadSnapshot(st, nil)
	if len(m.snap.Tasks) != 30 {
		t.Fatalf("snapshot tasks=%d want 30", len(m.snap.Tasks))
	}

	down := tea.KeyMsg{Type: tea.KeyDown}
	up := tea.KeyMsg{Type: tea.KeyUp}

	// ↓×10：selIdx=10，越过窗口下沿 → offset 滚到 10-5+1=6
	for i := 0; i < 10; i++ {
		mm, _ := m.Update(down)
		m = mm.(Model)
	}
	if m.selIdx != 10 || m.offset != 6 {
		t.Fatalf("↓×10 后 selIdx=%d offset=%d, want 10/6", m.selIdx, m.offset)
	}
	out := m.View()
	if !strings.Contains(out, "task-10") { // 选中行（idx 10）可见
		t.Fatalf("选中行 task-10 不在渲染窗口内:\n%s", out)
	}
	if strings.Contains(out, "task-00") || strings.Contains(out, "task-29") {
		t.Fatalf("窗口外行被渲染:\n%s", out)
	}

	// ↓ 到底：selIdx 到 29（列表末尾），offset=25，选中行仍可见
	for i := 0; i < 40; i++ {
		mm, _ := m.Update(down)
		m = mm.(Model)
	}
	if m.selIdx != 29 || m.offset != 25 {
		t.Fatalf("到底后 selIdx=%d offset=%d, want 29/25", m.selIdx, m.offset)
	}
	if out := m.View(); !strings.Contains(out, "task-29") {
		t.Fatalf("末尾选中行不可见:\n%s", out)
	}

	// ↑ 回顶：selIdx=0，offset 归零，首行可见、末行不可见
	for i := 0; i < 40; i++ {
		mm, _ := m.Update(up)
		m = mm.(Model)
	}
	if m.selIdx != 0 || m.offset != 0 {
		t.Fatalf("回顶后 selIdx=%d offset=%d, want 0/0", m.selIdx, m.offset)
	}
	out = m.View()
	if !strings.Contains(out, "task-00") || strings.Contains(out, "task-29") {
		t.Fatalf("回顶后窗口错误:\n%s", out)
	}
}

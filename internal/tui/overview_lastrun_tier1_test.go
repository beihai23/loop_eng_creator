package tui

import (
	"strings"
	"testing"

	"loop-eng/internal/state"
)

// TestTier1LastRunFormat 验收：formatLastRun 纯函数——空串/垃圾兜底「—」，
// RFC3339(Nano) 输入格式化为 "2006-01-02 15:04"。
func TestTier1LastRunFormat(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "—"},
		{"not-a-time", "—"},
		{"2026-07-20T10:30:00Z", "2026-07-20 10:30"},
		{"2026-07-20T10:30:00.123456789Z", "2026-07-20 10:30"},
	}
	for _, c := range cases {
		if got := formatLastRun(c.in); got != c.want {
			t.Fatalf("formatLastRun(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestTier1LastRunRender 验收：总览新增「最后运行」列（位置在状态列后、任务列前），
// 列宽 ovLastRunW > 16 以容纳 16 字宽值并留分隔；从未运行 task 显示「—」。
func TestTier1LastRunRender(t *testing.T) {
	if ovLastRunW <= 16 {
		t.Fatalf("ovLastRunW=%d, want > 16 so the 16-wide value is padded and separated from the task column", ovLastRunW)
	}
	snap := &Snapshot{
		Tasks: []state.TaskView{
			{ID: "t1", IssueRef: "#1", Description: "zzzdemo", Status: "done", LastRunAt: "2026-07-20T10:30:00Z"},
			{ID: "t2", IssueRef: "#2", Description: "fresh", Status: "new"}, // 无 LastRunAt → 「—」
		},
		Counts: map[string]int{"new": 1, "done": 1},
	}
	out := RenderOverview(snap, 0, 0, 5, 0.0, 80)
	if !strings.Contains(out, "最后运行") {
		t.Fatalf("最后运行 header missing: %q", out)
	}
	if !strings.Contains(out, "2026-07-20 10:30") {
		t.Fatalf("formatted last-run value missing: %q", out)
	}
	if !strings.Contains(out, "—") {
		t.Fatalf("placeholder — for never-run task missing: %q", out)
	}
	// 列顺序：状态(done) → 最后运行值 → 描述(zzzdemo)，索引严格递增（ANSI 不改变纯文本相对顺序）。
	iStatus := strings.Index(out, "done")
	iLast := strings.Index(out, "2026-07-20 10:30")
	iDesc := strings.Index(out, "zzzdemo")
	if !(iStatus >= 0 && iLast > iStatus && iDesc > iLast) {
		t.Fatalf("column order wrong: status@%d lastrun@%d desc@%d (want status<lastrun<desc): %q", iStatus, iLast, iDesc, out)
	}
}
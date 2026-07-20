package tui

import (
	"regexp"
	"strings"
	"testing"

	"loop-eng/internal/state"
)

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripANSI(s string) string { return ansiRe.ReplaceAllString(s, "") }

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

// TestTier1LastRunRender 验收「最后运行」列的列间距平衡：状态↔最后运行 不过大、
// 最后运行↔任务 不过小（不粘连），最长真实状态 needs-review 也不粘连最后运行值。
// 纯渲染断言：stripANSI 后量两段间距（测试环境 Ascii profile 本就无 ANSI，strip 兜底）。
func TestTier1LastRunRender(t *testing.T) {
	snap := &Snapshot{
		Tasks: []state.TaskView{
			{ID: "t1", IssueRef: "#1", Description: "zzzdemo", Status: "done", LastRunAt: "2026-07-20T10:30:00Z"},
			{ID: "t2", IssueRef: "#2", Description: "fresh", Status: "new"},
			{ID: "t3", IssueRef: "#3", Description: "spec review", Status: "needs-review", LastRunAt: "2026-07-20T09:12:00Z"},
		},
		Counts: map[string]int{"new": 1, "done": 1, "needs-review": 1},
	}
	plain := stripANSI(RenderOverview(snap, 0, 0, 5, 0.0, 80))
	for _, want := range []string{"最后运行", "2026-07-20 10:30", "—"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("%q missing: %q", want, plain)
		}
	}
	// done 行：状态 → 最后运行 → 任务 顺序，量两段间距
	di := strings.Index(plain, "done")
	li := strings.Index(plain, "2026-07-20 10:30")
	ei := strings.Index(plain, "zzzdemo")
	if !(di >= 0 && li > di && ei > li) {
		t.Fatalf("done-row order wrong: done@%d lastrun@%d desc@%d: %q", di, li, ei, plain)
	}
	gapLD := plain[li+len("2026-07-20 10:30") : ei] // 最后运行 → 任务
	if strings.Trim(gapLD, " ") != "" || len(gapLD) < 2 {
		t.Fatalf("lastrun->task gap too small (got %d spaces, want >= 2): %q", len(gapLD), plain)
	}
	gapSD := plain[di+len("done") : li] // 状态 → 最后运行
	if strings.Trim(gapSD, " ") != "" {
		t.Fatalf("status->lastrun gap has non-space: %q", gapSD)
	}
	if len(gapSD) > len(gapLD)*3 {
		t.Fatalf("status->lastrun gap (%d) > 3x lastrun->task gap (%d), columns not balanced: %q", len(gapSD), len(gapLD), plain)
	}
	// needs-review 行：最长真实状态不得粘连最后运行值（>= 2 间隔）
	ni := strings.Index(plain, "needs-review")
	nli := strings.Index(plain, "2026-07-20 09:12")
	if ni < 0 || nli < ni {
		t.Fatalf("needs-review row not found: %q", plain)
	}
	gapNR := plain[ni+len("needs-review") : nli]
	if strings.Trim(gapNR, " ") != "" || len(gapNR) < 2 {
		t.Fatalf("needs-review jams into lastrun (got %d spaces, want >= 2): %q", len(gapNR), plain)
	}
}

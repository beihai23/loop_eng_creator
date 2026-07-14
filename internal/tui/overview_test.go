package tui

import (
	"strings"
	"testing"

	"loop-eng/internal/state"
)

func TestRenderOverviewCountsAndSymbols(t *testing.T) {
	snap := &Snapshot{
		Tasks: []state.TaskView{
			{ID: "t_running", IssueRef: "#b", Description: "running task", Status: "running"},
			{ID: "t_new", IssueRef: "#a", Description: "new task", Status: "new"},
		},
		Running: &RunningInfo{TaskID: "t_running", Phase: "execute"},
		Counts:  map[string]int{"new": 1, "running": 1},
	}
	out := RenderOverview(snap, 0, 0.0, 80)
	// 计数条含待处理/进行中
	if !strings.Contains(out, "待处理") || !strings.Contains(out, "进行中") {
		t.Fatalf("counts bar missing: %q", out)
	}
	// 进行中任务行带 ● 符号（呼吸灯稳态）
	if !strings.Contains(out, "●") {
		t.Fatalf("running symbol missing: %q", out)
	}
	// 选中行带选中标记
	if !strings.Contains(out, "▸") {
		t.Fatalf("selection marker missing: %q", out)
	}
}

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
	// 选中行带选中标记（实心三角 ▶）
	if !strings.Contains(out, "▶") {
		t.Fatalf("selection marker missing: %q", out)
	}
	// 列头存在（字段不再靠猜）
	for _, h := range []string{"№", "状态", "任务"} {
		if !strings.Contains(out, h) {
			t.Fatalf("column header %q missing: %q", h, out)
		}
	}
	// 分隔线存在（表格结构感）
	if !strings.Contains(out, "─") {
		t.Fatalf("separator line missing: %q", out)
	}
	// 未选中行不应带 ▶（只有光标行带）
	// 第二行（selIdx=0 之外）用空格 marker，不含 ▶ 之外的多余三角——这里仅断言 ▶ 只出现一次。
	if strings.Count(out, "▶") != 1 {
		t.Fatalf("exactly one selected row expected, got %d ▶: %q", strings.Count(out, "▶"), out)
	}
}

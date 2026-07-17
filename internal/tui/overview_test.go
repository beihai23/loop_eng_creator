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
	// running 行直接显示当前 phase（不再只显示「running」）
	if !strings.Contains(out, "execute") {
		t.Fatalf("running row phase missing: %q", out)
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

// TestRenderOverviewRunningPhase 验收：注入 snap.Running{Phase:"execute"}，总览 running 行
// 含 "execute"（plan/execute/verify/starting 之一），不再只显示「running」；且 phase 只出现在
// running 行，其它态保持原 status。同时覆盖空 phase 兜底与 retry N 附缀。
func TestRenderOverviewRunningPhase(t *testing.T) {
	tasks := func() []state.TaskView {
		return []state.TaskView{
			{ID: "t_run", IssueRef: "#1", Description: "alpha", Status: "running"},
			{ID: "t_new", IssueRef: "#2", Description: "beta", Status: "new"},
		}
	}

	// phase=execute → running 行含 "execute"，非 running 行仍是 "new"
	out := RenderOverview(&Snapshot{
		Tasks:   tasks(),
		Running: &RunningInfo{TaskID: "t_run", Phase: "execute"},
		Counts:  map[string]int{"new": 1, "running": 1},
	}, 0, 0.0, 80)
	if !strings.Contains(out, "execute") {
		t.Fatalf("running row should show phase execute: %q", out)
	}
	if !strings.Contains(out, "new") {
		t.Fatalf("non-running row should still show status new: %q", out)
	}

	// 空 phase → 兜底 "running"（dispatched 尚未落到任一 phase）
	out = RenderOverview(&Snapshot{
		Tasks:   tasks(),
		Running: &RunningInfo{TaskID: "t_run", Phase: ""},
		Counts:  map[string]int{"new": 1, "running": 1},
	}, 0, 0.0, 80)
	if !strings.Contains(out, "running") {
		t.Fatalf("empty phase should fall back to running: %q", out)
	}

	// retry（attempt≥2）→ 附 " · retry N"
	out = RenderOverview(&Snapshot{
		Tasks:   tasks(),
		Running: &RunningInfo{TaskID: "t_run", Phase: "verify", Retry: 3},
		Counts:  map[string]int{"new": 1, "running": 1},
	}, 0, 0.0, 80)
	if !strings.Contains(out, "verify") || !strings.Contains(out, "retry 3") {
		t.Fatalf("running row should show phase + retry 3: %q", out)
	}
}

// TestRenderOverviewNoRunningKeepsStatus 验收「只 running 行有 phase；其它态不变」：
// snap.Running 为 nil（无活跃）时，即便某行 status==running，也只显示 "running"，无 phase。
func TestRenderOverviewNoRunningKeepsStatus(t *testing.T) {
	out := RenderOverview(&Snapshot{
		Tasks: []state.TaskView{
			{ID: "t_run", IssueRef: "#1", Description: "alpha", Status: "running"},
		},
		Running: nil,
		Counts:  map[string]int{"running": 1},
	}, 0, 0.0, 80)
	if !strings.Contains(out, "running") {
		t.Fatalf("stale running row without Running info should show running: %q", out)
	}
}

package tui

import (
	"strings"
	"testing"

	"loop-eng/internal/state"
)

// TestTier1OverviewRunningPhase 验收契约：running 行直接渲染 snap.Running.Phase。
func TestTier1OverviewRunningPhase(t *testing.T) {
	snap := &Snapshot{
		Tasks: []state.TaskView{
			{ID: "t1", IssueRef: "#1", Description: "demo running", Status: "running"},
		},
		Running: &RunningInfo{TaskID: "t1", Phase: "execute"},
		Counts:  map[string]int{"running": 1},
	}
	out := RenderOverview(snap, 0, 0, 50, 0.5, 80)
	if !strings.Contains(out, "execute") {
		t.Fatalf("running 行必须显示当前 phase=execute, got: %q", out)
	}

	// 非 running 态不被改成 phase：done 行仍渲染自身 status。
	done := &Snapshot{
		Tasks: []state.TaskView{
			{ID: "t2", IssueRef: "#2", Description: "demo done", Status: "done"},
		},
		Counts: map[string]int{"done": 1},
	}
	out2 := RenderOverview(done, 0, 0, 50, 0.0, 80)
	if !strings.Contains(out2, "done") {
		t.Fatalf("done 行应仍渲染自身 status=done, got: %q", out2)
	}
}

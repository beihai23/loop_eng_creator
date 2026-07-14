package tui

import (
	"strings"
	"testing"

	"loop-eng/internal/state"
)

func TestRenderTraceGroupsByRun(t *testing.T) {
	st, _ := state.Open(t.TempDir() + "/state.db")
	defer st.Close()
	tid, _ := st.InsertTask(state.TaskRow{IssueRef: "#1", Description: "d"})
	r1, _ := st.StartRun(tid)
	_ = st.AppendStep(state.StepRow{RunID: r1, Seq: 11, Role: "plan", Status: "ok"})
	_ = st.EndRun(r1, "needs-review")
	r2, _ := st.StartRun(tid)
	_ = st.AppendStep(state.StepRow{RunID: r2, Seq: 11, Role: "plan", Status: "ok"})
	_ = st.EndRun(r2, "done")

	out := RenderTrace(st, tid)
	// 两个 run 分组标题
	if !strings.Contains(out, "run 1") || !strings.Contains(out, "run 2") {
		t.Fatalf("run grouping missing:\n%s", out)
	}
	// 时间戳存在（B0 加的 At）
	if !strings.Contains(out, "▸ plan") {
		t.Fatalf("plan step missing:\n%s", out)
	}
}

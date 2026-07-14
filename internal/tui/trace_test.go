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

// TestRenderTraceRebuildsWhenRunsEmpty 复现并守住「runs 表为空时整页空白」的 bug：
// 旧数据直接 AppendStep（run_id==task_id）却没走 StartRun → runs 表为空。
// RenderTrace 必须按 task_id 重建 steps，而不是只打印标题。
func TestRenderTraceRebuildsWhenRunsEmpty(t *testing.T) {
	st, _ := state.Open(t.TempDir() + "/state.db")
	defer st.Close()
	tid, _ := st.InsertTask(state.TaskRow{IssueRef: "#1", Description: "d"})
	// 模拟旧数据：steps 落盘了（run_id==task_id），但 runs 表没有任何行。
	_ = st.AppendStep(state.StepRow{RunID: tid, Seq: 1, Role: "plan", Status: "ok", At: "2026-07-10T03:55:20Z"})
	_ = st.AppendStep(state.StepRow{RunID: tid, Seq: 2, Role: "verify", Status: "fail", At: "2026-07-10T04:08:01Z"})

	out := RenderTrace(st, tid)
	if !strings.Contains(out, "▸ plan") {
		t.Fatalf("runs 为空时 plan step 应按 task_id 重建出来:\n%s", out)
	}
	if !strings.Contains(out, "▸ verify") {
		t.Fatalf("verify step 应显示:\n%s", out)
	}
	if !strings.Contains(out, "重建") {
		t.Fatalf("应注明按 steps 重建:\n%s", out)
	}
}

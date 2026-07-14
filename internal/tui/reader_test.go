package tui

import (
	"testing"

	"loop-eng/internal/state"
)

func TestReadSnapshotFloatsRunningToTop(t *testing.T) {
	st, _ := state.Open(t.TempDir() + "/state.db")
	defer st.Close()
	a, _ := st.InsertTask(state.TaskRow{IssueRef: "#a", Description: "new task"})
	b, _ := st.InsertTask(state.TaskRow{IssueRef: "#b", Description: "running task"})
	_ = st.AppendTransition(b, "new", "running", "dispatched")
	_ = st.SetInFlight(b, "execute")
	// a 仍是 new

	snap, err := ReadSnapshot(st, nil) // cfg 可为 nil（本测试不验验收方式）
	if err != nil {
		t.Fatal(err)
	}

	// running 应浮顶：snap.Tasks[0].ID == b
	if len(snap.Tasks) < 2 || snap.Tasks[0].ID != b {
		t.Fatalf("running not floated to top: %+v", snap.Tasks)
	}
	if snap.Tasks[1].ID != a {
		t.Fatalf("new task not second: %+v", snap.Tasks)
	}
	// running 元信息
	if snap.Running == nil || snap.Running.TaskID != b {
		t.Fatalf("Running not set: %+v", snap.Running)
	}
	// 计数
	if snap.Counts["new"] != 1 || snap.Counts["running"] != 1 {
		t.Fatalf("counts = %+v", snap.Counts)
	}
	_ = a
}

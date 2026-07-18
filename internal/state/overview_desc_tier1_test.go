package state

import "testing"

// tier-1: TasksByStatus 组内按 created_at 新→旧，分组优先级不变；
// 同一批数据下 NextReadyTask 派发 FIFO 仍最老优先。
func TestTier1OverviewCreatedAtDesc(t *testing.T) {
	s, err := Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// 乱序插入三条 new 任务，created_at 显式区分
	mid, _ := s.InsertTask(TaskRow{IssueRef: "103", Description: "mid", CreatedAt: "2026-07-11T00:00:00Z"})
	old, _ := s.InsertTask(TaskRow{IssueRef: "101", Description: "old", CreatedAt: "2026-07-10T00:00:00Z"})
	newest, _ := s.InsertTask(TaskRow{IssueRef: "102", Description: "newest", CreatedAt: "2026-07-12T00:00:00Z"})
	_ = mid
	// 各塞一条 needs-review 和 done，验证分组优先级不变
	r1, _ := s.InsertTask(TaskRow{IssueRef: "201", Description: "rev", CreatedAt: "2026-07-13T00:00:00Z"})
	d1, _ := s.InsertTask(TaskRow{IssueRef: "301", Description: "done", CreatedAt: "2026-07-14T00:00:00Z"})
	if err := s.AppendTransition(r1, "new", "needs-review", "park"); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendTransition(d1, "new", "done", "ran"); err != nil {
		t.Fatal(err)
	}

	got, err := s.TasksByStatus()
	if err != nil {
		t.Fatal(err)
	}
	wantRefs := []string{"102", "103", "101", "201", "301"}
	if len(got) != len(wantRefs) {
		t.Fatalf("len=%d want %d: %+v", len(got), len(wantRefs), got)
	}
	for i, w := range wantRefs {
		if got[i].IssueRef != w {
			t.Fatalf("order[%d].IssueRef=%q want %q (full: %+v)", i, got[i].IssueRef, w, got)
		}
	}
	if got[0].Status != "new" || got[3].Status != "needs-review" || got[4].Status != "done" {
		t.Fatalf("group priority broken: %+v", got)
	}

	// 派发 FIFO：同批数据下 NextReadyTask 必须仍返回 created_at 最老的 101
	next, ok, err := s.NextReadyTask()
	if err != nil || !ok {
		t.Fatalf("NextReadyTask ok=%v err=%v", ok, err)
	}
	if next.ID != old || next.IssueRef != "101" {
		t.Fatalf("NextReadyTask=%q want oldest 101 (FIFO broken)", next.IssueRef)
	}
	_ = newest
}

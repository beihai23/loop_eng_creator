package state

import "testing"

// TestTasksByStatusOrdersNewestFirst 校验 TUI 概览列表同状态组内按 created_at
// 倒序（新 → 旧，最新任务在最上面）：注入乱序入库的任务（created_at 与入库序、
// issue 号都不同序），TasksByStatus 应返回新 → 旧序，而不是入库序或 issue 号序。
func TestTasksByStatusOrdersNewestFirst(t *testing.T) {
	s, err := Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	inserts := []struct{ ref, createdAt string }{
		{"11", "2026-07-17T09:00:00Z"},
		{"10", "2026-07-18T08:00:00Z"}, // 最新
		{"14", "2026-07-16T09:00:00Z"}, // 最旧
		{"13", "2026-07-17T10:00:00Z"},
	}
	for _, in := range inserts {
		if _, err := s.InsertTask(TaskRow{IssueRef: in.ref, Description: "desc " + in.ref, CreatedAt: in.createdAt}); err != nil {
			t.Fatalf("InsertTask %s: %v", in.ref, err)
		}
	}

	got, err := s.TasksByStatus()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"10", "13", "11", "14"} // 新 → 旧
	if len(got) != len(want) {
		t.Fatalf("got %d tasks, want %d: %+v", len(got), len(want), got)
	}
	for i, v := range got {
		if v.IssueRef != want[i] {
			t.Fatalf("order[%d].IssueRef=%q want %q（全序: %v）", i, v.IssueRef, want[i], refsOf(got))
		}
	}
}

// TestTasksByStatusSameTimestampTiebreak 校验同刻 created_at 的兜底排序：
// 按 rowid 倒序（后入库的排前面），保证展示序确定性、不随查询计划漂移。
func TestTasksByStatusSameTimestampTiebreak(t *testing.T) {
	s, err := Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	for _, ref := range []string{"a", "b", "c"} {
		if _, err := s.InsertTask(TaskRow{
			IssueRef: ref, Description: "desc " + ref,
			CreatedAt: "2026-07-18T00:00:00Z",
		}); err != nil {
			t.Fatalf("InsertTask %s: %v", ref, err)
		}
	}

	got, err := s.TasksByStatus()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"c", "b", "a"} // 同刻 → rowid 倒序
	if len(got) != len(want) {
		t.Fatalf("got %d tasks, want %d: %+v", len(got), len(want), got)
	}
	for i, v := range got {
		if v.IssueRef != want[i] {
			t.Fatalf("order[%d].IssueRef=%q want %q（全序: %v）", i, v.IssueRef, want[i], refsOf(got))
		}
	}
}

func refsOf(vs []TaskView) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = v.IssueRef
	}
	return out
}

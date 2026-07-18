package state

import "testing"

// TestTasksByStatusOrdersByIssueRefNum 校验 TUI 概览列表组内按 issue 号数值排序：
// 注入乱序入库的 issue_ref（11,10,14,13——老任务后入库），TasksByStatus 应返回
// 10,11,13,14（CAST(issue_ref AS INTEGER) ASC），而不是入库序 11,10,14,13。
func TestTasksByStatusOrdersByIssueRefNum(t *testing.T) {
	s, err := Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	for _, ref := range []string{"11", "10", "14", "13"} {
		if _, err := s.InsertTask(TaskRow{IssueRef: ref, Description: "desc " + ref}); err != nil {
			t.Fatalf("InsertTask %s: %v", ref, err)
		}
	}

	got, err := s.TasksByStatus()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"10", "11", "13", "14"}
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

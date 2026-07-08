package state

import "testing"

func TestInsertAndGetTask(t *testing.T) {
	s, err := Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	id, err := s.InsertTask(TaskRow{
		IssueRef: "owner/repo#1", Description: "fix login",
		TaskType: "bugfix", Source: "local", Criteria: []string{"login returns 200"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTask(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Description != "fix login" || len(got.Criteria) != 1 {
		t.Fatalf("got %+v", got)
	}
}

func TestAppendStepAndReplay(t *testing.T) {
	s, _ := Open(t.TempDir() + "/state.db")
	defer s.Close()
	tid, _ := s.InsertTask(TaskRow{Description: "x", TaskType: "t"})
	runID := newID("run")
	s.AppendTransition(tid, "", "running", "dispatched")
	for i, role := range []string{"triage", "plan", "execute"} {
		s.AppendStep(StepRow{RunID: runID, Seq: i, Role: role, Status: "ok"})
	}
	got, err := s.Replay(runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Role != "triage" || got[2].Role != "execute" {
		t.Fatalf("replay order wrong: %+v", got)
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	dir := t.TempDir() + "/state.db"
	if _, err := Open(dir); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir) // 二次打开 = migrate 不重复建表
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
}

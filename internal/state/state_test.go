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

// TestParkedTasksAndResumeTransitions covers the park/resume lifecycle reads the
// daemon tick relies on (spec §7.1 step 2 / §10 / §11 "任务生命周期/park-resume"):
// ParkedTasks surfaces only needs-review tasks, and a resume leaves a durable
// needs-review→new transition carrying the human feedback — readable back as
// the recoverable trace (principle 4).
func TestParkedTasksAndResumeTransitions(t *testing.T) {
	s, _ := Open(t.TempDir() + "/state.db")
	defer s.Close()
	a, _ := s.InsertTask(TaskRow{IssueRef: "A", Description: "a", TaskType: "feat"})
	b, _ := s.InsertTask(TaskRow{IssueRef: "B", Description: "b", TaskType: "feat"})

	// A parks on tier-3; B finishes.
	s.AppendTransition(a, "new", "running", "dispatched")
	s.AppendTransition(a, "running", "needs-review", "tier-3")
	s.AppendTransition(b, "new", "running", "dispatched")
	s.AppendTransition(b, "running", "done", "ran")

	parked, err := s.ParkedTasks()
	if err != nil {
		t.Fatal(err)
	}
	if len(parked) != 1 || parked[0].ID != a {
		t.Fatalf("expected only A parked, got %+v", parked)
	}

	// Resume A with human feedback → back to new (re-enters the dispatch FIFO).
	if err := s.AppendTransition(a, "needs-review", "new", "resumed: ship it"); err != nil {
		t.Fatal(err)
	}

	trans, err := s.Transitions(a)
	if err != nil {
		t.Fatal(err)
	}
	// A: new→running→needs-review→new (3 transitions, chronological).
	if len(trans) != 3 {
		t.Fatalf("expected 3 transitions for A, got %d: %+v", len(trans), trans)
	}
	last := trans[len(trans)-1]
	if last.From != "needs-review" || last.To != "new" || last.Reason != "resumed: ship it" {
		t.Fatalf("resume transition wrong: %+v", last)
	}

	// After resume A is no longer parked.
	if parked2, _ := s.ParkedTasks(); len(parked2) != 0 {
		t.Fatalf("A resumed → no parked tasks, got %+v", parked2)
	}
}

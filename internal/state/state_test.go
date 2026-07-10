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

// TestInFlight covers the single-active live view (spec §8.7/§8.3): a running
// task surfaces with its latest step role as Phase, an empty active slot yields
// ok=false, and only the newest step's role wins.
func TestInFlight(t *testing.T) {
	s, _ := Open(t.TempDir() + "/state.db")
	defer s.Close()

	// No running task yet → slot empty.
	if _, ok, err := s.InFlight(); err != nil || ok {
		t.Fatalf("empty slot: ok=%v err=%v", ok, err)
	}

	tid, _ := s.InsertTask(TaskRow{Description: "x", TaskType: "t"})
	s.AppendTransition(tid, "new", "running", "dispatched")
	// steps.run_id IS the task id (subloop.go writes RunID = task id).
	s.AppendStep(StepRow{RunID: tid, Seq: 1, Role: "plan", Status: "ok"})
	s.AppendStep(StepRow{RunID: tid, Seq: 2, Role: "execute", Status: "ok"})

	got, ok, err := s.InFlight()
	if err != nil || !ok {
		t.Fatalf("InFlight: ok=%v err=%v", ok, err)
	}
	if got.TaskID != tid {
		t.Fatalf("TaskID = %q want %q", got.TaskID, tid)
	}
	if got.Phase != "execute" {
		t.Fatalf("Phase = %q want execute (latest step)", got.Phase)
	}

	// A second running task should never happen (single-active, spec §12), but
	// InFlight is defined to return one row regardless — sanity-check it stays
	// scoped to a single task and does not panic on the LIMIT 1 query.
	tid2, _ := s.InsertTask(TaskRow{Description: "y", TaskType: "t"})
	s.AppendTransition(tid2, "new", "running", "dispatched")
	if _, ok, err := s.InFlight(); err != nil || !ok {
		t.Fatalf("InFlight after 2 running: ok=%v err=%v", ok, err)
	}
}

// TestRequeueOrphanedRunning guards daemon restart recovery (spec principle 4):
// tasks wedged in "running" (from a crashed/killed previous daemon) must reset
// to "new" on the next startup so they re-enter the FIFO instead of hanging.
func TestRequeueOrphanedRunning(t *testing.T) {
	s, _ := Open(t.TempDir() + "/state.db")
	defer s.Close()

	// Two tasks orphaned mid-flight (running), one already done (untouched).
	a, _ := s.InsertTask(TaskRow{Description: "a", TaskType: "t"})
	b, _ := s.InsertTask(TaskRow{Description: "b", TaskType: "t"})
	c, _ := s.InsertTask(TaskRow{Description: "c", TaskType: "t"})
	s.AppendTransition(a, "new", "running", "dispatched")
	s.AppendTransition(b, "new", "running", "dispatched")
	s.AppendTransition(c, "new", "running", "dispatched")
	s.AppendTransition(c, "running", "done", "ran")

	n, err := s.RequeueOrphanedRunning()
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("expected 2 orphaned running reset, got %d", n)
	}
	// a, b are new again; c stays done.
	for _, id := range []string{a, b} {
		got, _ := s.GetTask(id)
		_ = got
		st, _ := statusByID(s, id)
		if st != "new" {
			t.Fatalf("orphan %s must be new, got %q", id, st)
		}
	}
	st, _ := statusByID(s, c)
	if st != "done" {
		t.Fatalf("done task c must stay done, got %q", st)
	}
}

// statusByID reads one task's current status via ListStatuses.
func statusByID(s *Store, id string) (string, error) {
	rows, err := s.ListStatuses()
	if err != nil {
		return "", err
	}
	for _, r := range rows {
		if r.ID == id {
			return r.Status, nil
		}
	}
	return "", nil
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

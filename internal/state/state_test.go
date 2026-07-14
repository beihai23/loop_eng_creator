package state

import (
	"strings"
	"testing"
)

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

// TestInFlight covers the single-active live view via the dedicated in_flight
// table (spec §8.7 cross-process observable). SetInFlight writes the active
// task+phase; InFlight reads it back; ClearInFlight empties it.
func TestInFlight(t *testing.T) {
	s, _ := Open(t.TempDir() + "/state.db")
	defer s.Close()

	// No active task → slot empty.
	if _, ok, err := s.InFlight(); err != nil || ok {
		t.Fatalf("empty slot: ok=%v err=%v", ok, err)
	}

	tid := "task_abc123"

	// SetInFlight writes the active task + phase.
	if err := s.SetInFlight(tid, "plan"); err != nil {
		t.Fatalf("SetInFlight: %v", err)
	}
	got, ok, err := s.InFlight()
	if err != nil || !ok {
		t.Fatalf("InFlight after SetInFlight: ok=%v err=%v", ok, err)
	}
	if got.TaskID != tid {
		t.Fatalf("TaskID = %q want %q", got.TaskID, tid)
	}
	if got.Phase != "plan" {
		t.Fatalf("Phase = %q want plan", got.Phase)
	}

	// Upsert replaces the previous row (single-active).
	if err := s.SetInFlight(tid, "execute"); err != nil {
		t.Fatalf("SetInFlight execute: %v", err)
	}
	got2, ok2, _ := s.InFlight()
	if !ok2 || got2.Phase != "execute" {
		t.Fatalf("after upsert Phase = %q want execute", got2.Phase)
	}

	// ClearInFlight empties the slot.
	if err := s.ClearInFlight(); err != nil {
		t.Fatalf("ClearInFlight: %v", err)
	}
	if _, ok3, _ := s.InFlight(); ok3 {
		t.Fatalf("after ClearInFlight, slot must be empty")
	}
}

// TestSetInFlightOverwrite verifies that SetInFlight replaces any existing row
// (single-active invariant — at most one in_flight row, spec §12).
func TestSetInFlightOverwrite(t *testing.T) {
	s, _ := Open(t.TempDir() + "/state.db")
	defer s.Close()

	s.SetInFlight("task_a", "plan")
	s.SetInFlight("task_b", "execute") // overwrites task_a

	got, ok, err := s.InFlight()
	if err != nil || !ok {
		t.Fatalf("InFlight: ok=%v err=%v", ok, err)
	}
	if got.TaskID != "task_b" || got.Phase != "execute" {
		t.Fatalf("overwrite failed: got task=%s phase=%s, want task_b/execute", got.TaskID, got.Phase)
	}
}

// TestClearInFlightIdempotent verifies ClearInFlight on an empty table is a
// no-op (does not error).
func TestClearInFlightIdempotent(t *testing.T) {
	s, _ := Open(t.TempDir() + "/state.db")
	defer s.Close()

	// Clear an already-empty table.
	if err := s.ClearInFlight(); err != nil {
		t.Fatalf("first ClearInFlight (empty table) must not error: %v", err)
	}
	// Set then clear → clear again.
	s.SetInFlight("task_x", "verify")
	s.ClearInFlight()
	if err := s.ClearInFlight(); err != nil {
		t.Fatalf("second ClearInFlight (idempotent) must not error: %v", err)
	}
	if _, ok, _ := s.InFlight(); ok {
		t.Fatal("after double clear, slot must be empty")
	}
}

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

	// Also leave a stale in_flight row (simulating a crash mid-phase).
	s.SetInFlight(a, "execute")

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

	// Stale in_flight row must also be cleared.
	if _, ok, _ := s.InFlight(); ok {
		t.Fatal("orphan recovery must also clear stale in_flight row")
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

func TestStartEndRun(t *testing.T) {
	s, err := Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	tid, err := s.InsertTask(TaskRow{IssueRef: "o/r#1", Description: "d"})
	if err != nil {
		t.Fatal(err)
	}

	rid, err := s.StartRun(tid)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if rid == "" || !strings.HasPrefix(rid, "run_") {
		t.Fatalf("runID want run_ prefix, got %q", rid)
	}

	// ActiveRun 看到未结束的 run
	gotID, _, ok, err := s.ActiveRun(tid)
	if err != nil || !ok || gotID != rid {
		t.Fatalf("ActiveRun = %q %v %v, want %q true nil", gotID, ok, err, rid)
	}

	if err := s.EndRun(rid, "done"); err != nil {
		t.Fatalf("EndRun: %v", err)
	}

	// 结束后 ActiveRun 无活跃 run
	if _, _, ok, err := s.ActiveRun(tid); err != nil || ok {
		t.Fatalf("ActiveRun after EndRun: ok=%v err=%v, want false nil", ok, err)
	}

	runs, err := s.RunsOfTask(tid)
	if err != nil {
		t.Fatalf("RunsOfTask: %v", err)
	}
	if len(runs) != 1 || runs[0].Outcome != "done" || runs[0].EndedAt == "" {
		t.Fatalf("RunsOfTask = %+v, want 1 done run with EndedAt", runs)
	}
}

func TestCommandsInsertPendingApply(t *testing.T) {
	s, err := Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tid, _ := s.InsertTask(TaskRow{IssueRef: "o/r#1", Description: "d"})

	if err := s.InsertCommand(tid, "resume", "fix the thing"); err != nil {
		t.Fatalf("InsertCommand: %v", err)
	}
	if err := s.InsertCommand(tid, "cancel", ""); err != nil {
		t.Fatalf("InsertCommand cancel: %v", err)
	}

	pending, err := s.PendingCommands()
	if err != nil {
		t.Fatalf("PendingCommands: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("want 2 pending, got %d", len(pending))
	}

	// CancelRequested 看到 pending cancel
	if ok, err := s.CancelRequested(tid); err != nil || !ok {
		t.Fatalf("CancelRequested=%v err=%v, want true nil", ok, err)
	}

	// 应用第一条（resume），回写 applied_at
	if err := s.MarkCommandApplied(pending[0].ID); err != nil {
		t.Fatalf("MarkCommandApplied: %v", err)
	}
	// cancel 仍在 pending（第二条）
	rest, _ := s.PendingCommands()
	if len(rest) != 1 || rest[0].Verb != "cancel" {
		t.Fatalf("after apply resume: pending=%+v, want only cancel", rest)
	}
	if err := s.MarkCommandApplied(rest[0].ID); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.CancelRequested(tid); ok {
		t.Fatalf("CancelRequested after apply, want false")
	}
}

func TestRunsOfTaskMultipleRuns(t *testing.T) {
	s, _ := Open(t.TempDir() + "/state.db")
	defer s.Close()
	tid, _ := s.InsertTask(TaskRow{IssueRef: "o/r#1", Description: "d"})

	r1, _ := s.StartRun(tid)
	_ = s.EndRun(r1, "needs-review")
	r2, _ := s.StartRun(tid)
	_ = s.EndRun(r2, "done")

	runs, _ := s.RunsOfTask(tid)
	if len(runs) != 2 {
		t.Fatalf("want 2 runs, got %d", len(runs))
	}
}

// TestAppendVerificationAndRead covers per-tier verifications 落盘（spec §4.6）：
// AppendVerification 写一行/tier，VerificationsByRun 按 tier 顺序读回。
func TestAppendVerificationAndRead(t *testing.T) {
	s, err := Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tid, _ := s.InsertTask(TaskRow{IssueRef: "o/r#1", Description: "d"})
	rid, _ := s.StartRun(tid)

	if err := s.AppendVerification(rid, 1, true, "go test: ok"); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendVerification(rid, 2, false, "LLM: diff unrelated"); err != nil {
		t.Fatal(err)
	}

	got, err := s.VerificationsByRun(rid)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Tier != 1 || got[0].Passed != true || got[1].Tier != 2 || got[1].Passed != false {
		t.Fatalf("verifications=%+v", got)
	}
}

// TestTasksByStatusJoinsAndOrders 校验 tasks+task_status join 的 TUI 概览查询
// （spec §5[1]）：每条 task 带当前 status，一条查询喂整张列表（无 N+1）。
func TestTasksByStatusJoinsAndOrders(t *testing.T) {
	s, _ := Open(t.TempDir() + "/state.db")
	defer s.Close()
	// new、needs-review、done 各一
	a, _ := s.InsertTask(TaskRow{IssueRef: "#a", Description: "desc a"})
	b, _ := s.InsertTask(TaskRow{IssueRef: "#b", Description: "desc b"})
	c, _ := s.InsertTask(TaskRow{IssueRef: "#c", Description: "desc c"})
	_ = s.AppendTransition(b, "new", "needs-review", "park")
	_ = s.AppendTransition(c, "new", "done", "ran")

	got, err := s.TasksByStatus()
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]TaskView{}
	for _, v := range got {
		byID[v.ID] = v
	}
	if byID[a].Status != "new" || byID[a].Description != "desc a" || byID[a].IssueRef != "#a" {
		t.Fatalf("a = %+v", byID[a])
	}
	if byID[b].Status != "needs-review" || byID[c].Status != "done" {
		t.Fatalf("b=%+v c=%+v", byID[b], byID[c])
	}
}

// TestStepsAndTransitionsCarryAt 校验 StepRow.At / TransitionRow.At 在
// StepsOfTask 和 Transitions 读回时非空（Phase A 终审 I1，trace 时间戳用）。
func TestStepsAndTransitionsCarryAt(t *testing.T) {
	st, err := Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	tid, _ := st.InsertTask(TaskRow{IssueRef: "#1", Description: "d"})
	rid, _ := st.StartRun(tid)
	_ = st.AppendStep(StepRow{RunID: rid, Seq: 11, Role: "plan", Status: "ok"})
	_ = st.AppendTransition(tid, "new", "running", "dispatched")

	steps, err := st.StepsOfTask(tid)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].At == "" {
		t.Fatalf("step At empty: %+v", steps)
	}
	trs, err := st.Transitions(tid)
	if err != nil {
		t.Fatal(err)
	}
	if len(trs) != 1 || trs[0].At == "" {
		t.Fatalf("transition At empty: %+v", trs)
	}
}

// TestStepsOfTask 校验跨 run 的步骤查询（spec §5[3]）：返回某 task 所有 run
// 的全部 step，按 steps.at 排序（规避 Task 2 修复的 per-run seq 冲突）。
func TestStepsOfTask(t *testing.T) {
	s, _ := Open(t.TempDir() + "/state.db")
	defer s.Close()
	tid, _ := s.InsertTask(TaskRow{IssueRef: "#a", Description: "d"})
	rid, _ := s.StartRun(tid)
	_ = s.AppendStep(StepRow{RunID: rid, Seq: 11, Role: "plan", Status: "ok"})
	_ = s.AppendStep(StepRow{RunID: rid, Seq: 12, Role: "execute", Status: "ok"})
	got, err := s.StepsOfTask(tid)
	if err != nil || len(got) != 2 {
		t.Fatalf("StepsOfTask=%+v err=%v want 2", got, err)
	}
}

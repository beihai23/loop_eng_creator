package state

import (
	"fmt"
	"strings"
	"sync"
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

// TestNextReadyTaskOrdersBySubmissionTime proves the dispatch FIFO is ordered by
// issue submission time, NOT by ingest order (spec §8.7 FIFO). The daemon
// ingests via `gh issue list` (newest first), so a newer issue is ingested
// before an older one. If created_at stored ingest time, the newer issue would
// get the smallest created_at and be dispatched first = LIFO. By storing the
// channel-reported submission time (Task.CreatedAt), NextReadyTask must return
// the earliest-submitted task regardless of the order tasks were ingested.
func TestNextReadyTaskOrdersBySubmissionTime(t *testing.T) {
	s, _ := Open(t.TempDir() + "/state.db")
	defer s.Close()

	// #32 was submitted AFTER #31, but `gh issue list` returns newest first, so
	// #32 is ingested first (smaller rowid). Insert it first to mirror that.
	later, _ := s.InsertTask(TaskRow{
		IssueRef: "#32", Description: "newer issue, ingested first",
		CreatedAt: "2026-07-17T10:00:01Z", // submitted later
	})
	// #31 was submitted earlier; it is ingested second (larger rowid).
	earlier, _ := s.InsertTask(TaskRow{
		IssueRef: "#31", Description: "older issue, ingested second",
		CreatedAt: "2026-07-17T09:00:00Z", // submitted earlier
	})

	got, ok, err := s.NextReadyTask()
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected a ready task")
	}
	// Earliest-submitted (#31) is the FIFO head even though it was ingested
	// after #32. Had created_at tracked ingest time, #32 (ingested first →
	// smaller created_at and smaller rowid) would win and this would fail.
	if got.ID != earlier {
		t.Fatalf("NextReadyTask = %q (%s), want earliest-submitted #31 (%q)",
			got.ID, got.IssueRef, earlier)
	}
	if got.ID == later {
		t.Fatalf("NextReadyTask returned later-submitted #32; FIFO must prefer #31")
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

// TestInsertTaskAgentRoundTrip: the per-task agent override (issue frontmatter
// `agent: codex`) persists in tasks.agent and reads back through every dispatch
// path — GetTask (run-once), NextReadyTask (daemon FIFO head). This is the state
// half of the task-level agent override; the daemon reads task.Agent at dispatch
// to opt the task into a different provider.
func TestInsertTaskAgentRoundTrip(t *testing.T) {
	s, _ := Open(t.TempDir() + "/state.db")
	defer s.Close()
	id, err := s.InsertTask(TaskRow{
		IssueRef: "o/r#7", Description: "d", TaskType: "feat",
		Criteria: []string{"c"}, Agent: "codex",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTask(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Agent != "codex" {
		t.Fatalf("GetTask: want Agent=codex, got %q", got.Agent)
	}
	// NextReadyTask is the daemon's dispatch pick — it must surface Agent too so
	// runTask can apply the override without an extra query.
	head, ok, err := s.NextReadyTask()
	if err != nil || !ok {
		t.Fatalf("NextReadyTask: ok=%v err=%v", ok, err)
	}
	if head.Agent != "codex" {
		t.Fatalf("NextReadyTask: want Agent=codex, got %q", head.Agent)
	}
}

// TestAppendStepModelRefReplays: steps.model_ref (the per-step "who ran this"
// audit column) survives AppendStep → Replay so dashboard/replay can show the
// agent that produced each step. The column always existed but was never written
// until the agent layer lit it up.
func TestAppendStepModelRefReplays(t *testing.T) {
	s, _ := Open(t.TempDir() + "/state.db")
	defer s.Close()
	runID := newID("run")
	if err := s.AppendStep(StepRow{RunID: runID, Seq: 1, Role: "plan", Status: "ok", ModelRef: "kimi"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Replay(runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ModelRef != "kimi" {
		t.Fatalf("model_ref did not round-trip: %+v", got)
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

// TestTasksByStatusNewestFirstWithinGroup 钉死总览展示排序：状态分组优先级
// 不变（new → needs-review → needs-info → blocked → done → cancelled），
// 同状态组内按 created_at 倒序（新 → 旧，最新任务在最上面）。同时证明这次
// 展示层倒序不影响派发 FIFO——NextReadyTask 仍返回最老提交的 new 任务。
func TestTasksByStatusNewestFirstWithinGroup(t *testing.T) {
	s, _ := Open(t.TempDir() + "/state.db")
	defer s.Close()

	// 两个 new：old-new 提交更早，new-new 提交更晚。
	oldNew, _ := s.InsertTask(TaskRow{IssueRef: "#31", Description: "older new",
		CreatedAt: "2026-07-17T09:00:00Z"})
	newNew, _ := s.InsertTask(TaskRow{IssueRef: "#32", Description: "newer new",
		CreatedAt: "2026-07-18T09:00:00Z"})
	// 一个 done，提交时间夹在中间——用来验证分组优先级压过时间序。
	doneTask, _ := s.InsertTask(TaskRow{IssueRef: "#33", Description: "done",
		CreatedAt: "2026-07-17T12:00:00Z"})
	_ = s.AppendTransition(doneTask, "new", "done", "ran")

	got, err := s.TasksByStatus()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("len=%d want 3: %+v", len(got), got)
	}
	// new 组（新 → 旧）在前，done 组在后——即使 done 的提交时间比 new-new 旧。
	wantIDs := []string{newNew, oldNew, doneTask}
	wantStatus := []string{"new", "new", "done"}
	for i := range wantIDs {
		if got[i].ID != wantIDs[i] || got[i].Status != wantStatus[i] {
			t.Fatalf("order[%d]=(%s,%s) want (%s,%s); full: %+v",
				i, got[i].ID, got[i].Status, wantIDs[i], wantStatus[i], got)
		}
	}

	// 派发 FIFO 不受展示倒序影响：仍是最老提交的 #31 出队。
	head, ok, err := s.NextReadyTask()
	if err != nil || !ok {
		t.Fatalf("NextReadyTask: ok=%v err=%v", ok, err)
	}
	if head.ID != oldNew {
		t.Fatalf("NextReadyTask = %q (%s), want oldest-submitted #31 (%q); display DESC must not change dispatch FIFO",
			head.ID, head.IssueRef, oldNew)
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

// TestOpenSetsWALAndBusyTimeout pins the concurrency pragmas Open configures.
// WAL is what lets the dashboard's read connection coexist with the daemon's
// writes; busy_timeout is what lets concurrent writers wait instead of erroring
// "database is locked". If a future change drops these, the concurrent-write
// tests below and the dashboard's live view silently regress.
func TestOpenSetsWALAndBusyTimeout(t *testing.T) {
	s, err := Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var mode string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("read journal_mode: %v", err)
	}
	if strings.ToLower(mode) != "wal" {
		t.Fatalf("journal_mode=%q want wal", mode)
	}

	var bt int
	if err := s.db.QueryRow("PRAGMA busy_timeout").Scan(&bt); err != nil {
		t.Fatalf("read busy_timeout: %v", err)
	}
	if bt <= 0 {
		t.Fatalf("busy_timeout=%d want >0", bt)
	}
}

// TestConcurrentInsertsNoLock is the M3 concurrency acceptance test: many
// goroutines hammering InsertTask must all succeed under `go test -race` — no
// "database is locked" (busy_timeout + WAL) and no data race (the driver owns
// all shared memory). The daemon's background-ingest goroutine and synchronous
// tick share one Store, so concurrent writes are the production reality.
func TestConcurrentInsertsNoLock(t *testing.T) {
	s, err := Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	const goroutines, perG = 16, 25
	var wg sync.WaitGroup
	errCh := make(chan error, goroutines*perG)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				if _, err := s.InsertTask(TaskRow{
					IssueRef:    fmt.Sprintf("ref-%d-%d", g, i),
					Description: "concurrent insert",
					TaskType:    "feat",
				}); err != nil {
					errCh <- fmt.Errorf("goroutine %d insert %d: %w", g, i, err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}

	refs, err := s.IssueRefs()
	if err != nil {
		t.Fatalf("issue refs: %v", err)
	}
	if want := goroutines * perG; len(refs) != want {
		t.Fatalf("expected %d persisted refs, got %d", want, len(refs))
	}
}

// TestConcurrentMixedWritesNoLock stresses several write paths (different
// tables / transactions) concurrently: SetInFlight, AppendTransition,
// AppendStep. Each is a short transaction; under WAL + busy_timeout they
// serialize without "database is locked".
func TestConcurrentMixedWritesNoLock(t *testing.T) {
	s, err := Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	tid, err := s.InsertTask(TaskRow{IssueRef: "seed", Description: "d", TaskType: "feat"})
	if err != nil {
		t.Fatal(err)
	}
	runID := newID("run")

	const iters = 100
	var wg sync.WaitGroup
	errCh := make(chan error, 3)

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			if err := s.SetInFlight(tid, "plan"); err != nil {
				errCh <- fmt.Errorf("SetInFlight: %w", err)
				return
			}
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			if err := s.AppendTransition(tid, "new", "running", "stress"); err != nil {
				errCh <- fmt.Errorf("AppendTransition: %w", err)
				return
			}
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			if err := s.AppendStep(StepRow{RunID: runID, Seq: i, Role: "plan", Status: "ok"}); err != nil {
				errCh <- fmt.Errorf("AppendStep: %w", err)
				return
			}
		}
	}()
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}

	// Spot-check the writers actually wrote.
	if got, _ := s.Replay(runID); len(got) != iters {
		t.Fatalf("steps persisted=%d want %d", len(got), iters)
	}
	ifl, ok, _ := s.InFlight()
	if !ok || ifl.TaskID != tid {
		t.Fatalf("in_flight after churn = %+v ok=%v, want task %s", ifl, ok, tid)
	}
}

// TestSeparateConnectionSeesCommittedWrite proves cross-connection visibility —
// the dashboard opens its own Store (own *sql.DB / connection pool, often its
// own process) and must see tasks the daemon just committed. WAL readers see the
// latest committed snapshot without blocking on the writer, so this holds even
// while the daemon is mid-write.
func TestSeparateConnectionSeesCommittedWrite(t *testing.T) {
	path := t.TempDir() + "/state.db"
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	reader, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	if _, err := writer.InsertTask(TaskRow{
		IssueRef: "R1", Description: "reader-visible", TaskType: "feat",
	}); err != nil {
		t.Fatal(err)
	}

	views, err := reader.TasksByStatus()
	if err != nil {
		t.Fatalf("reader TasksByStatus: %v", err)
	}
	if len(views) != 1 || views[0].IssueRef != "R1" {
		t.Fatalf("reader did not see committed write: %+v", views)
	}
}

// TestInitialPrompts 钉死「run 的初始提示词」读取：取该 run 首个带 input_json 的
// plan / execute step（dashboard 详情页「初始提示词」的数据源）。要点：
//   - 多 attempt 时取首轮（seq 升序），不是最后一轮；
//   - 空 input_json 的 step（旧数据）被跳过；
//   - 某 role 无记录（如 plan 失败未走到 execute）→ 对应返回空串而非报错。
func TestInitialPrompts(t *testing.T) {
	st, _ := Open(t.TempDir() + "/state.db")
	defer st.Close()
	tid, _ := st.InsertTask(TaskRow{IssueRef: "#1", Description: "d"})
	rid, _ := st.StartRun(tid)

	// attempt 1：plan 有输入；execute 未走到（无 step）。
	_ = st.AppendStep(StepRow{RunID: rid, Seq: 11, Role: "plan", Status: "ok", InputJSON: "PLAN首轮提示词"})
	// attempt 2：plan 重试（更新的反馈）+ execute 首轮输入。
	_ = st.AppendStep(StepRow{RunID: rid, Seq: 21, Role: "plan", Status: "ok", InputJSON: "PLAN次轮提示词"})
	_ = st.AppendStep(StepRow{RunID: rid, Seq: 22, Role: "execute", Status: "ok", InputJSON: "EXECUTE首轮提示词"})
	// 旧数据：空 input_json 的 execute step 不得抢先命中（seq 更小但为空）。
	_ = st.AppendStep(StepRow{RunID: rid, Seq: 2, Role: "execute", Status: "ok"})

	plan, exec, err := st.InitialPrompts(rid)
	if err != nil {
		t.Fatal(err)
	}
	if plan != "PLAN首轮提示词" {
		t.Fatalf("plan prompt = %q, want 首轮", plan)
	}
	if exec != "EXECUTE首轮提示词" {
		t.Fatalf("execute prompt = %q, want 首轮非空记录", exec)
	}

	// 无 execute 记录的 run → execute 返回空串。
	rid2, _ := st.StartRun(tid)
	_ = st.AppendStep(StepRow{RunID: rid2, Seq: 11, Role: "plan", Status: "fail", InputJSON: "P2"})
	_, exec2, err := st.InitialPrompts(rid2)
	if err != nil {
		t.Fatal(err)
	}
	if exec2 != "" {
		t.Fatalf("execute prompt = %q, want empty (no execute step)", exec2)
	}
}

// TestUpdateTaskSpec 钉死 spec 快照的回写语义：description + criteria + 全文 body
// 被替换、updated_at 被刷新；TaskSpecsByRef 能读回新值。
func TestUpdateTaskSpec(t *testing.T) {
	st, _ := Open(t.TempDir() + "/state.db")
	defer st.Close()
	id, _ := st.InsertTask(TaskRow{IssueRef: "9", Description: "v1", Criteria: []string{"a"}, Body: "正文 v1 全文"})

	if err := st.UpdateTaskSpec(id, "v2 edited", []string{"a", "b"}, "正文 v2 全文"); err != nil {
		t.Fatal(err)
	}
	specs, err := st.TaskSpecsByRef()
	if err != nil {
		t.Fatal(err)
	}
	got := specs["9"]
	if got.Description != "v2 edited" || len(got.Criteria) != 2 || got.Criteria[1] != "b" {
		t.Fatalf("spec 未回写: %+v", got)
	}
	if got.Body != "正文 v2 全文" {
		t.Fatalf("body 未回写: %q", got.Body)
	}
	if got.UpdatedAt == "" {
		t.Fatalf("updated_at 未填充: %+v", got)
	}
	if got.ID != id {
		t.Fatalf("TaskSpecsByRef 应携带 id: got %q want %q", got.ID, id)
	}
}

// TestTaskBodyRoundTrip 钉死「全文保留」的读写回路：InsertTask 带 Body →
// GetTask / NextReadyTask 读回同一全文；旧库（body 列为 NULL）读成空串不报错。
func TestTaskBodyRoundTrip(t *testing.T) {
	st, _ := Open(t.TempDir() + "/state.db")
	defer st.Close()
	id, _ := st.InsertTask(TaskRow{IssueRef: "10", Description: "首行", Body: "首行\n\n## 背景\n这段叙述不能丢"})

	got, err := st.GetTask(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Body != "首行\n\n## 背景\n这段叙述不能丢" {
		t.Fatalf("GetTask body 未读回: %q", got.Body)
	}
	next, ok, err := st.NextReadyTask()
	if err != nil || !ok {
		t.Fatalf("NextReadyTask: ok=%v err=%v", ok, err)
	}
	if next.Body != got.Body {
		t.Fatalf("NextReadyTask body 未读回: %q", next.Body)
	}
}

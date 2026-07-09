package daemon

import (
	"context"
	"testing"
	"time"

	"loop-eng/internal/channel"
	"loop-eng/internal/state"
)

// scriptedChannel hands ListNewTasks a scripted sequence of task batches,
// advancing one batch per call. The other three Channel methods are no-ops —
// this cut of the engine only ingests (spec §7.1 step 1); replies / comments /
// status marks belong to dispatch+reap, which land in later issues.
type scriptedChannel struct {
	batches [][]channel.Task
	calls   int
}

func (f *scriptedChannel) ListNewTasks(ctx context.Context) ([]channel.Task, error) {
	if f.calls >= len(f.batches) {
		return nil, nil
	}
	b := f.batches[f.calls]
	f.calls++
	return b, nil
}

func (f *scriptedChannel) ListReplies(ctx context.Context, refs []string) (map[string][]channel.Reply, error) {
	return nil, nil
}
func (f *scriptedChannel) PostComment(ctx context.Context, ref, body string) error    { return nil }
func (f *scriptedChannel) UpdateStatus(ctx context.Context, ref, status string) error { return nil }

// cancelChannel returns no tasks but cancels its context on the Nth
// ListNewTasks call, letting TestRunLoopsUntilCancelled stop the resident loop
// deterministically — no wall-clock sleeps, no CI flake.
type cancelChannel struct {
	after  int
	calls  int
	cancel context.CancelFunc
}

func (f *cancelChannel) ListNewTasks(ctx context.Context) ([]channel.Task, error) {
	f.calls++
	if f.calls >= f.after {
		f.cancel()
	}
	return nil, nil
}

func (f *cancelChannel) ListReplies(ctx context.Context, refs []string) (map[string][]channel.Reply, error) {
	return nil, nil
}
func (f *cancelChannel) PostComment(ctx context.Context, ref, body string) error    { return nil }
func (f *cancelChannel) UpdateStatus(ctx context.Context, ref, status string) error { return nil }

func newTestStore(t *testing.T) *state.Store {
	t.Helper()
	st, err := state.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// TestTickIngestsAndDedups is the M3-step-1 acceptance test: a fake channel
// returns [A,B] then [A,B,C], the engine ticks twice over an in-memory Store,
// and exactly three tasks (A,B,C) persist — A/B are NOT re-inserted on the
// second tick.
func TestTickIngestsAndDedups(t *testing.T) {
	st := newTestStore(t)
	ch := &scriptedChannel{batches: [][]channel.Task{
		{
			{Ref: "A", Description: "task A", TaskType: "feat"},
			{Ref: "B", Description: "task B", TaskType: "feat"},
		},
		{
			{Ref: "A", Description: "task A", TaskType: "feat"},
			{Ref: "B", Description: "task B", TaskType: "feat"},
			{Ref: "C", Description: "task C", TaskType: "feat"},
		},
	}}
	eng := &Engine{Channel: ch, Store: st, Interval: time.Second}

	if err := eng.tick(context.Background()); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if err := eng.tick(context.Background()); err != nil {
		t.Fatalf("tick 2: %v", err)
	}

	// The dedup assertion: one task_status row per InsertTask call. Had tick 2
	// re-inserted A and B, this would be 5, not 3. (IssueRefs alone can't prove
	// this — a map collapses duplicate refs.)
	statuses, err := st.ListStatuses()
	if err != nil {
		t.Fatalf("list statuses: %v", err)
	}
	if len(statuses) != 3 {
		t.Fatalf("expected exactly 3 tasks in store (A,B,C; no dup), got %d: %+v", len(statuses), statuses)
	}

	// Identity check: the three persisted tasks are exactly A, B, C.
	refs, err := st.IssueRefs()
	if err != nil {
		t.Fatalf("issue refs: %v", err)
	}
	if len(refs) != 3 || !refs["A"] || !refs["B"] || !refs["C"] {
		t.Fatalf("expected refs {A,B,C}, got %v", refs)
	}
}

// TestRunLoopsUntilCancelled proves Run is a resident tick loop, not a one-shot:
// it ticks immediately, then on the ticker cadence, and returns ctx.Err() when
// the context is cancelled. The channel cancels its own context on the 2nd call,
// so exactly one ticker tick runs after the immediate one.
func TestRunLoopsUntilCancelled(t *testing.T) {
	st := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	ch := &cancelChannel{after: 2, cancel: cancel}
	eng := &Engine{Channel: ch, Store: st, Interval: 5 * time.Millisecond}

	err := eng.Run(ctx)
	if err != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	// calls == 2 ⇒ the immediate tick (1) + exactly one ticker tick (2). A value
	// of 1 would mean Run never advanced past the first tick.
	if ch.calls != 2 {
		t.Fatalf("expected 2 ListNewTasks calls (immediate + 1 ticker), got %d", ch.calls)
	}
}

// TestTickDispatchesReadyTask is the M3-dispatch acceptance test: a fake
// channel returns one task, a fake RunTask returns "done", and after a single
// tick that task's status is "done". Proves ingest→dispatch→status-update is
// wired end to end (spec §7.1 step 3).
func TestTickDispatchesReadyTask(t *testing.T) {
	st := newTestStore(t)
	ch := &scriptedChannel{batches: [][]channel.Task{
		{{Ref: "A", Description: "task A", TaskType: "feat"}},
	}}
	var got state.TaskRow
	eng := &Engine{
		Channel: ch, Store: st, Interval: time.Second,
		RunTask: func(ctx context.Context, task state.TaskRow) (string, error) {
			got = task
			return "done", nil
		},
	}

	if err := eng.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	// RunTask was actually called with the ingested task...
	if got.IssueRef != "A" {
		t.Fatalf("RunTask must be called with task A, got %+v", got)
	}
	// ...and that task is now done (new→running→done transitions left it done).
	statuses, err := st.ListStatuses()
	if err != nil {
		t.Fatalf("list statuses: %v", err)
	}
	if len(statuses) != 1 || statuses[0].Status != "done" {
		t.Fatalf("expected exactly 1 task with status done, got %+v", statuses)
	}
}

// TestTickNoReadyTaskIsNoOp proves dispatch is a no-op when nothing is ready:
// an empty channel + a RunTask that must never be called.
func TestTickNoReadyTaskIsNoOp(t *testing.T) {
	st := newTestStore(t)
	ch := &scriptedChannel{} // no batches → ListNewTasks always returns nil
	called := false
	eng := &Engine{
		Channel: ch, Store: st, Interval: time.Second,
		RunTask: func(ctx context.Context, task state.TaskRow) (string, error) {
			called = true
			return "done", nil
		},
	}

	if err := eng.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if called {
		t.Fatalf("RunTask must not be called when no task is ready")
	}
}

// TestTickDispatchesOldestFirst proves the FIFO pick: with two new tasks
// ingested in one batch (A before B), the single synchronous dispatch slot
// takes A — the oldest by created_at (spec §8.7).
func TestTickDispatchesOldestFirst(t *testing.T) {
	st := newTestStore(t)
	ch := &scriptedChannel{batches: [][]channel.Task{
		{
			{Ref: "A", Description: "task A", TaskType: "feat"},
			{Ref: "B", Description: "task B", TaskType: "feat"},
		},
	}}
	var dispatched string
	eng := &Engine{
		Channel: ch, Store: st, Interval: time.Second,
		RunTask: func(ctx context.Context, task state.TaskRow) (string, error) {
			dispatched = task.IssueRef
			return "done", nil
		},
	}

	if err := eng.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	// A was ingested before B, so it is the FIFO head and is dispatched first.
	if dispatched != "A" {
		t.Fatalf("expected oldest task A dispatched first, got %q", dispatched)
	}
}

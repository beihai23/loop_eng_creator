package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"loop-eng/internal/channel"
	"loop-eng/internal/state"
)

// scriptedChannel hands ListNewTasks a scripted sequence of task batches,
// advancing one batch per call, and hands ListReplies a fixed reply map (set by
// the test to simulate a human answering a parked task). PostComment /
// UpdateStatus are no-ops — writeback belongs to the SubLoop, not the daemon
// tick under test.
type scriptedChannel struct {
	batches [][]channel.Task
	replies map[string][]channel.Reply // human replies per issue ref; nil/empty → none
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

// ListReplies returns the scripted human replies for the parked refs the daemon
// asks about. The daemon only calls this with parked (needs-review) refs, so a
// test simulates "human answered" by setting replies[<ref>] before the tick.
func (f *scriptedChannel) ListReplies(ctx context.Context, refs []string) (map[string][]channel.Reply, error) {
	out := make(map[string][]channel.Reply)
	for _, r := range refs {
		if rs, ok := f.replies[r]; ok {
			out[r] = rs
		}
	}
	return out, nil
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
		RunTask: func(ctx context.Context, task state.TaskRow) (string, string, error) {
			got = task
			return "done", "", nil
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
		RunTask: func(ctx context.Context, task state.TaskRow) (string, string, error) {
			called = true
			return "done", "", nil
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
		RunTask: func(ctx context.Context, task state.TaskRow) (string, string, error) {
			dispatched = task.IssueRef
			return "done", "", nil
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

// statusOf looks up one task's current status by id via ListStatuses (the
// daemon's lifecycle writer is the Store, so tests read it back through the
// Store's own read API rather than poking Engine internals).
func statusOf(t *testing.T, st *state.Store, id string) string {
	t.Helper()
	rows, err := st.ListStatuses()
	if err != nil {
		t.Fatalf("list statuses: %v", err)
	}
	for _, r := range rows {
		if r.ID == id {
			return r.Status
		}
	}
	t.Fatalf("no task_status row for %s", id)
	return ""
}

// TestTickParksAndResumes is the M3 park/resume acceptance test (spec §7.1
// step 2 + §7.2c/§10): a fake channel returns one task, a scripted RunTask
// returns needs-review (tier-3 NeedsHuman) on the first call then done on the
// second, and the channel produces a human reply between the two ticks. Tick 1
// must park the task (status=needs-review, active slot freed); tick 2 must
// resume it — poll the reply, transition needs-review→new carrying the human
// feedback, and re-dispatch it to done.
func TestTickParksAndResumes(t *testing.T) {
	st := newTestStore(t)
	ch := &scriptedChannel{batches: [][]channel.Task{
		{{Ref: "A", Description: "task A", TaskType: "feat"}}, // tick 1: ingest A
		{}, // tick 2: no new tasks (A deduped against the Store)
	}}
	var runCalls int
	var taskID string
	eng := &Engine{
		Channel: ch, Store: st, Interval: time.Second,
		RunTask: func(ctx context.Context, task state.TaskRow) (string, string, error) {
			runCalls++
			taskID = task.ID
			if runCalls == 1 {
				return "needs-review", "", nil // tier-3 human review → park
			}
			return "done", "", nil // resumed re-run passes
		},
	}

	// ---- tick 1: dispatch A → tier-3 parks it (active slot freed) ----
	if err := eng.tick(context.Background()); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if runCalls != 1 {
		t.Fatalf("tick 1 must dispatch A exactly once, got %d RunTask calls", runCalls)
	}
	if s := statusOf(t, st, taskID); s != "needs-review" {
		t.Fatalf("after tick 1, A must be parked (needs-review), got %q", s)
	}

	// Human answers on the issue before tick 2 (simulated reply on the channel).
	ch.replies = map[string][]channel.Reply{"A": {{Body: "use approach 2 instead"}}}

	// ---- tick 2: poll parked A → reply → resume (needs-review→new, feedback
	// recorded) → re-dispatch → done ----
	if err := eng.tick(context.Background()); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if runCalls != 2 {
		t.Fatalf("tick 2 must resume+re-dispatch A (a 2nd RunTask call), got %d", runCalls)
	}
	if s := statusOf(t, st, taskID); s != "done" {
		t.Fatalf("after tick 2 (resumed re-dispatch), A must be done, got %q", s)
	}

	// The resume left a durable needs-review→new transition carrying the human
	// feedback in its reason — observable in the trace even though step 3 of the
	// same tick then ran A to done. This is criterion 2 ("改回 new" + 带反馈) and
	// the recoverable feedback record (spec principle 4).
	trans, err := st.Transitions(taskID)
	if err != nil {
		t.Fatalf("transitions: %v", err)
	}
	var resumed bool
	for _, tr := range trans {
		if tr.From == "needs-review" && tr.To == "new" && strings.Contains(tr.Reason, "use approach 2 instead") {
			resumed = true
		}
	}
	if !resumed {
		t.Fatalf("expected a needs-review→new resume transition carrying the reply, got %+v", trans)
	}
}

// TestTickParksThenRunsAnother proves a parked task frees the active slot (spec
// §7.2c/§10 "parked 不占活跃位"): A parks on tier-3, then a later tick with no
// reply for A still dispatches a freshly-ingested B. A stays parked; B runs.
func TestTickParksThenRunsAnother(t *testing.T) {
	st := newTestStore(t)
	ch := &scriptedChannel{batches: [][]channel.Task{
		{{Ref: "A", Description: "task A", TaskType: "feat"}}, // tick 1: A
		{{Ref: "B", Description: "task B", TaskType: "feat"}}, // tick 2: B (no reply for A)
	}}
	var dispatched []string
	var idsByRef = map[string]string{}
	eng := &Engine{
		Channel: ch, Store: st, Interval: time.Second,
		RunTask: func(ctx context.Context, task state.TaskRow) (string, string, error) {
			dispatched = append(dispatched, task.IssueRef)
			idsByRef[task.IssueRef] = task.ID
			if task.IssueRef == "A" {
				return "needs-review", "", nil // A parks
			}
			return "done", "", nil // B passes
		},
	}

	if err := eng.tick(context.Background()); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if err := eng.tick(context.Background()); err != nil {
		t.Fatalf("tick 2: %v", err)
	}

	// B was dispatched even though A is still parked — the slot was free.
	if len(dispatched) != 2 || dispatched[0] != "A" || dispatched[1] != "B" {
		t.Fatalf("expected A then B dispatched, got %v", dispatched)
	}
	if s := statusOf(t, st, idsByRef["A"]); s != "needs-review" {
		t.Fatalf("A must still be parked (no reply), got %q", s)
	}
	if s := statusOf(t, st, idsByRef["B"]); s != "done" {
		t.Fatalf("B must be done, got %q", s)
	}
}

// TestEngineCooldownOnTransientInfra: when a task blocks on transient upstream
// infra (GLM 529 "该模型当前访问量过大"), the daemon enters cooldown and the next
// tick skips dispatch — it must NOT churn the next ready task (B) into the same
// 529 wall. Guards the recurring GLM-congestion failure mode.
func TestEngineCooldownOnTransientInfra(t *testing.T) {
	st := newTestStore(t)
	ch := &scriptedChannel{batches: [][]channel.Task{
		{{Ref: "A", Description: "task A", TaskType: "feat"}}, // tick 1: ingest + dispatch A
		{{Ref: "B", Description: "task B", TaskType: "feat"}}, // tick 2: ingest B (should NOT dispatch)
	}}
	var runCalls int
	var dispatched []string
	var taskAID string
	eng := &Engine{
		Channel: ch, Store: st, Interval: time.Second, Cooldown: 1 * time.Hour,
		RunTask: func(ctx context.Context, task state.TaskRow) (string, string, error) {
			runCalls++
			dispatched = append(dispatched, task.IssueRef)
			taskAID = task.ID
			return "blocked", "retries exhausted: plan error: API Error: 529 [1305][该模型当前访问量过大]", nil
		},
	}

	if err := eng.tick(context.Background()); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if runCalls != 1 {
		t.Fatalf("tick 1 must dispatch A once, got %d", runCalls)
	}
	if eng.coolUntil.IsZero() {
		t.Fatal("transient-infra block must trip cooldown (coolUntil set)")
	}
	// transient infra re-queues the task (running→new), NOT permanent blocked —
	// after cooldown the daemon retries the SAME task (self-healing).
	if s := statusOf(t, st, taskAID); s != "new" {
		t.Fatalf("transient-infra block must re-queue A as new, got %q", s)
	}

	// tick 2: cooldown active → B is ingested but NOT dispatched.
	if err := eng.tick(context.Background()); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if runCalls != 1 {
		t.Fatalf("cooldown must skip dispatch on tick 2 (B not run); RunTask calls = %d, dispatched %v", runCalls, dispatched)
	}
}

// TestEngineNoCooldownOnRealBlock: a real block (verify rejection, not infra)
// must NOT trip cooldown — the next ready task dispatches normally next tick.
func TestEngineNoCooldownOnRealBlock(t *testing.T) {
	st := newTestStore(t)
	ch := &scriptedChannel{batches: [][]channel.Task{
		{{Ref: "A", Description: "task A", TaskType: "feat"}},
		{{Ref: "B", Description: "task B", TaskType: "feat"}},
	}}
	var runCalls int
	eng := &Engine{
		Channel: ch, Store: st, Interval: time.Second, Cooldown: 1 * time.Hour,
		RunTask: func(ctx context.Context, task state.TaskRow) (string, string, error) {
			runCalls++
			return "blocked", "verify rejected: acceptance criterion #2 not met in diff", nil
		},
	}

	if err := eng.tick(context.Background()); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if !eng.coolUntil.IsZero() {
		t.Fatal("real (non-transient) block must NOT trip cooldown")
	}
	if err := eng.tick(context.Background()); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if runCalls != 2 {
		t.Fatalf("no cooldown → B must dispatch on tick 2; RunTask calls = %d", runCalls)
	}
}

func TestIsTransientInfra(t *testing.T) {
	cases := map[string]bool{
		"retries exhausted: plan error: API Error: 529 [1305][该模型当前访问量过大]": true,
		"API Error: 529":                true,
		"upstream rate limit exceeded":  true,
		"503 service unavailable":       true,
		"verify rejected: missing test": false,
		"retries exhausted: plan error: claude -p: context canceled": false,
		"": false,
	}
	for detail, want := range cases {
		if got := isTransientInfra(detail); got != want {
			t.Errorf("isTransientInfra(%q) = %v, want %v", detail, got, want)
		}
	}
}

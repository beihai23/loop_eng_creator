package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
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
func (f *scriptedChannel) ListReplies(ctx context.Context, refs []string, since time.Time) (map[string][]channel.Reply, error) {
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
func (f *scriptedChannel) CloseIssue(ctx context.Context, ref string) error           { return nil }
func (f *scriptedChannel) GetTaskStates(ctx context.Context, refs []string) (map[string]channel.TaskState, error) {
	return nil, nil
}

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

func (f *cancelChannel) ListReplies(ctx context.Context, refs []string, since time.Time) (map[string][]channel.Reply, error) {
	return nil, nil
}
func (f *cancelChannel) PostComment(ctx context.Context, ref, body string) error    { return nil }
func (f *cancelChannel) UpdateStatus(ctx context.Context, ref, status string) error { return nil }
func (f *cancelChannel) CloseIssue(ctx context.Context, ref string) error           { return nil }
func (f *cancelChannel) GetTaskStates(ctx context.Context, refs []string) (map[string]channel.TaskState, error) {
	return nil, nil
}

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
		"API Error: 529":                                             true,
		"upstream rate limit exceeded":                               true,
		"503 service unavailable":                                    true,
		"verify rejected: missing test":                              false,
		"retries exhausted: plan error: claude -p: context canceled": false,
		"": false,
	}
	for detail, want := range cases {
		if got := isTransientInfra(detail); got != want {
			t.Errorf("isTransientInfra(%q) = %v, want %v", detail, got, want)
		}
	}
}

// TestDrainCommandsResumeAndCancel is the step-3.5 acceptance test (spec §7):
// a "resume" command on a needs-review task flips it → new and records the
// payload as resume feedback; a "cancel" command on a blocked task flips it →
// cancelled. Both commands are marked applied (PendingCommands drains to 0).
// 测试直接驱动 drainCommands —— 同 package，无需导出。
func TestDrainCommandsResumeAndCancel(t *testing.T) {
	st := newTestStore(t)
	ch := &scriptedChannel{} // drainCommands 不触碰 channel
	eng := &Engine{Channel: ch, Store: st, Interval: time.Second}

	t1, err := st.InsertTask(state.TaskRow{IssueRef: "o/r#1", Description: "d"})
	if err != nil {
		t.Fatalf("insert t1: %v", err)
	}
	t2, err := st.InsertTask(state.TaskRow{IssueRef: "o/r#2", Description: "d"})
	if err != nil {
		t.Fatalf("insert t2: %v", err)
	}
	if err := st.AppendTransition(t1, "new", "needs-review", "parked"); err != nil {
		t.Fatalf("transition t1: %v", err)
	}
	if err := st.AppendTransition(t2, "new", "blocked", "exhausted"); err != nil {
		t.Fatalf("transition t2: %v", err)
	}

	if err := st.InsertCommand(t1, "resume", "please retry with fix X"); err != nil {
		t.Fatalf("insert resume cmd: %v", err)
	}
	if err := st.InsertCommand(t2, "cancel", ""); err != nil {
		t.Fatalf("insert cancel cmd: %v", err)
	}

	if err := eng.drainCommands(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}

	// t1: needs-review → new，反馈落盘
	if got := statusOf(t, st, t1); got != "new" {
		t.Fatalf("t1 status=%q want new", got)
	}
	fb, err := st.PopResumeFeedback(t1)
	if err != nil {
		t.Fatalf("pop feedback t1: %v", err)
	}
	if !strings.Contains(fb, "fix X") {
		t.Fatalf("t1 feedback=%q want contains 'fix X'", fb)
	}
	// t2: blocked → cancelled
	if got := statusOf(t, st, t2); got != "cancelled" {
		t.Fatalf("t2 status=%q want cancelled", got)
	}
	// 全部已应用（pending 清零）
	pend, err := st.PendingCommands()
	if err != nil {
		t.Fatalf("pending cmds: %v", err)
	}
	if len(pend) != 0 {
		t.Fatalf("pending=%d want 0", len(pend))
	}
}

// TestDrainCommandsIdempotent 证明 drainCommands 对终态任务幂等：对已 done 的
// 任务 cancel 是 no-op（状态保持 done），但命令仍标记 applied（spec §7：
// running/done/cancelled → 不动作，只回写 applied_at，下 tick 不重试）。
func TestDrainCommandsIdempotent(t *testing.T) {
	st := newTestStore(t)
	ch := &scriptedChannel{}
	eng := &Engine{Channel: ch, Store: st, Interval: time.Second}

	t1, err := st.InsertTask(state.TaskRow{IssueRef: "o/r#1", Description: "d"})
	if err != nil {
		t.Fatalf("insert t1: %v", err)
	}
	if err := st.AppendTransition(t1, "new", "done", "ran"); err != nil {
		t.Fatalf("transition t1: %v", err)
	}
	if err := st.InsertCommand(t1, "cancel", ""); err != nil {
		t.Fatalf("insert cancel cmd: %v", err)
	}

	if err := eng.drainCommands(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if got := statusOf(t, st, t1); got != "done" {
		t.Fatalf("done task flipped to %q (must stay done)", got)
	}
	// cancel 命令虽是 no-op，仍标记 applied（不会下 tick 重试）
	pend, err := st.PendingCommands()
	if err != nil {
		t.Fatalf("pending cmds: %v", err)
	}
	if len(pend) != 0 {
		t.Fatalf("pending=%d want 0 (cancel on terminal must still be marked applied)", len(pend))
	}
}

// growingChannel models a channel whose visible task set grows over time:
// ListNewTasks always returns the *current* snapshot (no per-call advance), and
// add() appends a task mid-flight. This simulates a human filing a new issue
// (#31) while the daemon is busy running another task — the core scenario the
// background ingest goroutine exists to cover.
type growingChannel struct {
	mu    sync.Mutex
	tasks []channel.Task
}

func (c *growingChannel) add(t ...channel.Task) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tasks = append(c.tasks, t...)
}

func (c *growingChannel) snapshot() []channel.Task {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]channel.Task, len(c.tasks))
	copy(out, c.tasks)
	return out
}

func (c *growingChannel) ListNewTasks(ctx context.Context) ([]channel.Task, error) {
	return c.snapshot(), nil
}
func (c *growingChannel) ListReplies(ctx context.Context, refs []string, since time.Time) (map[string][]channel.Reply, error) {
	return nil, nil
}
func (c *growingChannel) PostComment(ctx context.Context, ref, body string) error    { return nil }
func (c *growingChannel) UpdateStatus(ctx context.Context, ref, status string) error { return nil }
func (c *growingChannel) CloseIssue(ctx context.Context, ref string) error           { return nil }
func (c *growingChannel) GetTaskStates(ctx context.Context, refs []string) (map[string]channel.TaskState, error) {
	return nil, nil
}

// flakyChannel fails ListNewTasks for the first failN calls (transient upstream
// error), then returns the configured task. Used to prove the background ingest
// loop logs the error and retries on the next jitter rather than giving up.
type flakyChannel struct {
	failN int
	task  channel.Task
	mu    sync.Mutex
	calls int
}

func (c *flakyChannel) ListNewTasks(ctx context.Context) ([]channel.Task, error) {
	c.mu.Lock()
	c.calls++
	n := c.calls
	c.mu.Unlock()
	if n <= c.failN {
		return nil, fmt.Errorf("upstream flaky (call %d)", n)
	}
	return []channel.Task{c.task}, nil
}
func (c *flakyChannel) ListReplies(ctx context.Context, refs []string, since time.Time) (map[string][]channel.Reply, error) {
	return nil, nil
}
func (c *flakyChannel) PostComment(ctx context.Context, ref, body string) error    { return nil }
func (c *flakyChannel) UpdateStatus(ctx context.Context, ref, status string) error { return nil }
func (c *flakyChannel) CloseIssue(ctx context.Context, ref string) error           { return nil }
func (c *flakyChannel) GetTaskStates(ctx context.Context, refs []string) (map[string]channel.TaskState, error) {
	return nil, nil
}

// waitUntil polls cond every few ms until it returns true or the timeout
// elapses. Used by the background-ingest tests to observe asynchronous
// ingestion without fixed sleeps (the goroutine runs on a small jitter).
func waitUntil(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(3 * time.Millisecond)
	}
	t.Fatalf("condition not satisfied within %s", timeout)
}

// TestBackgroundIngestDuringRun is the headline acceptance test for the
// single-active sync-tick blind spot (criterion 3/4/6): while RunTask is
// blocked running task A (the main tick is stuck inside the synchronous
// dispatch), a freshly-filed issue B must still be ingested into state.db by
// the background goroutine — and the dashboard (reading state.db) would see it.
// B lands as status="new" (ingest only inserts; it never takes the active slot),
// and A stays "running" the whole time.
//
// To prove criterion 6 (dashboard sees the runtime-ingested task), the test also
// opens a SECOND Store on the same DB file — exactly how the dashboard command
// opens its own connection pool — and asserts that reader sees B too, live.
func TestBackgroundIngestDuringRun(t *testing.T) {
	// Open the daemon's store on an explicit path so the "dashboard" reader can
	// open the same file.
	dbPath := t.TempDir() + "/state.db"
	st, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("open daemon store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	// The dashboard: its own *state.Store on the same DB (own connection pool /
	// process). WAL lets it read committed writes without blocking on the daemon.
	dash, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("open dashboard store: %v", err)
	}
	t.Cleanup(func() { dash.Close() })

	ch := &growingChannel{}
	ch.add(channel.Task{Ref: "A", Description: "task A", TaskType: "feat"})

	runStarted := make(chan struct{}) // closed once RunTask(A) is actually executing
	proceed := make(chan struct{})    // test closes it to let RunTask(A) return

	var aID string
	eng := &Engine{
		Channel: ch, Store: st, Interval: time.Second,
		// tiny jitter so the test observes ingestion within ms, not seconds
		IngestMin: 2 * time.Millisecond,
		IngestMax: 8 * time.Millisecond,
		RunTask: func(ctx context.Context, task state.TaskRow) (string, string, error) {
			aID = task.ID
			close(runStarted)
			// Block here — this is the "task runs for minutes" window during
			// which the synchronous tick would otherwise never poll again.
			select {
			case <-proceed:
			case <-ctx.Done():
			}
			return "done", "", nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- eng.Run(ctx) }()

	<-runStarted // A is running: active slot busy, main tick blocked in RunTask

	// While A is still running, file issue B on the channel.
	ch.add(channel.Task{Ref: "B", Description: "task B", TaskType: "feat"})

	// The background ingest goroutine must persist B even though the main tick
	// is stuck. (With the old sync-only tick, B would never appear here until A
	// finished.)
	waitUntil(t, 2*time.Second, func() bool {
		refs, err := st.IssueRefs()
		return err == nil && refs["A"] && refs["B"]
	})

	// Invariant (criterion 4): ingest only inserts. B is status="new", A is
	// still "running" — the active slot was NOT stolen by ingestion.
	statuses, err := st.ListStatuses()
	if err != nil {
		t.Fatalf("list statuses: %v", err)
	}
	byID := map[string]string{}
	for _, sr := range statuses {
		byID[sr.ID] = sr.Status
	}
	if len(statuses) != 2 {
		t.Fatalf("expected 2 tasks (A running + B new), got %+v", statuses)
	}
	if byID[aID] != "running" {
		t.Fatalf("A must still be running while B was ingested, got %q", byID[aID])
	}

	// Criterion 6: the dashboard's OWN store sees B live, while A is still
	// running. This is the real-time pending-list update the bug blocked.
	dashViews, err := dash.TasksByStatus()
	if err != nil {
		t.Fatalf("dashboard TasksByStatus: %v", err)
	}
	var sawBNew bool
	for _, v := range dashViews {
		if v.IssueRef == "B" && v.Status == "new" {
			sawBNew = true
		}
	}
	if !sawBNew {
		t.Fatalf("dashboard did not see runtime-ingested B as new: %+v", dashViews)
	}

	// Let A finish and shut the daemon down cleanly (Run waits for the ingest
	// goroutine before returning).
	close(proceed)
	cancel()
	select {
	case err := <-runErr:
		if err != nil && err != context.Canceled {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after shutdown (ingest goroutine leaked?)")
	}
}

// TestIngestLoopRetriesOnError proves criterion 5: a transient ListNewTasks
// failure is logged and retried on the next jitter, never affecting the main
// loop. The flaky channel errors the first few calls, then succeeds; the
// background loop must eventually ingest the task despite the earlier errors.
func TestIngestLoopRetriesOnError(t *testing.T) {
	st := newTestStore(t)
	ch := &flakyChannel{failN: 3, task: channel.Task{Ref: "X", Description: "task X", TaskType: "feat"}}

	var buf bytes.Buffer
	eng := &Engine{
		Channel: ch, Store: st, Interval: time.Second,
		IngestMin: 1 * time.Millisecond,
		IngestMax: 3 * time.Millisecond,
		Log:       log.New(&buf, "", 0),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	eng.wg.Add(1)
	go eng.ingestLoop(ctx)

	// Despite 3 consecutive ListNewTasks errors, the loop retries and ingests X.
	waitUntil(t, 2*time.Second, func() bool {
		refs, err := st.IssueRefs()
		return err == nil && refs["X"]
	})

	// Stop the goroutine BEFORE reading buf: bytes.Buffer is not concurrency-safe,
	// and ingestLoop keeps running until cancelled. The 3 error lines were written
	// during the failing calls, well before X landed — so they survive shutdown.
	cancel()
	eng.wg.Wait() // ingestLoop exits promptly on ctx cancel

	// The transient errors were logged (proving they were observed, not panic'd).
	if !strings.Contains(buf.String(), "background ingest error") {
		t.Fatalf("expected logged ingest errors, got log:\n%s", buf.String())
	}
}

// TestNextIngestDelayJitter pins the jitter contract: the delay always lands in
// [IngestMin, IngestMax) and varies across draws (not a fixed beat).
func TestNextIngestDelayJitter(t *testing.T) {
	eng := &Engine{IngestMin: 3 * time.Second, IngestMax: 10 * time.Second}
	seen := map[time.Duration]bool{}
	for i := 0; i < 1000; i++ {
		d := eng.nextIngestDelay()
		if d < 3*time.Second || d >= 10*time.Second {
			t.Fatalf("delay %s outside [3s, 10s)", d)
		}
		seen[d] = true
	}
	// Randomized → many distinct values across 1000 draws (not one fixed delay).
	if len(seen) < 100 {
		t.Fatalf("jitter produced only %d distinct delays in 1000 draws; not randomized?", len(seen))
	}

	// Degenerate range (max <= min) collapses to min with no panic.
	eng2 := &Engine{IngestMin: 5 * time.Second, IngestMax: 5 * time.Second}
	if got := eng2.nextIngestDelay(); got != 5*time.Second {
		t.Fatalf("degenerate range: got %s want 5s", got)
	}
	eng3 := &Engine{IngestMin: 0, IngestMax: 0}
	if got := eng3.nextIngestDelay(); got != 0 {
		t.Fatalf("zero range: got %s want 0", got)
	}
}

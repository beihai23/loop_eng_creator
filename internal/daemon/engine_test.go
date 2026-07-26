package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"loop-eng/internal/channel"
	"loop-eng/internal/skill"
	"loop-eng/internal/state"
)

// scriptedChannel hands ListNewTasks a scripted sequence of task batches,
// advancing one batch per call, and hands ListReplies a fixed reply map (set by
// the test to simulate a human answering a parked task). PostComment /
// UpdateStatus are no-ops — writeback belongs to the SubLoop, not the daemon
// tick under test.
type scriptedChannel struct {
	batches  [][]channel.Task
	replies  map[string][]channel.Reply // human replies per issue ref; nil/empty → none
	calls    int
	comments []string // PostComment 收到的正文（按序），供断言写回内容
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
func (f *scriptedChannel) PostComment(ctx context.Context, ref, body string) error {
	f.comments = append(f.comments, body)
	return nil
}
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

// TestDrainCommandsResumeCancelled 守住：cancel 后反悔的自助通道——对 cancelled
// 任务发 resume 命令，drainCommands 应迁 cancelled→new、把 payload 落盘为 resume
// 反馈、命令标记 applied。复用 newTestStore/statusOf/scriptedChannel（同 package）。
func TestDrainCommandsResumeCancelled(t *testing.T) {
	st := newTestStore(t)
	eng := &Engine{Channel: &scriptedChannel{}, Store: st, Interval: time.Second}

	id, err := st.InsertTask(state.TaskRow{IssueRef: "o/r#1", Description: "d"})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := st.AppendTransition(id, "new", "cancelled", "cancelled by TUI"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if err := st.InsertCommand(id, "resume", "retry with fix Y"); err != nil {
		t.Fatalf("insert resume cmd: %v", err)
	}

	if err := eng.drainCommands(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}

	// cancelled → new
	if got := statusOf(t, st, id); got != "new" {
		t.Fatalf("status=%q want new", got)
	}
	// payload 落盘为 resume 反馈
	fb, err := st.PopResumeFeedback(id)
	if err != nil {
		t.Fatalf("pop feedback: %v", err)
	}
	if !strings.Contains(fb, "fix Y") {
		t.Fatalf("feedback=%q want contains 'fix Y'", fb)
	}
	// transitions 含一条 cancelled→new，reason 前缀 tui resume:
	trans, err := st.Transitions(id)
	if err != nil {
		t.Fatalf("transitions: %v", err)
	}
	var saw bool
	for _, tr := range trans {
		if tr.From == "cancelled" && tr.To == "new" && strings.HasPrefix(tr.Reason, "tui resume:") {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("no cancelled→new resume transition in %+v", trans)
	}
	// 命令标记 applied（pending 清零）
	pend, err := st.PendingCommands()
	if err != nil {
		t.Fatalf("pending cmds: %v", err)
	}
	if len(pend) != 0 {
		t.Fatalf("pending=%d want 0", len(pend))
	}
}

// TestDrainCommandsResumeIdempotentTerminal 回归：把 cancelled 加进 resume 白
// 名单后，done/error 仍应是 no-op——不迁移、不写 feedback，只回写 applied_at。
func TestDrainCommandsResumeIdempotentTerminal(t *testing.T) {
	st := newTestStore(t)
	eng := &Engine{Channel: &scriptedChannel{}, Store: st, Interval: time.Second}

	mk := func(term string) string {
		id, err := st.InsertTask(state.TaskRow{IssueRef: "o/r#" + term, Description: "d"})
		if err != nil {
			t.Fatalf("insert %s: %v", term, err)
		}
		if err := st.AppendTransition(id, "new", term, "ran"); err != nil {
			t.Fatalf("transition %s: %v", term, err)
		}
		if err := st.InsertCommand(id, "resume", "should be ignored"); err != nil {
			t.Fatalf("insert resume cmd %s: %v", term, err)
		}
		return id
	}
	doneID := mk("done")
	errID := mk("error")

	if err := eng.drainCommands(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}

	for _, c := range []struct{ id, want string }{{doneID, "done"}, {errID, "error"}} {
		if got := statusOf(t, st, c.id); got != c.want {
			t.Fatalf("status=%q want %q (resume must be idempotent)", got, c.want)
		}
		fb, err := st.PopResumeFeedback(c.id)
		if err != nil || fb != "" {
			t.Fatalf("%s feedback=%q want empty (no side effect)", c.want, fb)
		}
		trans, err := st.Transitions(c.id)
		if err != nil {
			t.Fatalf("transitions %s: %v", c.want, err)
		}
		for _, tr := range trans {
			if tr.To == "new" {
				t.Fatalf("%s task got a →new transition %+v (must be no-op)", c.want, tr)
			}
		}
	}
	pend, err := st.PendingCommands()
	if err != nil {
		t.Fatalf("pending cmds: %v", err)
	}
	if len(pend) != 0 {
		t.Fatalf("pending=%d want 0 (terminal resume must still be marked applied)", len(pend))
	}
}

// TestDrainCommandsCrashMidIdempotent 钉死 drainCommands 的 crash-中途幂等契约
// （原 applyCommand + MarkCommandApplied 两步非事务的原子替代）。构造旧两步代码的
// crash 残留态：任务 blocked 上挂一条 pending resume 命令，先用 AppendTransition
// 模拟旧 applyCommand 已提交 transition（status 已翻 new）但 crash 在 MarkCommandApplied
// 之前（命令仍 pending）；再调 drainCommands（= 重启重 drain）。断言 transitions 行数
// 不变（无重复）、无 from_status="" 的行（无错 from_status）、命令现已 applied。
// 并附 happy-path 双 drain：fresh resume-on-blocked 连 drain 两次只产一条 blocked→new。
func TestDrainCommandsCrashMidIdempotent(t *testing.T) {
	st := newTestStore(t)
	eng := &Engine{Channel: &scriptedChannel{}, Store: st, Interval: time.Second}

	id, err := st.InsertTask(state.TaskRow{IssueRef: "o/r#1", Description: "d"})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := st.AppendTransition(id, "new", "blocked", "exhausted"); err != nil {
		t.Fatalf("park: %v", err)
	}
	if err := st.InsertCommand(id, "resume", "retry fix Z"); err != nil {
		t.Fatalf("insert cmd: %v", err)
	}

	// 模拟旧两步代码的 crash 残留：applyCommand 已提交 transition（blocked→new，
	// status 已翻 new）但 crash 落在 MarkCommandApplied 之前——命令仍 pending。
	if err := st.AppendTransition(id, "blocked", "new", "tui resume: retry fix Z"); err != nil {
		t.Fatalf("simulate pre-crash transition: %v", err)
	}

	transBefore, err := st.Transitions(id)
	if err != nil {
		t.Fatalf("transitions before: %v", err)
	}
	before := len(transBefore)

	// 重启重 drain：命令幂等标记 applied，不写重复 transition（task 已不在 resumable 集）。
	if err := eng.drainCommands(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}

	transAfter, err := st.Transitions(id)
	if err != nil {
		t.Fatalf("transitions after: %v", err)
	}
	if len(transAfter) != before {
		t.Fatalf("re-drain wrote a duplicate transition: before=%d after=%d %+v", before, len(transAfter), transAfter)
	}
	for _, tr := range transAfter {
		if tr.From == "" {
			t.Fatalf("transition with empty from_status (wrong from_status): %+v", tr)
		}
	}
	pend, err := st.PendingCommands()
	if err != nil {
		t.Fatalf("pending cmds: %v", err)
	}
	if len(pend) != 0 {
		t.Fatalf("pending=%d want 0 (crash-restart re-drain must mark applied)", len(pend))
	}

	// Happy-path 双 drain：fresh resume-on-blocked 连 drain 两次只产一条 blocked→new。
	id2, err := st.InsertTask(state.TaskRow{IssueRef: "o/r#2", Description: "d"})
	if err != nil {
		t.Fatalf("insert id2: %v", err)
	}
	if err := st.AppendTransition(id2, "new", "blocked", "exhausted"); err != nil {
		t.Fatalf("park id2: %v", err)
	}
	if err := st.InsertCommand(id2, "resume", "go"); err != nil {
		t.Fatalf("insert cmd id2: %v", err)
	}
	countBlockedToNew := func() int {
		trans, err := st.Transitions(id2)
		if err != nil {
			t.Fatalf("transitions id2: %v", err)
		}
		n := 0
		for _, tr := range trans {
			if tr.To == "new" && tr.From == "blocked" {
				n++
			}
		}
		return n
	}
	if err := eng.drainCommands(context.Background()); err != nil {
		t.Fatalf("drain id2 #1: %v", err)
	}
	if err := eng.drainCommands(context.Background()); err != nil {
		t.Fatalf("drain id2 #2: %v", err)
	}
	if n := countBlockedToNew(); n != 1 {
		t.Fatalf("fresh resume-on-blocked double drain: blocked→new transitions=%d want exactly 1", n)
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

// TestIngestResyncsEditedSpec 钉死「正文重新摄入」：issue 正文被编辑后，下一次 ingest
// 把新 desc/criteria 回写进 tasks 行（不新增任务、不改 status）；正文未变时不产生
// 回写（updated_at 不动）。数据来自每次轮询已有的 ListNewTasks 载荷——零额外 channel 读。
func TestIngestResyncsEditedSpec(t *testing.T) {
	st := newTestStore(t)
	ch := &scriptedChannel{batches: [][]channel.Task{
		{
			{Ref: "A", Description: "task A v1", TaskType: "feat", AcceptanceCriteria: []string{"c1"}},
			{Ref: "B", Description: "task B", TaskType: "feat"},
		},
		{
			// A 的正文+验收标准被编辑；B 原样。
			{Ref: "A", Description: "task A v2 (edited)", TaskType: "feat", AcceptanceCriteria: []string{"c1", "c2"}},
			{Ref: "B", Description: "task B", TaskType: "feat"},
		},
		{
			// 再来一轮原样——确认无 no-op 回写。
			{Ref: "A", Description: "task A v2 (edited)", TaskType: "feat", AcceptanceCriteria: []string{"c1", "c2"}},
			{Ref: "B", Description: "task B", TaskType: "feat"},
		},
	}}
	eng := &Engine{Channel: ch, Store: st, Interval: time.Second}

	if err := eng.tick(context.Background()); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	specs1, _ := st.TaskSpecsByRef()
	if specs1["A"].Description != "task A v1" {
		t.Fatalf("前置：A 应为 v1, got %q", specs1["A"].Description)
	}

	if err := eng.tick(context.Background()); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	specs2, _ := st.TaskSpecsByRef()
	// A 被重新摄入：desc + criteria 都换成 v2。
	if specs2["A"].Description != "task A v2 (edited)" {
		t.Fatalf("A 正文未刷新: %q", specs2["A"].Description)
	}
	if len(specs2["A"].Criteria) != 2 || specs2["A"].Criteria[1] != "c2" {
		t.Fatalf("A 验收标准未刷新: %+v", specs2["A"].Criteria)
	}
	// 刷新不产生重复任务，也不改 status（仍是 new 等派发）。
	statuses, _ := st.ListStatuses()
	if len(statuses) != 2 {
		t.Fatalf("刷新不应新增任务, got %d statuses", len(statuses))
	}
	for _, s := range statuses {
		if s.Status != "new" {
			t.Fatalf("task %s status=%s, 刷新不应触碰 status", s.ID, s.Status)
		}
	}

	// tick 3 内容未变：updated_at 不得再动（无 no-op 回写）。
	updatedAtBefore := specs2["A"].UpdatedAt
	if err := eng.tick(context.Background()); err != nil {
		t.Fatalf("tick 3: %v", err)
	}
	specs3, _ := st.TaskSpecsByRef()
	if specs3["A"].UpdatedAt != updatedAtBefore {
		t.Fatalf("正文未变不应回写: updated_at %s → %s", updatedAtBefore, specs3["A"].UpdatedAt)
	}
}

// TestIngestResyncsBodyOnlyEdit 钉死「仅叙述文字变更也触发重新摄入」：desc/criteria
// 不变、只改了正文的背景段落（全文 body 变）→ 照样回写。全文保留后，plan/execute
// 吃 body，只比对蒸馏字段会漏掉这类编辑。
func TestIngestResyncsBodyOnlyEdit(t *testing.T) {
	st := newTestStore(t)
	ch := &scriptedChannel{batches: [][]channel.Task{
		{{Ref: "A", Description: "task A", TaskType: "feat", Body: "task A\n\n背景 v1"}},
		{{Ref: "A", Description: "task A", TaskType: "feat", Body: "task A\n\n背景 v2（补充了约束）"}},
	}}
	eng := &Engine{Channel: ch, Store: st, Interval: time.Second}

	if err := eng.tick(context.Background()); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if err := eng.tick(context.Background()); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	specs, _ := st.TaskSpecsByRef()
	if specs["A"].Body != "task A\n\n背景 v2（补充了约束）" {
		t.Fatalf("body-only 编辑未触发重新摄入: %q", specs["A"].Body)
	}
}

// TestTriageGate 钉死 triage 派发门的三条分支 + 唤醒回路：
//  1. !startable → needs-info：不占活跃位（RunTask 未被调用），issue 收到缺信息评论；
//  2. 人补充信息（评论）→ pollSignals 唤醒 → 重新分诊（这次 startable）→ 正常派发；
//  3. needs_human_decision → needs-human-decision 挂起。
//
// 同时断言 TriageFunc 拿到全文 Body（分诊判断「缺不缺信息」的输入）。
func TestTriageGate(t *testing.T) {
	st := newTestStore(t)
	ch := &scriptedChannel{
		batches: [][]channel.Task{
			{{Ref: "A", Description: "首行描述", TaskType: "feat", Body: "首行描述\n\n## 背景\n全文要到达分诊"}},
			nil, nil, // 后续 tick 无新任务
		},
		replies: map[string][]channel.Reply{},
	}
	var ran bool
	var triageCalls int
	var triageBody string
	eng := &Engine{
		Channel: ch, Store: st, Interval: time.Second,
		RunTask: func(ctx context.Context, task state.TaskRow) (string, string, error) {
			ran = true
			return "done", "", nil
		},
		Triage: func(ctx context.Context, task state.TaskRow) (skill.TriageOutput, error) {
			triageCalls++
			triageBody = task.Body
			if triageCalls == 1 {
				return skill.TriageOutput{
					Startable: false, MissingInfo: []string{"要改哪个接口"}, Reason: "无法定位改动点",
				}, nil
			}
			return skill.TriageOutput{Startable: true, LoopDoable: true, Difficulty: "low"}, nil
		},
	}

	// tick 1：分诊拦下 → needs-info，RunTask 未被调用，评论含缺信息清单。
	if err := eng.tick(context.Background()); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if ran {
		t.Fatal("!startable 时 RunTask 不应被调用")
	}
	statuses, _ := st.ListStatuses()
	if len(statuses) != 1 || statuses[0].Status != "needs-info" {
		t.Fatalf("task 应为 needs-info, got %+v", statuses)
	}
	if len(ch.comments) != 1 || !strings.Contains(ch.comments[0], "要改哪个接口") || !strings.Contains(ch.comments[0], "NEEDS-INFO") {
		t.Fatalf("缺信息评论未发出或内容不对: %+v", ch.comments)
	}
	if triageBody != "首行描述\n\n## 背景\n全文要到达分诊" {
		t.Fatalf("TriageFunc 未拿到全文 Body: %q", triageBody)
	}

	// 人补充信息 → tick 2：pollSignals 唤醒（needs-info → new）→ 重新分诊 startable → 派发跑完。
	ch.replies["A"] = []channel.Reply{{Body: "改 channel.Linear 接口"}}
	if err := eng.tick(context.Background()); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if !ran {
		t.Fatal("唤醒+分诊通过后 RunTask 应被调用")
	}
	statuses, _ = st.ListStatuses()
	if statuses[0].Status != "done" {
		t.Fatalf("唤醒后应跑完 done, got %s", statuses[0].Status)
	}
	if triageCalls != 2 {
		t.Fatalf("唤醒后应重新分诊, triageCalls=%d", triageCalls)
	}
}

// TestTriageGateHumanDecision：needs_human_decision → needs-human-decision 挂起，
// 不占活跃位；分诊器报错不 gate（照跑，可用性优先）。
func TestTriageGateHumanDecision(t *testing.T) {
	st := newTestStore(t)
	ch := &scriptedChannel{batches: [][]channel.Task{
		{{Ref: "A", Description: "deploy to prod", TaskType: "deploy"}},
		{{Ref: "B", Description: "task B", TaskType: "feat"}},
	}}
	var ran []string
	triageN := 0
	eng := &Engine{
		Channel: ch, Store: st, Interval: time.Second,
		RunTask: func(ctx context.Context, task state.TaskRow) (string, string, error) {
			ran = append(ran, task.IssueRef)
			return "done", "", nil
		},
		Triage: func(ctx context.Context, task state.TaskRow) (skill.TriageOutput, error) {
			triageN++
			if task.IssueRef == "A" {
				return skill.TriageOutput{Startable: true, LoopDoable: true, NeedsHumanDecision: true, Reason: "生产部署需人批准"}, nil
			}
			return skill.TriageOutput{}, errors.New("triage backend down")
		},
	}

	// tick 1：A 被挂起到 needs-human-decision。
	if err := eng.tick(context.Background()); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	statusOf := func(ref string) string {
		specs, _ := st.TaskSpecsByRef()
		rows, _ := st.ListStatuses()
		for _, r := range rows {
			if r.ID == specs[ref].ID {
				return r.Status
			}
		}
		return ""
	}
	if s := statusOf("A"); s != "needs-human-decision" {
		t.Fatalf("A 应为 needs-human-decision, got %s", s)
	}
	if len(ran) != 0 {
		t.Fatalf("挂起任务不应运行, ran=%v", ran)
	}
	if len(ch.comments) != 1 || !strings.Contains(ch.comments[0], "NEEDS-HUMAN-DECISION") {
		t.Fatalf("人审评论未发出: %+v", ch.comments)
	}

	// tick 2：B 的分诊器报错 → 不 gate，照常派发。
	if err := eng.tick(context.Background()); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if len(ran) != 1 || ran[0] != "B" {
		t.Fatalf("triage 报错不应阻塞派发, ran=%v", ran)
	}
}

// TestZeroGainBlockedResumesWithFeedback 钉死 DoD 3 的 daemon 段：SubLoop 零增益结局把任务
// 落成 blocked（detail 注明零增益）→ 人在 issue 评论补充信息 → pollSignals 把它从 blocked
// 重排队到 new 并把回复落成 ResumeFeedback → 下一轮 SubLoop 经 PopResumeFeedback（subloop.go
// 的既有通道）携带新信息重试。本测试只驱动 daemon 段（直接模拟 SubLoop 的 blocked 结局 +
// SetLastCommentAt），不断言 SubLoop 内部。
func TestZeroGainBlockedResumesWithFeedback(t *testing.T) {
	st := newTestStore(t)
	ch := &scriptedChannel{}
	eng := &Engine{Channel: ch, Store: st, Interval: time.Second}

	taskID, err := st.InsertTask(state.TaskRow{IssueRef: "A", Description: "零增益任务"})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	// 模拟 SubLoop 零增益结局：running→blocked，detail 注明零增益（escalateZeroGain→report
	// 会做这步；这里直接落 transition 以隔离 daemon 段）。
	if err := st.AppendTransition(taskID, "running", "blocked",
		"重试零增益提前终止（连续两轮失败签名相同，重试不会自愈）。签名: x"); err != nil {
		t.Fatalf("transition: %v", err)
	}
	// report() 在 PostComment 成功后会 SetLastCommentAt——pollSignals 据此判定「新」回复。
	if err := st.SetLastCommentAt(taskID, time.Now()); err != nil {
		t.Fatalf("set last comment at: %v", err)
	}
	// 人在 issue 回复（携带新信息：修订后的实现合同）。
	ch.replies = map[string][]channel.Reply{"A": {{Body: "合同是 plan 冻结 3 个值，execute 按这个改"}}}

	if err := eng.pollSignals(context.Background()); err != nil {
		t.Fatalf("pollSignals: %v", err)
	}

	// 零增益 blocked 任务在人回复后被重排队到 new。
	if got := statusOf(t, st, taskID); got != "new" {
		t.Fatalf("零增益 blocked 任务人回复后应重排队为 new, got %q", got)
	}
	// transitions 含 blocked→new 且 reason 含回复原文。
	trans, err := st.Transitions(taskID)
	if err != nil {
		t.Fatalf("transitions: %v", err)
	}
	var resumed bool
	for _, tr := range trans {
		if tr.From == "blocked" && tr.To == "new" && strings.Contains(tr.Reason, "合同是 plan 冻结 3 个值") {
			resumed = true
		}
	}
	if !resumed {
		t.Fatalf("缺 blocked→new 且含回复原文的 transition: %+v", trans)
	}
	// PopResumeFeedback 返回非空——下一轮 SubLoop 经 subloop.go 的 PopResumeFeedback 通道
	// 注入 priorFailure 的新信息（携带人回复重试）。
	fb, err := st.PopResumeFeedback(taskID)
	if err != nil {
		t.Fatalf("pop resume feedback: %v", err)
	}
	if strings.TrimSpace(fb) == "" {
		t.Fatalf("PopResumeFeedback 应返回非空（携带人回复作为下轮新信息）, got %q", fb)
	}
	if !strings.Contains(fb, "合同是 plan 冻结 3 个值") {
		t.Fatalf("resume feedback 应含回复原文, got %q", fb)
	}
}

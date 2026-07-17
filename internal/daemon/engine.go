// Package daemon implements the resident M3 control loop (Engine): a slow
// ticker that polls the ticket channel, ingests newly-arrived tasks into the
// durable FIFO, and drives a single active SubLoop to completion. The daemon
// never blocks on a human (spec principle 7): tier-3 human review parks the
// task, freeing the active slot, and is resumed on a later tick.
package daemon

import (
	"context"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"strings"
	"sync"
	"time"

	"loop-eng/internal/channel"
	"loop-eng/internal/state"
)

// RunTaskFunc runs one task to completion and returns its terminal status
// (e.g. "done", "blocked") plus the outcome Detail (the failure reason on a
// block — used by the engine to classify transient-infra blocks for cooldown).
// It is injected so the daemon stays decoupled from the concrete SubLoop wiring.
// The daemon owns the task lifecycle bookkeeping (status transitions, spec §8.3
// step 3/4); RunTask just does the work and reports the outcome — it must NOT
// mutate task_status itself, so the daemon is the single writer of the lifecycle.
type RunTaskFunc func(ctx context.Context, task state.TaskRow) (status, detail string, err error)

// Engine is the resident M3 daemon. It polls Channel every Interval, ingesting
// new tasks into Store (the durable FIFO) deduped by issue_ref, resuming parked
// tasks whose human has replied, then dispatching the head of the FIFO by
// running it through RunTask. The active slot is implicit (single-active by
// construction — one synchronous RunTask per tick, spec §12) and the parked set
// is the durable query Store.ParkedTasks() (status=needs-review), so the Engine
// holds no task-lifecycle state of its own — it is all rebuildable from disk
// (principle 4).
type Engine struct {
	Channel  channel.Channel
	Store    *state.Store
	Interval time.Duration
	Cooldown time.Duration
	RunTask  RunTaskFunc

	// Log is the observability sink for daemon tick events (ingest, dispatch,
	// park, resume). When nil, defaults to os.Stderr with a "[daemon]" prefix.
	// Tests inject a logger backed by bytes.Buffer to assert on output.
	Log *log.Logger

	// IngestMin/IngestMax bound the background ingest loop's per-iteration
	// jittered sleep. When IngestMax > 0, Run starts a goroutine that keeps
	// ingesting newly-filed issues into the durable FIFO *independently* of the
	// synchronous main tick — so an issue filed while the single active slot is
	// busy running a task for minutes still lands in state.db and shows up on the
	// dashboard (the single-active sync-tick blind spot). When IngestMax <= 0
	// (the zero value), no background goroutine runs and the main tick alone
	// ingests (legacy behavior, used by tests that drive tick() directly).
	IngestMin time.Duration
	IngestMax time.Duration

	coolUntil time.Time

	// ingestMu serializes ingest() calls (the main tick's step 1 and the
	// background loop) so the read-IssueRefs-then-InsertTask dedup is atomic
	// w.r.t. itself — without it two concurrent ingests could both miss a ref
	// and double-insert it (issue_ref has no UNIQUE index).
	ingestMu sync.Mutex

	// wg tracks the background ingest goroutine so Run can wait for it to exit
	// cleanly on ctx cancellation before returning.
	wg sync.WaitGroup
}

// logf writes a formatted line to the daemon log (or stderr if Log is nil).
func (e *Engine) logf(format string, args ...interface{}) {
	if e.Log != nil {
		e.Log.Printf(format, args...)
	} else {
		fmt.Fprintf(os.Stderr, format+"\n", args...)
	}
}

// Run is the resident entry point. It ticks once immediately (a fresh daemon
// should not wait a full Interval before its first poll) and then every
// Interval until ctx is cancelled, returning ctx.Err(). A single tick's error
// is logged to stderr but does NOT halt the loop — a transient channel failure
// skips this tick and retries next, losing no persisted state (spec §11).
//
// Because a tick's dispatch step blocks synchronously inside RunTask (single
// active sub-loop, spec §12), the loop does NOT advance while a task is
// running — so a freshly-filed issue would go unseen for the task's whole run.
// When IngestMax > 0, Run therefore starts a background ingest goroutine that
// keeps pulling new issues into the FIFO on a jittered 3–10s cadence,
// independent of the blocked main tick (spec principle: the daemon never
// blocks on a human; this extends "never blocks" to ingestion during a run).
func (e *Engine) Run(ctx context.Context) error {
	if n, err := e.Store.RequeueOrphanedRunning(); err != nil {
		e.logf("[daemon] orphan recovery failed: %v", err)
	} else if n > 0 {
		e.logf("[daemon] recovered %d orphaned running task(s) → new", n)
	}
	if e.IngestMax > 0 {
		e.wg.Add(1)
		go e.ingestLoop(ctx)
	}
	if err := e.tick(ctx); err != nil {
		e.logf("daemon tick error: %v", err)
	}
	t := time.NewTicker(e.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			e.wg.Wait() // let the background ingest goroutine exit cleanly
			return ctx.Err()
		case <-t.C:
			if err := e.tick(ctx); err != nil {
				e.logf("daemon tick error: %v", err)
			}
		}
	}
}

// tick is one daemon pass (spec §7.1). Steps:
//  1. Ingest new tasks from the channel (dedup against persisted tasks).
//  2. Reconcile: read channel-side state for terminal tasks (done/blocked) and
//     detect human-driven reversals — done issue reopened → re-queue as new;
//     blocked issue had its label removed → re-queue as new.
//  3. Poll signals: check needs-review + blocked tasks for new human replies
//     since the daemon's last comment; re-queue any that got a reply.
//  4. Dispatch the FIFO head (single-active synchronous, spec §12).
func (e *Engine) tick(ctx context.Context) error {
	// ---- step 1: ingest new tasks (non-fatal: a flaky channel must not block
	// dispatch of already-ingested ready tasks) ----
	if err := e.ingest(ctx); err != nil {
		e.logf("[daemon] ingest error: %v", err)
	}

	// ---- step 2: reconcile terminal tasks against channel-side state ----
	if err := e.reconcile(ctx); err != nil {
		e.logf("[daemon] reconcile error: %v", err)
	}

	// ---- step 3: poll new human replies on parked + blocked tasks ----
	if err := e.pollSignals(ctx); err != nil {
		e.logf("[daemon] poll signals error: %v", err)
	}

	// ---- step 3.5: drain TUI commands (spec §4.2/§7) ----
	if err := e.drainCommands(ctx); err != nil {
		e.logf("[daemon] drain commands error: %v", err)
	}

	// ---- step 4: dispatch (spec §7.1 step 3) ----
	if e.RunTask == nil {
		return nil
	}
	if now := time.Now(); now.Before(e.coolUntil) {
		e.logf("[daemon] tick dispatch: skip (cooldown until %s)", e.coolUntil.Format(time.TimeOnly))
		return nil
	}
	ready, ok, err := e.Store.NextReadyTask()
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if err := e.Store.AppendTransition(ready.ID, "new", "running", "dispatched"); err != nil {
		return err
	}
	e.logf("[daemon] tick dispatch: task %s (%s) → running", shortTaskID(ready.ID), ready.IssueRef)
	status, detail, err := e.RunTask(ctx, ready)
	if err != nil {
		e.logf("dispatch: task %s (%s) failed: %v", ready.ID, ready.IssueRef, err)
		status = "error"
	}
	if status == "blocked" && isTransientInfra(detail) {
		e.coolUntil = time.Now().Add(e.cooldown())
		e.logf("[daemon] tick park: task %s transient-infra block → cooldown until %s, re-queue",
			shortTaskID(ready.ID), e.coolUntil.Format(time.TimeOnly))
		return e.Store.AppendTransition(ready.ID, "running", "new", "transient infra; re-queued")
	}
	if status == "needs-review" {
		e.logf("[daemon] tick park: task %s parked on tier-3 human review", shortTaskID(ready.ID))
	} else {
		e.logf("[daemon] tick done: task %s → %s", shortTaskID(ready.ID), status)
	}
	return e.Store.AppendTransition(ready.ID, "running", status, "ran")
}

// ingest pulls new tasks from the channel into the durable FIFO, deduped by
// issue_ref against already-persisted tasks (spec §7.1 step 1). It is called
// both from the synchronous tick (step 1) and from the background ingestLoop
// goroutine, so it serializes on ingestMu: without the lock, two concurrent
// ingests could both read IssueRefs before either inserts, and each would
// double-insert the same ref (issue_ref has no UNIQUE index).
//
// Ingest is INSERT-only: it inserts tasks at status="new" and touches nothing
// else. It never occupies the active slot, flips in_flight, or mutates a
// running task — so single-active dispatch (NextReadyTask + one synchronous
// RunTask per tick) is unaffected by concurrent ingestion.
func (e *Engine) ingest(ctx context.Context) error {
	e.ingestMu.Lock()
	defer e.ingestMu.Unlock()

	tasks, err := e.Channel.ListNewTasks(ctx)
	if err != nil {
		return err
	}
	seen, err := e.Store.IssueRefs()
	if err != nil {
		return err
	}
	var ingested int
	for _, t := range tasks {
		if seen[t.Ref] {
			continue
		}
		if _, err := e.Store.InsertTask(state.TaskRow{
			IssueRef:    t.Ref,
			Description: t.Description,
			TaskType:    t.TaskType,
			Source:      "daemon",
			Criteria:    t.AcceptanceCriteria,
		}); err != nil {
			return err
		}
		seen[t.Ref] = true
		ingested++
	}
	if ingested > 0 {
		e.logf("[daemon] ingest: %d new task(s)", ingested)
	}
	return nil
}

// ingestLoop is the background ingest goroutine started by Run when IngestMax >
// 0. It keeps pulling new issues into the FIFO on a jittered cadence so an
// issue filed while the single active slot is busy running a long task still
// lands in state.db (and on the dashboard) — the synchronous tick is blocked
// inside RunTask and would otherwise not poll again until that task finishes.
//
// Each iteration sleeps a random delay in [IngestMin, IngestMax) (3–10s by
// default). A ListNewTasks / Store failure is logged and retried on the next
// jitter; it never propagates to or halts the main tick (spec §11 resilience).
// The loop exits when ctx is cancelled; Run's wg.Wait() awaits it on shutdown.
func (e *Engine) ingestLoop(ctx context.Context) {
	defer e.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(e.nextIngestDelay()):
		}
		if err := e.ingest(ctx); err != nil {
			e.logf("[daemon] background ingest error: %v", err)
		}
	}
}

// nextIngestDelay returns the next randomized ingest interval in [IngestMin,
// IngestMax). Randomizing the cadence (jitter) spreads channel polls off a
// fixed beat — avoiding synchronized bursts against a rate-limited upstream and
// making the loop's timing unobservable.
func (e *Engine) nextIngestDelay() time.Duration {
	min, max := e.IngestMin, e.IngestMax
	if min < 0 {
		min = 0
	}
	if max <= min {
		return min
	}
	return min + time.Duration(rand.Int64N(int64(max-min)))
}

// shortTaskID returns a truncated task ID for log lines (first 12 chars).
func shortTaskID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// cooldown returns the configured transient-infra cooldown, defaulting to 5
// minutes (spec §10/§11 resilience: wait out upstream congestion rather than
// burn the FIFO one blocked task per tick).
func (e *Engine) cooldown() time.Duration {
	if e.Cooldown > 0 {
		return e.Cooldown
	}
	return 5 * time.Minute
}

// isTransientInfra reports whether a blocked task's detail smells like transient
// upstream infrastructure failure (model-gateway congestion / rate limiting) — as
// opposed to a real verify rejection. Such blocks trip the daemon cooldown so the
// whole queue isn't burned one task per tick. Substring match (case-insensitive
// for ASCII) on the signals seen in the wild: GLM 529 "该模型当前访问量过大",
// generic 503 / rate-limit / overloaded. "context canceled" is NOT transient infra
// (that's a shutdown/timeout, not congestion).
func isTransientInfra(detail string) bool {
	d := strings.ToLower(detail)
	for _, sig := range []string{
		"529", "该模型当前", "访问量过大", "rate limit", "rate-limit",
		"overloaded", "too many requests", "service unavailable", "503",
	} {
		if strings.Contains(d, sig) {
			return true
		}
	}
	return false
}

// truncate returns s cut to at most n bytes with a "..." marker, for log lines.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// reconcile reads the channel-side state of every terminal task (done /
// blocked) and detects human-driven reversals:
//   - done + channel shows issue OPEN (reopened) → done → new
//   - blocked + channel shows no "loop:blocked" label → blocked → new
//
// This is the bidirectional sync: when a human acts on the channel (reopen,
// remove label), the daemon notices on the next tick and re-queues the task
// without a restart or DB poke.
func (e *Engine) reconcile(ctx context.Context) error {
	terms, err := e.Store.TerminalTasks()
	if err != nil {
		return err
	}
	if len(terms) == 0 {
		return nil
	}
	refs := make([]string, len(terms))
	byRef := make(map[string]state.TaskRow, len(terms))
	for i, t := range terms {
		refs[i] = t.IssueRef
		byRef[t.IssueRef] = t
	}
	states, err := e.Channel.GetTaskStates(ctx, refs)
	if err != nil {
		return err
	}
	for ref, s := range states {
		task, ok := byRef[ref]
		if !ok {
			continue
		}
		cur, _ := e.statusOf(task.ID)
		switch cur {
		case "done":
			if s.IsOpen {
				e.logf("[daemon] reconcile: done task %s (#%s) reopened on channel → re-queue", task.ID, ref)
				if err := e.Store.AppendTransition(task.ID, "done", "new", "reconcile: channel reopened"); err != nil {
					return err
				}
			}
		case "blocked":
			blockLabel := "loop:blocked"
			hasBlockLabel := false
			for _, l := range s.Labels {
				if l == blockLabel {
					hasBlockLabel = true
					break
				}
			}
			if !hasBlockLabel {
				if s.IsOpen {
					e.logf("[daemon] reconcile: blocked task %s (#%s) label removed on channel → re-queue", task.ID, ref)
					if err := e.Store.AppendTransition(task.ID, "blocked", "new", "reconcile: channel label removed"); err != nil {
						return err
					}
				} else {
					e.logf("[daemon] reconcile: blocked task %s (#%s) manually closed on channel → resolved", task.ID, ref)
					if err := e.Store.AppendTransition(task.ID, "blocked", "done", "reconcile: channel closed"); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func (e *Engine) statusOf(taskID string) (string, bool) {
	rows, err := e.Store.ListStatuses()
	if err != nil {
		return "", false
	}
	for _, r := range rows {
		if r.ID == taskID {
			return r.Status, true
		}
	}
	return "", false
}

// pollSignals checks every parked (needs-review) and blocked task for new human
// replies since the daemon's last comment. A task that has been replied to is
// re-queued (→ new) with the human feedback recorded in the transition reason.
// The daemon never blocks here: ListReplies is a non-blocking poll, and a
// transient channel error is logged but does not halt the tick (spec §10/§11).
func (e *Engine) pollSignals(ctx context.Context) error {
	if err := e.pollTaskReplies(ctx, e.Store.ParkedTasks, "needs-review"); err != nil {
		e.logf("poll signals (needs-review): %v", err)
	}
	if err := e.pollTaskReplies(ctx, e.Store.BlockedTasks, "blocked"); err != nil {
		e.logf("poll signals (blocked): %v", err)
	}
	return nil
}

// pollTaskReplies polls one class of task (needs-review or blocked) for new
// human reply comments. Only comments posted after the daemon's last comment
// (last_comment_at) are surfaced so the same reply doesn't re-queue the task
// on every tick.
func (e *Engine) pollTaskReplies(
	ctx context.Context,
	fetch func() ([]state.TaskRow, error),
	currentStatus string,
) error {
	tasks, err := fetch()
	if err != nil {
		return err
	}
	var resumed int
	for _, t := range tasks {
		since, _ := e.Store.LastCommentAt(t.ID)
		replies, err := e.Channel.ListReplies(ctx, []string{t.IssueRef}, since)
		if err != nil {
			e.logf("[daemon] pollTaskReplies for %s: %v", t.ID, err)
			continue
		}
		rs := replies[t.IssueRef]
		if len(rs) == 0 {
			continue
		}
		e.logf("[daemon] tick resume: task %s (%s=%s) has new human reply → re-queue",
			shortTaskID(t.ID), currentStatus, t.IssueRef)
		reason := "resumed: " + joinReplies(rs)
		if err := e.Store.AppendTransition(t.ID, currentStatus, "new", reason); err != nil {
			return err
		}
		if err := e.Store.SetResumeFeedback(t.ID, joinReplies(rs)); err != nil {
			e.logf("[daemon] task %s SetResumeFeedback failed: %v", t.ID, err)
		}
		resumed++
	}
	if resumed > 0 {
		e.logf("[daemon] tick resume: %d task(s) re-queued", resumed)
	}
	return nil
}

// joinReplies flattens one task's human replies into a single feedback string
// for the durable resume record. A human may post several replies; they are
// joined so the resumed task sees the whole thread as next round's feedback.
func joinReplies(rs []channel.Reply) string {
	parts := make([]string, len(rs))
	for i, r := range rs {
		parts[i] = r.Body
	}
	return strings.Join(parts, " | ")
}

// drainCommands applies every pending TUI command (resume/cancel) and marks it
// applied (spec §4.2/§7)。幂等：目标已终态（done/cancelled）或 running（由 SubLoop
// 自查处理）的命令只回写 applied_at，不产生 transition。单条命令出错则中断本轮
// drain，下 tick 从尚未 applied 的行重试。
func (e *Engine) drainCommands(ctx context.Context) error {
	cmds, err := e.Store.PendingCommands()
	if err != nil {
		return err
	}
	for _, c := range cmds {
		if err := e.applyCommand(ctx, c); err != nil {
			return err
		}
		if err := e.Store.MarkCommandApplied(c.ID); err != nil {
			return err
		}
	}
	if len(cmds) > 0 {
		e.logf("[daemon] tick commands: drained %d", len(cmds))
	}
	return nil
}

// applyCommand 把单条 TUI 命令翻译成 transition（spec §7）：
//   - resume（needs-review/blocked）→ X→new 并把 payload 落盘为 resume 反馈。
//   - cancel（new/needs-info/needs-review/blocked）→ X→cancelled。
//   - running/done/cancelled → 不动作（running 由 SubLoop 自查；其余已终态）。
func (e *Engine) applyCommand(ctx context.Context, c state.CommandRow) error {
	cur, _ := e.statusOf(c.TaskID)
	switch c.Verb {
	case "resume":
		if cur == "needs-review" || cur == "blocked" {
			if err := e.Store.SetResumeFeedback(c.TaskID, c.Payload); err != nil {
				return err
			}
			return e.Store.AppendTransition(c.TaskID, cur, "new", "tui resume: "+c.Payload)
		}
	case "cancel":
		switch cur {
		case "new", "needs-info", "needs-review", "blocked":
			return e.Store.AppendTransition(c.TaskID, cur, "cancelled", "cancelled by TUI")
		}
		// running → SubLoop 自查处理；done/cancelled → 已终态
	}
	return nil
}

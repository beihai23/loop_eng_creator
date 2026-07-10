// Package daemon implements the resident M3 control loop (Engine): a slow
// ticker that polls the ticket channel, ingests newly-arrived tasks into the
// durable FIFO, and drives a single active SubLoop to completion. The daemon
// never blocks on a human (spec principle 7): tier-3 human review parks the
// task, freeing the active slot, and is resumed on a later tick.
//
// This cut implements the tick loop + §7.1 step 1 (ingest with dedup), step 2
// (poll parked tasks for human replies → resume with feedback), and step 3
// (dispatch the FIFO head; reap collapses into "RunTask returned", and a
// needs-review return parks the task). Gate (step 5) and the SubLoop wiring
// land in subsequent issues.
package daemon

import (
	"context"
	"fmt"
	"os"
	"strings"
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
// (principle 4). The SubLoop wiring (RunTask → real SubLoop.Run) lands later.
type Engine struct {
	Channel  channel.Channel
	Store    *state.Store
	Interval time.Duration // poll_interval (spec §8.2): cadence between ticks
	Cooldown time.Duration // on a transient-infra block (upstream 529/congestion), skip dispatch this long
	RunTask  RunTaskFunc   // injected task-runner; nil → dispatch is a no-op until wired

	// coolUntil is the in-memory cooldown deadline after a transient-infra block.
	// Transient runtime state, NOT task lifecycle: a daemon restart resets it (a
	// fresh daemon should retry, not inherit a stale backoff — principle 4 covers
	// task state, not ephemeral backoff).
	coolUntil time.Time
}

// Run is the resident entry point. It ticks once immediately (a fresh daemon
// should not wait a full Interval before its first poll) and then every
// Interval until ctx is cancelled, returning ctx.Err(). A single tick's error
// is logged to stderr but does NOT halt the loop — a transient channel failure
// skips this tick and retries next, losing no persisted state (spec §11).
func (e *Engine) Run(ctx context.Context) error {
	if err := e.tick(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "daemon tick error: %v\n", err)
	}
	t := time.NewTicker(e.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if err := e.tick(ctx); err != nil {
				fmt.Fprintf(os.Stderr, "daemon tick error: %v\n", err)
			}
		}
	}
}

// tick is one daemon pass (spec §7.1). This cut implements step 1 (ingest with
// dedup), step 2 (resume parked tasks whose human has replied), and step 3
// (dispatch the FIFO head; a needs-review return parks the task). Gate (step 5)
// and the SubLoop wiring arrive in later issues.
//
// Dispatch is synchronous: M3 runs a single active sub-loop with no
// concurrency (spec §12), so running the task to completion inside this tick
// is what makes single-active structural — a second task cannot start until
// the first finishes. Reap (step 4) collapses into "RunTask returned": the
// terminal status is written back here, and needs-review means park (the task
// leaves running for needs-review, freeing the active slot — spec §7.2c/§10).
//
// Dedup is durable: it consults the persisted tasks table, not an in-memory
// set, so a daemon restart does not re-ingest tasks the channel still lists as
// new (spec principle 4 — recover from disk).
func (e *Engine) tick(ctx context.Context) error {
	tasks, err := e.Channel.ListNewTasks(ctx)
	if err != nil {
		return err
	}
	seen, err := e.Store.IssueRefs()
	if err != nil {
		return err
	}
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
		// Fold into seen so a duplicate within the same batch is skipped too.
		seen[t.Ref] = true
	}

	// ---- step 2: resume parked tasks whose human has replied (spec §7.1) ----
	// Runs before dispatch: a resumed task (needs-review→new) re-enters the FIFO
	// and may be the very task dispatched this same tick. Parked tasks with no
	// reply stay parked — the daemon never blocks here (principle 7).
	if err := e.pollReplies(ctx); err != nil {
		return err
	}

	// ---- dispatch (spec §7.1 step 3) ----
	// No task-runner wired yet → dispatch is a no-op (the SubLoop wiring lands in
	// a later issue). This also keeps the ingest-only daemon usable.
	if e.RunTask == nil {
		return nil
	}
	// Cooldown gate: a previous task blocked on transient upstream infra (e.g.
	// the model gateway 529 "该模型当前访问量过大"). Dispatching the next task
	// would hit the same wall and block it too, so the daemon waits out the
	// congestion instead of churning the whole FIFO to blocked one tick at a time.
	if now := time.Now(); now.Before(e.coolUntil) {
		fmt.Fprintf(os.Stderr, "[daemon] cooling down until %s (transient infra); skip dispatch\n",
			e.coolUntil.Format(time.TimeOnly))
		return nil
	}
	ready, ok, err := e.Store.NextReadyTask()
	if err != nil {
		return err
	}
	if !ok {
		return nil // FIFO empty this tick
	}
	// Occupy the active slot (status=running, spec §8.3 step 3) before running.
	if err := e.Store.AppendTransition(ready.ID, "new", "running", "dispatched"); err != nil {
		return err
	}
	// Run to completion synchronously (single-active by construction, spec §12),
	// then reap: the returned status is the terminal lifecycle state. A
	// "needs-review" return parks the task — the AppendTransition below moves it
	// running→needs-review, and since RunTask already returned, the active slot
	// is free; a later tick's pollReplies resumes it on a human reply.
	status, detail, err := e.RunTask(ctx, ready)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dispatch: task %s (%s) failed: %v\n", ready.ID, ready.IssueRef, err)
		status = "error"
	}
	// A transient-infra block trips the cooldown so the next tick skips dispatch
	// (gate above) AND re-queues this task (running→new) instead of marking it
	// permanently blocked: the failure was upstream congestion, not the task
	// itself, so after cooldown the daemon retries the SAME task — self-healing,
	// no manual reset. A real block (verify rejected, fatal auth) stays blocked.
	if status == "blocked" && isTransientInfra(detail) {
		e.coolUntil = time.Now().Add(e.cooldown())
		fmt.Fprintf(os.Stderr, "[daemon] transient-infra block on %s → cooldown until %s, re-queue task: %s\n",
			ready.ID, e.coolUntil.Format(time.TimeOnly), truncate(detail, 120))
		return e.Store.AppendTransition(ready.ID, "running", "new", "transient infra; re-queued")
	}
	return e.Store.AppendTransition(ready.ID, "running", status, "ran")
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

// pollReplies is spec §7.1 step 2: for every task parked on tier-3 human review,
// ask the channel for new human replies; a task that has been replied to is
// resumed — transitioned needs-review→new with the human feedback recorded in
// the transition reason — so the next dispatch (this tick's step 3, or a later
// tick if the active slot is busy) re-runs it. Parked tasks with no reply stay
// parked. The daemon never blocks here: ListReplies is a non-blocking poll, and
// a transient channel error returns from the tick and retries next (spec §10/§11,
// losing no persisted state).
//
// The feedback is durable in the transition trace (principle 4): the SubLoop
// that re-runs the task reads it back as next round's prior-failure input. That
// wiring (the SubLoop reading the resume reason) lands with the daemon↔SubLoop
// glue; this method's contract is "attach the feedback + re-queue the task".
func (e *Engine) pollReplies(ctx context.Context) error {
	parked, err := e.Store.ParkedTasks()
	if err != nil {
		return err
	}
	if len(parked) == 0 {
		return nil
	}
	refs := make([]string, len(parked))
	for i, p := range parked {
		refs[i] = p.IssueRef
	}
	replies, err := e.Channel.ListReplies(ctx, refs)
	if err != nil {
		return err
	}
	for _, p := range parked {
		rs := replies[p.IssueRef]
		if len(rs) == 0 {
			continue // still waiting on a human
		}
		// 人回了 → 带反馈恢复：标回 new（重新入 FIFO），反馈落盘进 transition reason。
		if err := e.Store.AppendTransition(p.ID, "needs-review", "new",
			"resumed: "+joinReplies(rs)); err != nil {
			return err
		}
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

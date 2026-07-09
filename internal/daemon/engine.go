// Package daemon implements the resident M3 control loop (Engine): a slow
// ticker that polls the ticket channel, ingests newly-arrived tasks into the
// durable FIFO, and — once dispatch/reap/park land in later issues — drives a
// single active SubLoop to completion. The daemon never blocks on a human
// (spec principle 7): tier-3 human review parks the task, freeing the active
// slot, and is resumed on a later tick.
//
// This first cut implements only the tick loop + §7.1 step 1 (ingest with
// dedup). Dispatch (step 3), reap (step 4), gate (step 5), and park/resume
// arrive in subsequent issues.
package daemon

import (
	"context"
	"fmt"
	"os"
	"time"

	"loop-eng/internal/channel"
	"loop-eng/internal/state"
)

// Engine is the resident M3 daemon. It polls Channel every Interval, ingesting
// new tasks into Store (the durable FIFO) deduped by issue_ref. Later fields
// (active slot, parked set, SubLoop wiring) are added as dispatch/reap/park
// land.
type Engine struct {
	Channel  channel.Channel
	Store    *state.Store
	Interval time.Duration // poll_interval (spec §8.2): cadence between ticks
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

// tick is one daemon pass (spec §7.1). This cut implements only step 1 —
// ingest: poll the channel for new tasks, dedup each by issue_ref against the
// Store's already-persisted tasks, and insert the unseen ones (status=new,
// into the FIFO). Steps 2–5 (replies, dispatch, reap, gate) and park/resume
// arrive in later issues.
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
	return nil
}

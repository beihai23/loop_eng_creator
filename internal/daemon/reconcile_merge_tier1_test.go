package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"loop-eng/internal/channel"
	"loop-eng/internal/state"
)

// mergeFakeCh serves GetTaskStates (configurably open), records CloseIssue, and
// answers IsPRMerged from a map — the three seams reconcile's PR-merge step needs.
type mergeFakeCh struct {
	closed []string
	merged map[string]bool
	open   bool
}

func (f *mergeFakeCh) ListNewTasks(context.Context) ([]channel.Task, error) { return nil, nil }
func (f *mergeFakeCh) ListReplies(context.Context, []string, time.Time) (map[string][]channel.Reply, error) {
	return nil, nil
}
func (f *mergeFakeCh) PostComment(context.Context, string, string) error { return nil }
func (f *mergeFakeCh) UpdateStatus(context.Context, string, string) error { return nil }
func (f *mergeFakeCh) CloseIssue(_ context.Context, ref string) error {
	f.closed = append(f.closed, ref)
	return nil
}
func (f *mergeFakeCh) GetTaskStates(_ context.Context, refs []string) (map[string]channel.TaskState, error) {
	out := make(map[string]channel.TaskState, len(refs))
	for _, r := range refs {
		out[r] = channel.TaskState{Ref: r, IsOpen: f.open}
	}
	return out, nil
}
func (f *mergeFakeCh) IsPRMerged(_ context.Context, branch string) (bool, error) {
	return f.merged[branch], nil
}

// TestTier1ReconcileClosesOnPRMerge: done task with a recorded land_branch whose
// PR has merged → reconcile closes the issue and records a "PR merged" transition.
func TestTier1ReconcileClosesOnPRMerge(t *testing.T) {
	st := newTestStore(t)
	id, err := st.InsertTask(state.TaskRow{IssueRef: "77", Description: "x", Source: "t"})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := st.AppendTransition(id, "new", "done", "ran"); err != nil {
		t.Fatalf("transition: %v", err)
	}
	if err := st.SetLandBranch(id, "loop/task-77-r1"); err != nil {
		t.Fatalf("SetLandBranch: %v", err)
	}
	ch := &mergeFakeCh{merged: map[string]bool{"loop/task-77-r1": true}, open: true}
	eng := &Engine{Channel: ch, Store: st, Interval: time.Second}
	if err := eng.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(ch.closed) != 1 || ch.closed[0] != "77" {
		t.Fatalf("reconcile must close #77 after PR merge, got %v", ch.closed)
	}
	trs, err := st.Transitions(id)
	if err != nil {
		t.Fatalf("transitions: %v", err)
	}
	var saw bool
	for _, tr := range trs {
		if strings.Contains(tr.Reason, "PR merged") {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("missing 'PR merged' transition reason among %+v", trs)
	}
}

// TestTier1ReconcileNoMergeStaysOpen: PR not yet merged → reconcile must NOT
// close and must NOT re-queue (task stays done, issue open, awaiting merge).
func TestTier1ReconcileNoMergeStaysOpen(t *testing.T) {
	st := newTestStore(t)
	id, _ := st.InsertTask(state.TaskRow{IssueRef: "78", Description: "x", Source: "t"})
	st.AppendTransition(id, "new", "done", "ran")
	st.SetLandBranch(id, "loop/task-78-r1")
	ch := &mergeFakeCh{merged: map[string]bool{}, open: true}
	eng := &Engine{Channel: ch, Store: st, Interval: time.Second}
	if err := eng.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(ch.closed) != 0 {
		t.Fatalf("must NOT close before merge, got %v", ch.closed)
	}
	if cur, _ := eng.statusOf(id); cur != "done" {
		t.Fatalf("unmerged-PR task must stay done (not re-queue), got %q", cur)
	}
}

// TestTier1ReconcileReopenRequeues: done+open with NO land_branch = a locally-
// landed (closed) task reopened by a human → re-queue to new (existing behavior).
func TestTier1ReconcileReopenRequeues(t *testing.T) {
	st := newTestStore(t)
	id, _ := st.InsertTask(state.TaskRow{IssueRef: "79", Description: "x", Source: "t"})
	st.AppendTransition(id, "new", "done", "ran")
	// no SetLandBranch → land_branch empty → reopen semantics (regression guard)
	ch := &mergeFakeCh{merged: map[string]bool{}, open: true}
	eng := &Engine{Channel: ch, Store: st, Interval: time.Second}
	if err := eng.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if cur, _ := eng.statusOf(id); cur != "new" {
		t.Fatalf("reopened done task (no land_branch) must re-queue to new, got %q", cur)
	}
}
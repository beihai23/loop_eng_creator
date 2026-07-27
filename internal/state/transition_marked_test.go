package state

import "testing"

// TestAppendTransitionMarkedAtomic pins #104's crash-gap fix: AppendTransitionMarked
// writes the transition AND marks the originating command applied in ONE transaction.
// After it returns, the transition row exists, task_status flipped, AND the command is
// marked applied (PendingCommands drains to 0) — so re-draining can't write a dup
// transition. Pre-fix did AppendTransition then MarkCommandApplied as two separate
// calls; a crash between them left the command un-marked → next tick re-drained it →
// AppendTransition wrote a DUPLICATE transition row (status table was fine — idempotent
// UPDATE — but the audit trace gained a dup).
func TestAppendTransitionMarkedAtomic(t *testing.T) {
	s, _ := Open(t.TempDir() + "/state.db")
	defer s.Close()
	tid, _ := s.InsertTask(TaskRow{IssueRef: "C", Description: "c"})
	if err := s.InsertCommand(tid, "cancel", ""); err != nil {
		t.Fatal(err)
	}
	pending, _ := s.PendingCommands()
	if len(pending) != 1 {
		t.Fatalf("setup: want 1 pending command, got %d", len(pending))
	}
	cmdID := pending[0].ID

	if err := s.AppendTransitionMarked(tid, "new", "cancelled", "tui cancel", cmdID); err != nil {
		t.Fatalf("AppendTransitionMarked: %v", err)
	}

	// (1) the transition landed. InsertTask writes no transition row, so this is the only one.
	trs, err := s.Transitions(tid)
	if err != nil {
		t.Fatalf("Transitions: %v", err)
	}
	if len(trs) != 1 || trs[0].From != "new" || trs[0].To != "cancelled" {
		t.Fatalf("want one new→cancelled transition, got %+v", trs)
	}
	// (2) status flipped — same transaction as the transition.
	if st, _ := statusByID(s, tid); st != "cancelled" {
		t.Fatalf("status must be cancelled, got %q", st)
	}
	// (3) the command is marked applied — the atomicity win: it landed WITH the
	// transition, so a re-drain finds nothing pending and cannot dup the transition.
	if pend, _ := s.PendingCommands(); len(pend) != 0 {
		t.Fatalf("command must be marked applied (pending=0); got %d", len(pend))
	}
}

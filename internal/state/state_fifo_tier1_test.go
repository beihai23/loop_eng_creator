package state

import "testing"

// TestTier1FIFOBySubmission proves NextReadyTask orders by issue submission
// time (TaskRow.CreatedAt → created_at column), NOT by ingest order. #32 is
// ingested FIRST (lower rowid) but submitted LATER; #31 is ingested LAST
// (higher rowid) but submitted EARLIER. FIFO-by-submission must return #31.
// Old code (created_at = nowISO at ingest) would return #32 → fails there.
func TestTier1FIFOBySubmission(t *testing.T) {
	s, err := Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// #32: submitted later, ingested FIRST (lower rowid).
	if _, err := s.InsertTask(TaskRow{
		IssueRef: "32", Description: "t32", TaskType: "feat",
		CreatedAt: "2026-07-17T10:00:00Z",
	}); err != nil {
		t.Fatalf("insert #32: %v", err)
	}
	// #31: submitted earlier, ingested LAST (higher rowid).
	if _, err := s.InsertTask(TaskRow{
		IssueRef: "31", Description: "t31", TaskType: "feat",
		CreatedAt: "2026-07-17T09:00:00Z",
	}); err != nil {
		t.Fatalf("insert #31: %v", err)
	}
	got, ok, err := s.NextReadyTask()
	if err != nil {
		t.Fatalf("NextReadyTask err: %v", err)
	}
	if !ok {
		t.Fatal("want a ready task, got none")
	}
	if got.IssueRef != "31" {
		t.Fatalf("FIFO by submission: want IssueRef=31 (submitted earlier), got %s", got.IssueRef)
	}
}

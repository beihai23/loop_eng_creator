package channel

import (
	"context"
	"time"
)

type Task struct {
	Ref                string
	Description        string
	AcceptanceCriteria []string
	TaskType           string
	// Body is the full raw issue/ticket text. parseLocalTask only distills the
	// first line (Description) and `- [ ]` items (AcceptanceCriteria) — the
	// background/constraints/context prose lives here and flows to the plan and
	// execute prompts. Empty for tasks ingested before this field existed
	// (their first spec re-ingest backfills it).
	Body string
	// CreatedAt is the issue/ticket submission time (RFC3339) as reported by the
	// channel. Carries through to tasks.created_at so the dispatch FIFO orders by
	// submission time, not by ingest order. Empty when the channel has no value.
	CreatedAt string
}

type Reply struct{ Body string }

// TaskState is the channel-side view of a single task's status — used by the
// daemon reconcile step to detect human-driven state changes (reopen, un-label)
// and re-queue tasks whose channel state no longer matches the DB terminal state.
type TaskState struct {
	Ref    string
	IsOpen bool     // issue/ticket not closed/archived
	Labels []string // channel-native status markers (GitHub labels, Linear columns, Jira statuses)
}

type Channel interface {
	ListNewTasks(ctx context.Context) ([]Task, error)
	ListReplies(ctx context.Context, refs []string, since time.Time) (map[string][]Reply, error)
	PostComment(ctx context.Context, ref, body string) error
	UpdateStatus(ctx context.Context, ref, status string) error
	CloseIssue(ctx context.Context, ref string) error
	GetTaskStates(ctx context.Context, refs []string) (map[string]TaskState, error)
}

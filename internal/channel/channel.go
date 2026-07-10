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

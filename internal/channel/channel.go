package channel

import "context"

type Task struct {
	Ref                string
	Description        string
	AcceptanceCriteria []string
	TaskType           string
}

type Reply struct{ Body string }

type Channel interface {
	ListNewTasks(ctx context.Context) ([]Task, error)
	ListReplies(ctx context.Context, refs []string) (map[string][]Reply, error)
	PostComment(ctx context.Context, ref, body string) error
	UpdateStatus(ctx context.Context, ref, status string) error
}

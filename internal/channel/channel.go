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
	// Agent is an optional per-task coding-agent override (a provider key, e.g.
	// "codex"). Sourced from the issue's `agent:` frontmatter (local) or an
	// `agent:codex`-style label (github, future). Empty = use the configured
	// default provider for every role. Carries through to tasks.agent so the
	// daemon can opt this one task into a different provider stack.
	Agent string
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

// StatusLabeler is an optional Channel capability: channels that move status
// via labels (GitHub) expose how a loop status maps to a label name, so
// consumers (daemon reconcile) never hardcode the label family — especially
// under a custom label_prefix ("ai:running" instead of "loop:running").
// Channels without label semantics (Local) simply don't implement it.
type StatusLabeler interface {
	StatusLabel(status string) string
}

// MergeChecker is an optional Channel capability for channels that integrate
// done work via a pull-request-style flow (GitHub). IsPRMerged reports whether
// the PR opened from the given head branch has been merged. The daemon's
// reconcile step uses it to auto-close an issue left OPEN pending merge: once
// the branch's PR merges, the next tick closes the issue (closing the
// "done-before-integrated" gap — #72/#75 had PRs merged only after their issues
// were already closed). Channels without PR semantics (Local, Linear) simply
// don't implement it; reconcile then leaves the issue alone (no merge to detect).
type MergeChecker interface {
	IsPRMerged(ctx context.Context, branch string) (bool, error)
}

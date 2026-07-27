package channel

import (
	"context"
	"time"
)

// maxRefsPerTick caps how many refs a single daemon tick will poll for
// replies/states. The daemon previously issued one network call per ref
// (each with its own 3× retry / 2s-4s backoff), so a busy tick with many
// parked/blocked tasks could stall dispatch for minutes on a network blip.
// Capping keeps a single tick bounded: over the cap, the excess is deferred
// to the next tick (logged, not silently dropped) so the batched/concurrent
// fetch stays O(cap) wall-clock. The value is chosen well above any realistic
// single-project parked-task count (50) while keeping one tick's network
// budget predictable.
const maxRefsPerTick = 50

// capRefs bounds refs to maxRefsPerTick, ROTATING the window by `off` so a
// stable list larger than the cap is covered across successive ticks instead of
// always truncating the same tail. A fixed refs[:cap] truncation starves the
// overflow — the refs come from an ordered query (TerminalTasks etc.), so the
// same tail would be cut every tick and NEVER polled. Rotation advances the
// window by cap each call (wrapping mod len), so over ceil(N/cap) ticks every
// ref is polled. Returns the selected refs and the next offset to pass on the
// following call (0 when no rotation was needed). Rotation is correct, covered
// by TestRefCapRotatesNoStarvation, and routine (not an exception) — so it is
// silent: no per-tick log, which would spam once a repo accumulates >cap polled
// tasks (and the codebase logs exceptions, not routine operation).
func capRefs(refs []string, off int) ([]string, int) {
	if len(refs) <= maxRefsPerTick {
		return refs, 0
	}
	n := len(refs)
	sel := make([]string, maxRefsPerTick)
	for i := 0; i < maxRefsPerTick; i++ {
		sel[i] = refs[(off+i)%n]
	}
	return sel, (off + maxRefsPerTick) % n
}

type Task struct {
	Ref                string
	Description        string
	AcceptanceCriteria []string
	TaskType           string
	// Title is the issue/ticket headline (GitHub issue title, Linear issue
	// title; the local channel falls back to the task file's first
	// non-`#` line). It is distinct from Description — which is the distilled
	// first line of the BODY, not the title. The PR title prefers Title so a
	// body that opens with a "# 目标"/"# 现状" markdown heading does not leak a
	// mid-body bullet into the PR title (the #74/#76 bug). Empty = a task
	// ingested before this field existed; consumers fall back to Description.
	Title string
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

// Reply is one human-side comment surfaced back to the daemon. Body is the
// verbatim comment text. CreatedAt is the comment's RFC3339 time (empty when
// the channel can't report it). Carrying CreatedAt on the Reply itself —
// instead of only filtering inside the channel — lets the daemon batch-fetch
// ALL of a tick's replies (since=zero) and then re-filter per task against
// each task's own last_comment_at, which is the premise of the batched
// pollTaskReplies: one network round-trip for N refs, N client-side filters.
type Reply struct {
	Body      string
	CreatedAt string // RFC3339; empty when the channel has no value
}

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

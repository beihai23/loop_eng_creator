package channel

import (
	"context"
	"errors"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseIssueJSONAndBody(t *testing.T) {
	raw := `[{"number":42,"title":"add X","body":"## 任务\nadd X\ntype: feature\n## 验收标准\n- [ ] it works"}]`
	tasks, err := parseIssuesJSON([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].Ref != "42" || tasks[0].TaskType != "feature" {
		t.Fatalf("parse wrong: %+v", tasks)
	}
	if len(tasks[0].AcceptanceCriteria) != 1 || tasks[0].AcceptanceCriteria[0] != "it works" {
		t.Fatalf("criteria wrong: %+v", tasks[0].AcceptanceCriteria)
	}
	if tasks[0].Description != "add X" {
		t.Fatalf("desc wrong: %q", tasks[0].Description)
	}
}

// TestParseIssuesJSONCarriesCreatedAt locks in that the issue submission time
// from `gh issue list --json ...,createdAt` is carried onto Task.CreatedAt —
// the field InsertTask writes into tasks.created_at so the dispatch FIFO orders
// by submission time, not ingest order. Lists come back newest-first, so without
// this the FIFO would be LIFO.
func TestParseIssuesJSONCarriesCreatedAt(t *testing.T) {
	raw := `[{"number":42,"title":"add X","body":"add X","createdAt":"2026-07-15T10:00:00Z"}]`
	tasks, err := parseIssuesJSON([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("got %d tasks", len(tasks))
	}
	if tasks[0].CreatedAt != "2026-07-15T10:00:00Z" {
		t.Fatalf("CreatedAt not carried from gh createdAt: got %q", tasks[0].CreatedAt)
	}
}

// TestParseIssuesJSONCarriesTitle locks in that the GitHub issue title lands on
// Task.Title — distinct from Description (the distilled body first line). Title
// is the source of the PR title (prTitleFor prefers it), fixing #74/#76 where a
// body opening with "# 目标" leaked a mid-body bullet into the PR title.
func TestParseIssuesJSONCarriesTitle(t *testing.T) {
	raw := `[{"number":42,"title":"Fix login bug","body":"# 目标\n- bullet from the body"}]`
	tasks, err := parseIssuesJSON([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("got %d tasks", len(tasks))
	}
	if tasks[0].Title != "Fix login bug" {
		t.Fatalf("Title should be the issue title, got %q", tasks[0].Title)
	}
	if tasks[0].Description != "- bullet from the body" {
		t.Fatalf("Description should be the distilled body first line (not the title), got %q", tasks[0].Description)
	}
}

// TestParseIssueCommentsJSON feeds a trimmed `gh issue view <ref> --json comments`
// blob and asserts each comment maps to one Reply whose body is verbatim.
func TestParseIssueCommentsJSON(t *testing.T) {
	// fixture mirrors real gh output shape: comments carry author/createdAt/etc.
	// which the parser must ignore; only body is carried. Includes a multi-line
	// body and an empty body to prove verbatim preservation.
	raw := `{"comments":[` +
		`{"author":{"login":"alice"},"authorAssociation":"NONE","body":"LGTM, ship it","createdAt":"2026-07-09T10:00:00Z","isMinimized":false},` +
		`{"author":{"login":"bob"},"body":"please address nits\n- naming\n- tests","createdAt":"2026-07-09T10:05:00Z"},` +
		`{"author":{"login":"carol"},"body":""}` +
		`]}`

	replies, err := parseIssueCommentsJSON([]byte(raw), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	// map[ref][]Reply — every comment becomes exactly one Reply.
	m := map[string][]Reply{"42": replies}
	if len(m["42"]) != 3 {
		t.Fatalf("want 3 replies for ref 42, got %d", len(m["42"]))
	}
	wantBodies := []string{
		"LGTM, ship it",
		"please address nits\n- naming\n- tests",
		"",
	}
	for i, want := range wantBodies {
		if got := m["42"][i].Body; got != want {
			t.Fatalf("reply[%d].Body = %q, want %q", i, got, want)
		}
	}
}

// TestParseIssueCommentsJSONEmpty covers the zero-comments case (a parked
// task with no human reply yet): the ref still maps to an empty slice.
func TestParseIssueCommentsJSONEmpty(t *testing.T) {
	replies, err := parseIssueCommentsJSON([]byte(`{"comments":[]}`), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(replies) != 0 {
		t.Fatalf("want 0 replies, got %d", len(replies))
	}
}

// TestParseIssueCommentsJSONBad asserts malformed JSON surfaces an error
// rather than silently returning an empty result.
func TestParseIssueCommentsJSONBad(t *testing.T) {
	if _, err := parseIssueCommentsJSON([]byte(`{not-json`), time.Time{}); err == nil {
		t.Fatal("want error for malformed comments JSON, got nil")
	}
}

// TestParseIssueCommentsJSONCarriesCreatedAt pins that every Reply now carries
// the comment's RFC3339 createdAt — the field the daemon's batched
// repliesSince re-filter relies on after a single ListReplies(since=zero) fetch.
func TestParseIssueCommentsJSONCarriesCreatedAt(t *testing.T) {
	raw := `{"comments":[{"body":"first","createdAt":"2026-07-01T00:00:00Z"},{"body":"second","createdAt":"2026-07-09T10:05:00Z"}]}`
	replies, err := parseIssueCommentsJSON([]byte(raw), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(replies) != 2 {
		t.Fatalf("want 2 replies, got %d", len(replies))
	}
	if replies[0].CreatedAt != "2026-07-01T00:00:00Z" || replies[1].CreatedAt != "2026-07-09T10:05:00Z" {
		t.Fatalf("CreatedAt not carried: %+v", replies)
	}
}

// TestGitHubListRepliesCapsRefs pins that ListReplies honors the per-tick ref
// cap: over maxRefsPerTick, only the first maxRefsPerTick are fetched (the rest
// deferred to the next tick) and gh is called exactly maxRefsPerTick times.
func TestGitHubListRepliesCapsRefs(t *testing.T) {
	var calls int32
	g := &GitHub{Repo: "owner/repo", TaskLabel: "loop:task", ghFunc: func(ctx context.Context, args ...string) ([]byte, error) {
		atomic.AddInt32(&calls, 1)
		return []byte(`{"comments":[]}`), nil
	}}
	refs := make([]string, maxRefsPerTick+3)
	for i := range refs {
		refs[i] = strconv.Itoa(i + 1)
	}
	got, err := g.ListReplies(context.Background(), refs, time.Time{})
	if err != nil {
		t.Fatalf("ListReplies: %v", err)
	}
	if int(calls) != maxRefsPerTick {
		t.Fatalf("gh 调用数 = %d, want 封顶到 maxRefsPerTick=%d", calls, maxRefsPerTick)
	}
	if len(got) != maxRefsPerTick {
		t.Fatalf("回填 replies = %d, want 封顶到 maxRefsPerTick=%d", len(got), maxRefsPerTick)
	}
}

// TestGitHubListRepliesPartialFailure pins the bounded pool's partial-failure
// semantics: when one ref's gh call fails, the OTHER refs are still fetched and
// back-filled, and the first error is returned (not nil). This is the key win
// over the old serial loop — one flaky ref no longer aborts the whole tick.
func TestGitHubListRepliesPartialFailure(t *testing.T) {
	g := &GitHub{Repo: "owner/repo", TaskLabel: "loop:task", ghFunc: func(ctx context.Context, args ...string) ([]byte, error) {
		// ref is args[2] (issue view <ref> ...); simulate ref "2" failing.
		if len(args) > 2 && args[2] == "2" {
			return nil, errors.New("gh: HTTP 502")
		}
		return []byte(`{"comments":[{"body":"ok","createdAt":"2026-07-01T00:00:00Z"}]}`), nil
	}}
	got, err := g.ListReplies(context.Background(), []string{"1", "2", "3"}, time.Time{})
	if err == nil {
		t.Fatal("应透出首个失败 ref 的 error, got nil")
	}
	if len(got["1"]) != 1 || got["1"][0].Body != "ok" {
		t.Fatalf("ref 1 应在 ref 2 失败时仍回填, got[1]=%v", got["1"])
	}
	if len(got["3"]) != 1 || got["3"][0].Body != "ok" {
		t.Fatalf("ref 3 应在 ref 2 失败时仍回填, got[3]=%v", got["3"])
	}
	if _, ok := got["2"]; ok {
		t.Fatalf("失败的 ref 2 不应回填, got[2]=%v", got["2"])
	}
}

// TestGitHubGetTaskStatesPartialFailure is the GetTaskStates analog of the
// above: one failing ref returns firstErr but the rest are still populated, so
// reconcile can act on whichever terminal refs it learned about.
func TestGitHubGetTaskStatesPartialFailure(t *testing.T) {
	g := &GitHub{Repo: "owner/repo", TaskLabel: "loop:task", ghFunc: func(ctx context.Context, args ...string) ([]byte, error) {
		if len(args) > 2 && args[2] == "2" {
			return nil, errors.New("gh: HTTP 502")
		}
		return []byte(`{"state":"OPEN","labels":[{"name":"loop:running"}]}`), nil
	}}
	got, err := g.GetTaskStates(context.Background(), []string{"1", "2", "3"})
	if err == nil {
		t.Fatal("应透出首个失败 ref 的 error, got nil")
	}
	if !got["1"].IsOpen || len(got["1"].Labels) != 1 {
		t.Fatalf("ref 1 应回填 OPEN state, got[1]=%+v", got["1"])
	}
	if !got["3"].IsOpen {
		t.Fatalf("ref 3 应在 ref 2 失败时仍回填, got[3]=%+v", got["3"])
	}
	if _, ok := got["2"]; ok {
		t.Fatalf("失败的 ref 2 不应回填, got[2]=%+v", got["2"])
	}
}

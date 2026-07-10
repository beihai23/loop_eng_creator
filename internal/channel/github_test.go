package channel

import (
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

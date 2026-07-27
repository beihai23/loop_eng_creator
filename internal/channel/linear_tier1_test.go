package channel

// Tier-1 acceptance for the Linear channel, black-box against the plan-pinned
// contract:
//   - func NewLinear(apiKey, endpoint, projectID, teamID string, statusMap map[string]string) *Linear
//   - Linear satisfies the frozen Channel interface (six methods, no more).
//   - Authorization header carries the raw API key (NO Bearer prefix).
//   - GraphQL "errors" arrays fold into Go errors.
// It never touches unexported fields or the gql helper directly.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

var _ Channel = (*Linear)(nil)

type tier1Req struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

type tier1Rec struct {
	mu       sync.Mutex
	auths    []string
	commentV map[string]any
	updates  []map[string]any
}

func tier1HasVar(vars map[string]any, want string) bool {
	for _, v := range vars {
		if s, ok := v.(string); ok && s == want {
			return true
		}
	}
	return false
}

func TestTier1LinearChannel(t *testing.T) {
	rec := &tier1Rec{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req tier1Req
		_ = json.Unmarshal(raw, &req)
		rec.mu.Lock()
		rec.auths = append(rec.auths, r.Header.Get("Authorization"))
		rec.mu.Unlock()
		q := req.Query
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(q, "commentCreate"):
			rec.mu.Lock()
			rec.commentV = req.Variables
			rec.mu.Unlock()
			io.WriteString(w, `{"data":{"commentCreate":{"success":true}}}`)
		case strings.Contains(q, "issueUpdate"):
			rec.mu.Lock()
			rec.updates = append(rec.updates, req.Variables)
			rec.mu.Unlock()
			io.WriteString(w, `{"data":{"issueUpdate":{"success":true}}}`)
		case strings.Contains(q, "workflowStates"):
			nodes := `[{"id":"s-todo","name":"Todo","type":"unstarted"},{"id":"s-done","name":"Done","type":"completed"}]`
			if strings.Contains(q, "team") {
				io.WriteString(w, `{"data":{"team":{"workflowStates":{"nodes":`+nodes+`}}}}`)
			} else {
				io.WriteString(w, `{"data":{"workflowStates":{"nodes":`+nodes+`}}}`)
			}
		case strings.Contains(q, "comments"):
			// 批量 ListReplies：别名 r0 顶层对象（不再单 issue 包裹）。
			io.WriteString(w, `{"data":{"r0":{"comments":{"nodes":[{"body":"old","createdAt":"2026-01-01T00:00:00Z"},{"body":"new","createdAt":"2026-07-10T00:00:00Z"}]}}}}`)
		case strings.Contains(q, "archivedAt"):
			// 批量 GetTaskStates：别名 r0 顶层对象（不再单 issue 包裹）。
			io.WriteString(w, `{"data":{"r0":{"identifier":"ENG-1","archivedAt":null,"state":{"id":"s-prog","name":"In Progress","type":"started"}}}}`)
		case strings.Contains(q, "issues"):
			io.WriteString(w, `{"data":{"issues":{"nodes":[{"identifier":"ENG-1","title":"fallback title","description":"## 任务\ndo the thing\ntype: bug\n- [ ] ac1","createdAt":"2026-07-01T00:00:00Z","state":{"id":"s-todo","name":"Todo","type":"unstarted"}}]}}}`)
		default: // identifier→UUID resolution: issue(id:$ref){ id }
			io.WriteString(w, `{"data":{"issue":{"id":"uuid-1"}}}`)
		}
	}))
	defer srv.Close()

	lc := NewLinear("test-key", srv.URL, "proj-uuid", "team-uuid", map[string]string{"done": "Done"})
	ctx := context.Background()

	// ListNewTasks: project-filtered issues, description parsed via parseLocalTask.
	tasks, err := lc.ListNewTasks(ctx)
	if err != nil {
		t.Fatalf("ListNewTasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("ListNewTasks len=%d want 1", len(tasks))
	}
	if tasks[0].Ref != "ENG-1" {
		t.Fatalf("Ref=%q want ENG-1 (identifier)", tasks[0].Ref)
	}
	if tasks[0].Description != "do the thing" {
		t.Fatalf("Description=%q want parsed body line", tasks[0].Description)
	}
	if tasks[0].TaskType != "bug" {
		t.Fatalf("TaskType=%q want bug", tasks[0].TaskType)
	}
	if len(tasks[0].AcceptanceCriteria) != 1 || tasks[0].AcceptanceCriteria[0] != "ac1" {
		t.Fatalf("AcceptanceCriteria=%v want [ac1]", tasks[0].AcceptanceCriteria)
	}
	if tasks[0].CreatedAt != "2026-07-01T00:00:00Z" {
		t.Fatalf("CreatedAt=%q want passthrough", tasks[0].CreatedAt)
	}

	// PostComment: identifier must be resolved to UUID before commentCreate.
	if err := lc.PostComment(ctx, "ENG-1", "hi"); err != nil {
		t.Fatalf("PostComment: %v", err)
	}
	rec.mu.Lock()
	cv := rec.commentV
	rec.mu.Unlock()
	if !tier1HasVar(cv, "uuid-1") {
		t.Fatalf("commentCreate vars=%v want resolved UUID uuid-1 (not ENG-1)", cv)
	}
	if !tier1HasVar(cv, "hi") {
		t.Fatalf("commentCreate vars=%v want body hi", cv)
	}

	// UpdateStatus: statusMap value is a state NAME; runtime name→id must yield s-done.
	if err := lc.UpdateStatus(ctx, "ENG-1", "done"); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	rec.mu.Lock()
	if len(rec.updates) == 0 {
		rec.mu.Unlock()
		t.Fatal("UpdateStatus issued no issueUpdate mutation")
	}
	up := rec.updates[len(rec.updates)-1]
	before := len(rec.updates)
	rec.mu.Unlock()
	if !tier1HasVar(up, "s-done") {
		t.Fatalf("UpdateStatus issueUpdate vars=%v want stateId s-done (name Done → id)", up)
	}

	// CloseIssue: no dedicated close mutation — exactly one more issueUpdate to completed-type state.
	if err := lc.CloseIssue(ctx, "ENG-1"); err != nil {
		t.Fatalf("CloseIssue: %v", err)
	}
	rec.mu.Lock()
	n := len(rec.updates)
	var last map[string]any
	if n > 0 {
		last = rec.updates[n-1]
	}
	rec.mu.Unlock()
	if n != before+1 {
		t.Fatalf("CloseIssue: issueUpdate count %d → %d, want exactly one more", before, n)
	}
	if !tier1HasVar(last, "s-done") {
		t.Fatalf("CloseIssue issueUpdate vars=%v want stateId s-done", last)
	}

	// ListReplies: client-side since filter, body verbatim.
	replies, err := lc.ListReplies(ctx, []string{"ENG-1"}, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("ListReplies: %v", err)
	}
	if len(replies["ENG-1"]) != 1 || replies["ENG-1"][0].Body != "new" {
		t.Fatalf("ListReplies=%v want only the post-since comment 'new'", replies)
	}

	// GetTaskStates: IsOpen from archivedAt + state.type, Labels = [state.name].
	states, err := lc.GetTaskStates(ctx, []string{"ENG-1"})
	if err != nil {
		t.Fatalf("GetTaskStates: %v", err)
	}
	st, ok := states["ENG-1"]
	if !ok {
		t.Fatalf("GetTaskStates missing ENG-1: %v", states)
	}
	if !st.IsOpen {
		t.Fatal("IsOpen=false want true (started-type state, not archived)")
	}
	if len(st.Labels) != 1 || st.Labels[0] != "In Progress" {
		t.Fatalf("Labels=%v want [In Progress] (state.name)", st.Labels)
	}

	// Auth: EVERY request must carry the raw key, no Bearer prefix.
	rec.mu.Lock()
	auths := append([]string(nil), rec.auths...)
	rec.mu.Unlock()
	if len(auths) == 0 {
		t.Fatal("no requests recorded")
	}
	for i, a := range auths {
		if a != "test-key" {
			t.Fatalf("request %d Authorization=%q want raw key test-key (no Bearer)", i, a)
		}
	}
}

func TestTier1LinearGQLErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"errors":[{"message":"boom"}]}`)
	}))
	defer srv.Close()
	lc := NewLinear("test-key", srv.URL, "proj-uuid", "team-uuid", nil)
	_, err := lc.ListNewTasks(context.Background())
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err=%v want GraphQL errors folded into Go error containing 'boom'", err)
	}
}

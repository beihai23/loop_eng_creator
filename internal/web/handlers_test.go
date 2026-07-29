package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"loop-eng/internal/state"
)

// newTestServer builds a Server over a fresh temp-DB Store with cfg=nil
// (the handler must be nil-safe — see verifyTiers / detail budget limit).
func newTestServer(t *testing.T) (*Server, *state.Store) {
	t.Helper()
	st, err := state.Open(t.TempDir() + "/s.db")
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st, nil), st
}

func do(t *testing.T, h http.Handler, method, target string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != nil {
		r = httptest.NewRequest(method, target, bytes.NewReader(body))
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func decode(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode json: %v body=%s", err, w.Body.String())
	}
	return m
}

// ── overview ─────────────────────────────────────────────────────────────────

func TestOverviewEmpty(t *testing.T) {
	srv, _ := newTestServer(t)
	w := do(t, srv.Handler(), http.MethodGet, "/api/overview", nil)

	if w.Code != 200 {
		t.Fatalf("status=%d want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type=%q", ct)
	}
	m := decode(t, w)
	// tasks MUST be an empty array, not null (frontend forEach would crash on null).
	tasks, ok := m["tasks"].([]any)
	if !ok {
		t.Fatalf("tasks not an array: %T %v", m["tasks"], m["tasks"])
	}
	if len(tasks) != 0 {
		t.Fatalf("tasks=%v want empty", tasks)
	}
	counts, ok := m["counts"].(map[string]any)
	if !ok {
		t.Fatalf("counts not an object: %T", m["counts"])
	}
	if len(counts) != 0 {
		t.Fatalf("counts=%v want empty", counts)
	}
}

func TestOverviewWithData(t *testing.T) {
	srv, st := newTestServer(t)
	id1, _ := st.InsertTask(state.TaskRow{IssueRef: "o/r#1", Description: "first", Criteria: []string{"a"}})
	if _, err := st.InsertTask(state.TaskRow{IssueRef: "o/r#2", Description: "second"}); err != nil {
		t.Fatal(err)
	}

	w := do(t, srv.Handler(), http.MethodGet, "/api/overview", nil)
	if w.Code != 200 {
		t.Fatalf("status=%d", w.Code)
	}
	m := decode(t, w)
	tasks := m["tasks"].([]any)
	if len(tasks) != 2 {
		t.Fatalf("tasks len=%d want 2", len(tasks))
	}
	// TasksByStatus orders newest-first (created_at DESC); don't assume which
	// lands first — find the id1 row by id and assert its lowercase fields.
	var first map[string]any
	for _, tk := range tasks {
		row := tk.(map[string]any)
		if row["id"] == id1 {
			first = row
			break
		}
	}
	if first == nil {
		t.Fatalf("id1 task not in list: %v", tasks)
	}
	if first["description"] != "first" || first["status"] != "new" {
		t.Fatalf("id1 task=%v", first)
	}
	// counts bucketed by status; both fresh tasks are "new".
	counts := m["counts"].(map[string]any)
	if counts["new"] != float64(2) {
		t.Fatalf("counts=%v want new=2", counts)
	}
	// lowercase keys present (not the Go field names ID/Description/Status).
	for _, k := range []string{"id", "issue_ref", "description", "task_type", "status", "created_at", "last_run_at"} {
		if _, ok := first[k]; !ok {
			t.Fatalf("task missing lowercase key %q: %v", k, first)
		}
	}
}

// ── detail ───────────────────────────────────────────────────────────────────

func TestDetailFound(t *testing.T) {
	srv, st := newTestServer(t)
	id, _ := st.InsertTask(state.TaskRow{
		IssueRef: "o/r#9", Description: "build dashboard", Criteria: []string{"c1", "c2"},
	})

	w := do(t, srv.Handler(), http.MethodGet, "/api/tasks/"+id, nil)
	if w.Code != 200 {
		t.Fatalf("status=%d", w.Code)
	}
	m := decode(t, w)
	if m["description"] != "build dashboard" {
		t.Fatalf("description=%v", m["description"])
	}
	if m["status"] != "new" {
		t.Fatalf("status=%v want new", m["status"])
	}
	crit, ok := m["criteria"].([]any)
	if !ok || len(crit) != 2 {
		t.Fatalf("criteria=%v want 2-item array", m["criteria"])
	}
}

func TestDetailNotFound(t *testing.T) {
	srv, _ := newTestServer(t)
	w := do(t, srv.Handler(), http.MethodGet, "/api/tasks/does-not-exist", nil)
	if w.Code != 404 {
		t.Fatalf("status=%d want 404", w.Code)
	}
}

// ── trace ────────────────────────────────────────────────────────────────────

func TestTraceShape(t *testing.T) {
	srv, st := newTestServer(t)
	id, _ := st.InsertTask(state.TaskRow{IssueRef: "o/r#3", Description: "traced"})
	runID, err := st.StartRun(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AppendStep(state.StepRow{RunID: runID, Seq: 1, Role: "plan", Status: "ok", TokensIn: 10, TokensOut: 5}); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendVerification(runID, 1, true, "go test ./...: ok"); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendBudget(runID, "call", "tokens", 42, 200); err != nil {
		t.Fatal(err)
	}
	if err := st.EndRun(runID, "done"); err != nil {
		t.Fatal(err)
	}

	w := do(t, srv.Handler(), http.MethodGet, "/api/tasks/"+id+"/trace", nil)
	if w.Code != 200 {
		t.Fatalf("status=%d", w.Code)
	}
	m := decode(t, w)
	runs := m["runs"].([]any)
	if len(runs) != 1 {
		t.Fatalf("runs len=%d want 1", len(runs))
	}
	run := runs[0].(map[string]any)
	if run["outcome"] != "done" {
		t.Fatalf("outcome=%v", run["outcome"])
	}
	// steps / verifications / budget all present as arrays under the run.
	for _, k := range []string{"steps", "verifications", "budget"} {
		if _, ok := run[k].([]any); !ok {
			t.Fatalf("run missing %q array: %v", k, run[k])
		}
	}
	vers := run["verifications"].([]any)
	if len(vers) != 1 || vers[0].(map[string]any)["tier"] != float64(1) {
		t.Fatalf("verifications=%v", vers)
	}
}

func TestTraceNotFound(t *testing.T) {
	srv, _ := newTestServer(t)
	w := do(t, srv.Handler(), http.MethodGet, "/api/tasks/ghost/trace", nil)
	if w.Code != 404 {
		t.Fatalf("status=%d want 404", w.Code)
	}
}

// ── command ──────────────────────────────────────────────────────────────────

func TestCommandVerbValidation(t *testing.T) {
	srv, st := newTestServer(t)
	id, _ := st.InsertTask(state.TaskRow{IssueRef: "o/r#1", Description: "cmd target"})

	cases := []struct {
		name    string
		body    string
		want2xx bool
		// delta in pending command rows after this request
		wantDelta int
	}{
		{"resume ok", `{"verb":"resume","payload":"fix it"}`, true, 1},
		{"cancel ok", `{"verb":"cancel","payload":""}`, true, 1},
		{"bad verb explode", `{"verb":"explode"}`, false, 0},
		{"unknown verb empty", `{"verb":""}`, false, 0},
		{"malformed json", `{not json`, false, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := countPending(t, st)
			w := do(t, srv.Handler(), http.MethodPost, "/api/tasks/"+id+"/command", []byte(c.body))
			if c.want2xx {
				if w.Code < 200 || w.Code >= 300 {
					t.Fatalf("status=%d want 2xx body=%s", w.Code, w.Body.String())
				}
			} else {
				if w.Code < 400 || w.Code >= 500 {
					t.Fatalf("status=%d want 4xx body=%s", w.Code, w.Body.String())
				}
			}
			after := countPending(t, st)
			if got := after - before; got != c.wantDelta {
				t.Fatalf("pending delta=%d want %d", got, c.wantDelta)
			}
		})
	}

	// the two valid verbs both landed as rows with matching task_id + verb.
	pending, err := st.PendingCommands()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("pending=%d want 2", len(pending))
	}
	verbs := map[string]bool{}
	for _, p := range pending {
		if p.TaskID != id {
			t.Fatalf("task_id=%q want %q", p.TaskID, id)
		}
		verbs[p.Verb] = true
	}
	if !verbs["resume"] || !verbs["cancel"] {
		t.Fatalf("pending verbs=%v want resume+cancel", verbs)
	}
}

func countPending(t *testing.T, st *state.Store) int {
	t.Helper()
	rows, err := st.PendingCommands()
	if err != nil {
		t.Fatal(err)
	}
	return len(rows)
}

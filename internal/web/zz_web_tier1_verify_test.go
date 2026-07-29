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

// TestTier1WebDashboard 钉死 plan 承诺的 web 契约：New(st,cfg) + Handler() +
// /api/overview、/api/tasks/{id}、/api/tasks/{id}/command 的行为。
func TestTier1WebDashboard(t *testing.T) {
	st, err := state.Open(t.TempDir() + "/s.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	id, err := st.InsertTask(state.TaskRow{IssueRef: "o/r#1", Description: "web dash", Criteria: []string{"c1"}})
	if err != nil {
		t.Fatal(err)
	}

	srv := New(st, nil) // cfg 传 nil：handler 必须容错（tier 标签可省）
	h := srv.Handler()

	// GET /api/overview -> 200 + JSON{tasks,counts}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/overview", nil))
	if w.Code != 200 {
		t.Fatalf("overview status=%d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("overview content-type=%q", ct)
	}
	var ov map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &ov); err != nil {
		t.Fatalf("overview json: %v body=%s", err, w.Body.String())
	}
	tasks, _ := ov["tasks"].([]any)
	if len(tasks) != 1 {
		t.Fatalf("overview tasks=%v want 1", tasks)
	}
	if _, ok := ov["counts"]; !ok {
		t.Fatalf("overview missing counts: %v", ov)
	}

	// GET /api/tasks/{id} -> 200 + description
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/tasks/"+id, nil))
	if w.Code != 200 {
		t.Fatalf("task status=%d", w.Code)
	}
	var td map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &td); err != nil {
		t.Fatalf("task json: %v", err)
	}
	if td["description"] != "web dash" {
		t.Fatalf("task description=%v", td["description"])
	}

	// GET /api/tasks/{bogus} -> 404
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/tasks/nope", nil))
	if w.Code != 404 {
		t.Fatalf("bogus task status=%d want 404", w.Code)
	}

	// POST /api/tasks/{id}/command {verb:cancel} -> 2xx + commands 行写入
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/tasks/"+id+"/command",
		bytes.NewReader([]byte(`{"verb":"cancel","payload":""}`))))
	if w.Code < 200 || w.Code >= 300 {
		t.Fatalf("command status=%d", w.Code)
	}
	pending, err := st.PendingCommands()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Verb != "cancel" || pending[0].TaskID != id {
		t.Fatalf("pending=%v", pending)
	}

	// POST 非法 verb -> 4xx，且不再新增行
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/tasks/"+id+"/command",
		bytes.NewReader([]byte(`{"verb":"explode"}`))))
	if w.Code >= 200 && w.Code < 300 {
		t.Fatalf("bad verb status=%d want 4xx", w.Code)
	}
	pending2, _ := st.PendingCommands()
	if len(pending2) != 1 {
		t.Fatalf("bad verb wrote extra rows: %v", pending2)
	}
}

package channel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestTier1NewLinearExplicitSignature pins the refactored explicit signature
//   func NewLinear(apiKey, endpoint, projectID, teamID string, statusMap map[string]string) *Linear
// and verifies endpoint is honored POSITIONALLY (2nd arg), projectID (3rd),
// statusMap (5th). The compile-time var-_ line is the old/new discriminator:
// a reverted variadic func(string, ...any) is NOT assignable to a fixed 5-param
// func type, so an undone refactor makes this file fail to compile (the whole
// package's tests then abort). Old-vs-new cannot be told apart by runtime
// behavior on CORRECT calls (old content-guessing was backward-compatible with
// explicit endpoint passing), so the compile pin is the real gate; the
// behavioral asserts below additionally catch a botched field assignment in
// the new constructor body (e.g. projectID/teamID swapped).
func TestTier1NewLinearExplicitSignature(t *testing.T) {
	var _ func(string, string, string, string, map[string]string) *Linear = NewLinear

	var gotProject any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotProject = req.Variables["project"]
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"issues":{"nodes":[]}}}`))
	}))
	defer srv.Close()

	// endpoint = srv.URL (2nd positional arg); projectID = "proj-positional" (3rd).
	lc := NewLinear("k", srv.URL, "proj-positional", "team-positional", map[string]string{"running": "Doing"})
	if _, err := lc.ListNewTasks(context.Background()); err != nil {
		t.Fatalf("ListNewTasks: %v (endpoint not honored as 2nd positional arg)", err)
	}
	if gotProject != "proj-positional" {
		t.Fatalf("variables.project = %v, want proj-positional (3rd positional arg)", gotProject)
	}
	if lc.statusName("running") != "Doing" {
		t.Fatalf("statusMap (5th positional arg) not honored: %q", lc.statusName("running"))
	}
}

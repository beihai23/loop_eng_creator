package channel

import (
	"context"
	"strings"
	"testing"
)

func TestLinearEnsureWorkflowStatesCreatesMissing(t *testing.T) {
	var created []string
	lc, _ := newLinearStub(t, func(q linearStubReq) any {
		switch {
		case strings.Contains(q.Query, "workflowStateCreate"):
			if n, ok := q.Variables["name"].(string); ok {
				created = append(created, n)
			}
			return stubData(map[string]any{"workflowStateCreate": map[string]any{"success": true}})
		case strings.Contains(q.Query, "states"):
			return stubData(map[string]any{"team": map[string]any{"states": map[string]any{"nodes": []map[string]any{
				{"id": "s-done", "name": "Done", "type": "completed"},
				{"id": "s-prog", "name": "In Progress", "type": "started"},
				{"id": "s-canc", "name": "Canceled", "type": "canceled"},
				{"id": "s-ni", "name": "Needs Info", "type": "started"},
			}}}})
		case strings.Contains(q.Query, "team(id:"):
			return stubData(map[string]any{"team": map[string]any{"id": "team-uuid-1"}})
		}
		t.Errorf("unexpected query: %s", q.Query)
		return stubData(map[string]any{})
	})
	if err := lc.EnsureStatusMarkers(context.Background()); err != nil {
		t.Fatalf("EnsureStatusMarkers: %v", err)
	}
	for _, want := range []string{"Needs Review", "Needs Human Decision", "Blocked"} {
		if !containsStr(created, want) {
			t.Errorf("should create %q; created=%v", want, created)
		}
	}
	for _, bad := range []string{"Done", "In Progress", "Canceled", "Needs Info"} {
		if containsStr(created, bad) {
			t.Errorf("should NOT create existing %q; created=%v", bad, created)
		}
	}
}

func TestLinearEnsureWorkflowStatesBackfillsStatusMap(t *testing.T) {
	lc, _ := newLinearStub(t, func(q linearStubReq) any {
		switch {
		case strings.Contains(q.Query, "workflowStateCreate"):
			return stubData(map[string]any{"workflowStateCreate": map[string]any{"success": true}})
		case strings.Contains(q.Query, "states"):
			return stubData(map[string]any{"team": map[string]any{"states": map[string]any{"nodes": []map[string]any{
				{"id": "s-done", "name": "Done", "type": "completed"},
			}}}})
		case strings.Contains(q.Query, "team(id:"):
			return stubData(map[string]any{"team": map[string]any{"id": "team-uuid-1"}})
		}
		t.Errorf("unexpected query: %s", q.Query)
		return stubData(map[string]any{})
	})
	if err := lc.EnsureStatusMarkers(context.Background()); err != nil {
		t.Fatalf("EnsureStatusMarkers: %v", err)
	}
	if got := lc.statusName("running"); got != "In Progress" {
		t.Errorf("status_map[running] = %q, want %q", got, "In Progress")
	}
	if got := lc.statusName("needs-info"); got != "Needs Info" {
		t.Errorf("status_map[needs-info] = %q, want %q", got, "Needs Info")
	}
}

func containsStr(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

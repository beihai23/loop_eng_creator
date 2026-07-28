package channel

import (
	"context"
	"strings"
	"testing"
)

// TestLinearEnsureWorkflowStatesCreatesMissing pins the StatusEnsurer behavior
// for Linear: at startup it maps common loop statuses to EXISTING Linear columns
// (running→In Progress, done→Done, cancelled→Canceled — no duplicates) and
// CREATES dedicated WorkflowStates only for the parked-waiting-on-human statuses
// that lack one (Needs Review / Needs Human Decision / Blocked). Idempotent —
// existing dedicated states (Needs Info) are skipped.
func TestLinearEnsureWorkflowStatesCreatesMissing(t *testing.T) {
	var created []string // names the daemon sent to workflowStateCreate
	lc, _ := newLinearStub(t, func(q linearStubReq) any {
		switch {
		case strings.Contains(q.Query, "workflowStateCreate"):
			if n, ok := q.Variables["name"].(string); ok {
				created = append(created, n)
			}
			return stubData(map[string]any{"workflowStateCreate": map[string]any{"success": true}})
		case strings.Contains(q.Query, "states"): // team-scoped states query
			return stubData(map[string]any{"team": map[string]any{"states": map[string]any{"nodes": []map[string]any{
				{"id": "s-done", "name": "Done", "type": "completed"},
				{"id": "s-prog", "name": "In Progress", "type": "started"},
				{"id": "s-canc", "name": "Canceled", "type": "canceled"},
				{"id": "s-ni", "name": "Needs Info", "type": "started"}, // already exists → skip
			}}}})
		}
		t.Errorf("unexpected query: %s", q.Query)
		return stubData(map[string]any{})
	})
	if err := lc.EnsureStatusMarkers(context.Background()); err != nil {
		t.Fatalf("EnsureStatusMarkers: %v", err)
	}
	// Must create the dedicated parked states that don't exist yet.
	for _, want := range []string{"Needs Review", "Needs Human Decision", "Blocked"} {
		if !containsStr(created, want) {
			t.Errorf("should create dedicated state %q; created=%v", want, created)
		}
	}
	// Must NOT re-create the existing common or already-present dedicated states.
	for _, bad := range []string{"Done", "In Progress", "Canceled", "Needs Info"} {
		if containsStr(created, bad) {
			t.Errorf("should NOT create existing state %q (map to it instead); created=%v", bad, created)
		}
	}
}

// TestLinearEnsureWorkflowStatesBackfillsStatusMap pins that after provisioning,
// the in-memory status_map carries the default names so resolveStateID resolves
// the (existing or created) states BY NAME rather than relying on the type
// fallback (which would shoehorn needs-info into "In Progress").
func TestLinearEnsureWorkflowStatesBackfillsStatusMap(t *testing.T) {
	lc, _ := newLinearStub(t, func(q linearStubReq) any {
		switch {
		case strings.Contains(q.Query, "workflowStateCreate"):
			return stubData(map[string]any{"workflowStateCreate": map[string]any{"success": true}})
		case strings.Contains(q.Query, "states"):
			return stubData(map[string]any{"team": map[string]any{"states": map[string]any{"nodes": []map[string]any{
				{"id": "s-done", "name": "Done", "type": "completed"},
			}}}})
		}
		t.Errorf("unexpected query: %s", q.Query)
		return stubData(map[string]any{})
	})
	if err := lc.EnsureStatusMarkers(context.Background()); err != nil {
		t.Fatalf("EnsureStatusMarkers: %v", err)
	}
	// running maps to "In Progress" (the default name), needs-info to "Needs Info".
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

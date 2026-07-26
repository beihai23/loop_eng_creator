package channel

// Tier-1 acceptance for the GitHub channel's label-ensure + decoupled
// UpdateStatus (#46/#54: loop:<status> labels were missing from the repo, so
// `gh issue edit --add-label loop:running` 404'd, and because edit is atomic the
// --remove-label never ran either — leaving the old status label stuck).
//
// Contract pinned here, black-box through (*GitHub).UpdateStatus only:
//   - UpdateStatus idempotently ensures the full loop:<status> label set via
//     `gh label create loop:<status> --force` (best-effort, never blocks).
//   - UpdateStatus runs remove in its own edit BEFORE add; add failure does NOT
//     roll back remove, so a missing-label 404 can no longer strand the old label.
//
// The recorder is named ghRecorder on purpose — an earlier round redeclared the
// Linear test's tier1Rec here and broke the build. Do not reintroduce that name.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// ghRecorder is a recording ghFunc: every call is appended to calls and resp
// classifies by args to return canned output or an error. Tests assert on calls.
type ghRecorder struct {
	mu    sync.Mutex
	calls [][]string
	resp  func(args []string) ([]byte, error)
}

func (r *ghRecorder) run(_ context.Context, args ...string) ([]byte, error) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string(nil), args...))
	resp := r.resp
	r.mu.Unlock()
	if resp == nil {
		return nil, nil
	}
	return resp(args)
}

// hasCall reports whether some recorded call contains every wanted token
// (order-independent; gh flag order varies).
func (r *ghRecorder) hasCall(want ...string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.calls {
		ok := true
		for _, w := range want {
			if !tokHas(c, w) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// removeBeforeAdd reports whether the first issue-edit with --remove-label was
// recorded before the first issue-edit with --add-label (remove runs first).
func (r *ghRecorder) removeBeforeAdd() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	rmIdx, addIdx := -1, -1
	for i, c := range r.calls {
		if len(c) < 2 || c[0] != "issue" || c[1] != "edit" {
			continue
		}
		if rmIdx == -1 && tokHas(c, "--remove-label") {
			rmIdx = i
		}
		if addIdx == -1 && tokHas(c, "--add-label") {
			addIdx = i
		}
	}
	return rmIdx != -1 && addIdx != -1 && rmIdx < addIdx
}

func tokHas(s []string, w string) bool {
	for _, v := range s {
		if v == w {
			return true
		}
	}
	return false
}

// ghClassify sorts a recorded call into a stable bucket for resp dispatchers.
func ghClassify(args []string) string {
	if len(args) < 2 {
		return "other"
	}
	switch {
	case args[0] == "label" && args[1] == "create":
		return "label-create"
	case args[0] == "issue" && args[1] == "view":
		return "issue-view"
	case args[0] == "issue" && args[1] == "edit" && tokHas(args, "--remove-label"):
		return "edit-remove"
	case args[0] == "issue" && args[1] == "edit" && tokHas(args, "--add-label"):
		return "edit-add"
	}
	return "other"
}

// TestTier1GitHubEnsureLabelsCreatesStatusSet: UpdateStatus must idempotently
// ensure the full loop:<status> label set — including loop:running, the exact
// label missing from the repo in #54 — via `gh label create ... --force`.
func TestTier1GitHubEnsureLabelsCreatesStatusSet(t *testing.T) {
	rec := &ghRecorder{resp: func(args []string) ([]byte, error) {
		if ghClassify(args) == "issue-view" {
			return []byte(`{"labels":[]}`), nil // no existing status labels
		}
		return nil, nil // label create + add succeed
	}}
	g := &GitHub{Repo: "owner/repo", TaskLabel: "loop:task", ghFunc: rec.run}

	if err := g.UpdateStatus(context.Background(), "46", "running"); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	// The #54 label: must be created with --force.
	if !rec.hasCall("label", "create", "loop:running", "--force") {
		t.Fatalf("EnsureLabels did not create loop:running --force; calls=%v", rec.calls)
	}
	// The whole status set, not just the one being applied.
	for _, s := range loopStatusNames {
		if !rec.hasCall("label", "create", "loop:"+s, "--force") {
			t.Fatalf("EnsureLabels did not create loop:%s --force; calls=%v", s, rec.calls)
		}
	}
	// Task identity label too.
	if !rec.hasCall("label", "create", "loop:task", "--force") {
		t.Fatalf("EnsureLabels did not create task label; calls=%v", rec.calls)
	}
}

// TestTier1GitHubUpdateStatusRemoveSurvivesAddFailure: the #54 regression. The
// operator lacks labels:write (label create fails) AND loop:running is still
// missing (add 404s). UpdateStatus MUST still run remove in its own edit first —
// the old loop:blocked gets cleared even though add fails afterward.
func TestTier1GitHubUpdateStatusRemoveSurvivesAddFailure(t *testing.T) {
	labelsJSON := []byte(`{"labels":[{"name":"loop:task"},{"name":"loop:blocked"}]}`)
	rec := &ghRecorder{resp: func(args []string) ([]byte, error) {
		switch ghClassify(args) {
		case "label-create":
			return nil, errors.New("403 Resource not accessible by personal access token") // no labels:write
		case "issue-view":
			return labelsJSON, nil
		case "edit-remove":
			return nil, nil // remove succeeds
		case "edit-add":
			return nil, errors.New("'loop:running' not found: failed to update 1 issue") // #54
		}
		return nil, nil
	}}
	g := &GitHub{Repo: "owner/repo", TaskLabel: "loop:task", ghFunc: rec.run}

	err := g.UpdateStatus(context.Background(), "54", "running")
	if err == nil {
		t.Fatalf("UpdateStatus: want error from failed add, got nil")
	}
	// The whole point: remove ran despite add failing.
	if !rec.hasCall("issue", "edit", "--remove-label", "loop:blocked") {
		t.Fatalf("remove did not execute before add failed; calls=%v", rec.calls)
	}
	if !rec.hasCall("issue", "edit", "--add-label", "loop:running") {
		t.Fatalf("add was not attempted; calls=%v", rec.calls)
	}
	if !rec.removeBeforeAdd() {
		t.Fatalf("remove must precede add (decoupled, remove first); calls=%v", rec.calls)
	}
}

// TestTier1GitHubUpdateStatusAddRemoveRegression: happy path — labels exist,
// everything succeeds. The decoupling must not lose either edit: old status is
// removed and new status is added (remove first).
func TestTier1GitHubUpdateStatusAddRemoveRegression(t *testing.T) {
	labelsJSON := []byte(`{"labels":[{"name":"loop:task"},{"name":"loop:blocked"}]}`)
	rec := &ghRecorder{resp: func(args []string) ([]byte, error) {
		if ghClassify(args) == "issue-view" {
			return labelsJSON, nil
		}
		return nil, nil // label create + both edits succeed
	}}
	g := &GitHub{Repo: "owner/repo", TaskLabel: "loop:task", ghFunc: rec.run}

	if err := g.UpdateStatus(context.Background(), "30", "running"); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	if !rec.hasCall("issue", "edit", "--remove-label", "loop:blocked") {
		t.Fatalf("regression: remove loop:blocked not issued; calls=%v", rec.calls)
	}
	if !rec.hasCall("issue", "edit", "--add-label", "loop:running") {
		t.Fatalf("regression: add loop:running not issued; calls=%v", rec.calls)
	}
	if !rec.removeBeforeAdd() {
		t.Fatalf("regression: remove must precede add; calls=%v", rec.calls)
	}
}

// TestGitHubEnsureLabelsIsBestEffort: every label create failing (e.g. no
// labels:write scope) is logged, not returned — EnsureLabels is best-effort
// and never blocks UpdateStatus even when the whole set 403s. The full set is
// still attempted (no short-circuit on first failure).
func TestGitHubEnsureLabelsIsBestEffort(t *testing.T) {
	rec := &ghRecorder{resp: func(args []string) ([]byte, error) {
		if ghClassify(args) == "label-create" {
			return nil, errors.New("no permission")
		}
		return nil, nil
	}}
	g := &GitHub{Repo: "owner/repo", TaskLabel: "loop:task", ghFunc: rec.run}

	// EnsureLabels is void — best-effort, swallows all create errors.
	g.EnsureLabels(context.Background())

	wantCreates := len(loopStatusNames) + 1 // every status + the task label
	gotCreates := 0
	rec.mu.Lock()
	for _, c := range rec.calls {
		if ghClassify(c) == "label-create" {
			gotCreates++
		}
	}
	rec.mu.Unlock()
	if gotCreates != wantCreates {
		t.Fatalf("EnsureLabels issued %d label creates, want %d (no short-circuit on failure); calls=%v", gotCreates, wantCreates, rec.calls)
	}
}

// TestTier1UpdateStatusCustomPrefix：LabelPrefix=ai: 的实例打 ai:running、创建
// ai:* 状态族 + 任务标签——整组标签随前缀派生（自定义前缀端到端的最小证据）。
func TestTier1UpdateStatusCustomPrefix(t *testing.T) {
	labelsJSON := []byte(`{"labels":[{"name":"ai:task"},{"name":"ai:blocked"}]}`)
	rec := &ghRecorder{resp: func(args []string) ([]byte, error) {
		if ghClassify(args) == "issue-view" {
			return labelsJSON, nil
		}
		return nil, nil
	}}
	g := &GitHub{Repo: "owner/repo", TaskLabel: "ai:task", LabelPrefix: "ai:", ghFunc: rec.run}
	if err := g.UpdateStatus(context.Background(), "42", "running"); err != nil {
		t.Fatal(err)
	}
	if !rec.hasCall("issue", "edit", "--add-label", "ai:running") {
		t.Fatalf("UpdateStatus must add ai:running under custom prefix; calls=%v", rec.calls)
	}
	if !rec.hasCall("issue", "edit", "--remove-label", "ai:blocked") {
		t.Fatalf("UpdateStatus must remove old ai: status; calls=%v", rec.calls)
	}
	if !rec.hasCall("label", "create", "ai:running", "--force") || !rec.hasCall("label", "create", "ai:task", "--force") {
		t.Fatalf("EnsureLabels must create the ai: family + task label; calls=%v", rec.calls)
	}
}

// TestTier1RequiredGitHubLabelsPrefix：preflight/向导共用的所需标签集合随前缀
// 派生（ai: → ai:running… + ai:task）；空前缀回落 loop:。
func TestTier1RequiredGitHubLabelsPrefix(t *testing.T) {
	got := RequiredGitHubLabels("ai:", "ai:task")
	for _, want := range []string{"ai:running", "ai:done", "ai:blocked", "ai:task"} {
		found := false
		for _, n := range got {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("RequiredGitHubLabels(ai:) missing %q: %v", want, got)
		}
	}
	for _, n := range got {
		if strings.HasPrefix(n, "loop:") {
			t.Fatalf("custom-prefix set must not contain loop: labels: %v", got)
		}
	}
}

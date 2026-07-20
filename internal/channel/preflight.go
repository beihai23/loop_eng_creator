package channel

// Preflight is the channel provider readiness check: confirm the provider's
// prerequisites are configured before the daemon starts, fail fast with an
// actionable checklist instead of discovering a gap mid-run (#65). This is the
// preventive complement to #58's runtime tolerance — #58 keeps UpdateStatus
// from crashing when a label is missing; preflight makes the daemon refuse to
// start until that label exists, so the operator learns at boot, not at the
// first status move.
//
// Integration points:
//   - `loop-eng daemon` runs Preflight once at startup; any issue refuses start.
//   - `loop-eng doctor` runs the same check on demand and prints the report.
//
// Local has no prerequisites and does NOT implement Preflighter (always ready).

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// PreflightIssue is one missing or misconfigured prerequisite a Preflight found.
// An empty/nil issue list (with nil error) means the channel is ready.
type PreflightIssue struct {
	// Code is a stable machine identifier for the issue kind:
	// "missing-label" | "missing-project" | "unresolvable-status" | "auth".
	Code string
	// Target is the specific name/id/config key that's missing or unresolvable
	// (e.g. "loop:running", `status_map[done]="Finished"`). Empty when N/A.
	Target string
	// Message is a human-readable, actionable explanation (what's wrong + how
	// to fix). Printed verbatim by the daemon/doctor checklist.
	Message string
}

// Preflighter is an optional capability a Channel may implement: a startup
// readiness check that lists missing prerequisites so the daemon can fail fast
// with a checklist instead of crashing mid-run.
//
// Contract (pinned by preflight_tier1_test.go — a signature drift fails the
// build there, not just at a call site):
//
//	Preflight(ctx context.Context) ([]PreflightIssue, error)
//
// Returns the missing-item list (nil/empty = ready) and a separate error ONLY
// when the check itself could not complete (e.g. `gh` unreachable, GraphQL
// endpoint down). Config gaps are issues; infra failures are errors.
type Preflighter interface {
	Preflight(ctx context.Context) ([]PreflightIssue, error)
}

// Compile-time guarantees that GitHub and Linear satisfy Preflighter. This also
// pins the signature: an accidental return-type/arity drift fails the build
// here, at the definition, rather than surfacing as a confusing call-site error.
var (
	_ Preflighter = (*GitHub)(nil)
	_ Preflighter = (*Linear)(nil)
)

// --- GitHub ----------------------------------------------------------------

// Preflight checks the repo carries the full loop label set the engine moves
// tasks through: the task identity label (loop:task) + every loop:<status>
// (#54 found loop:running missing at runtime — UpdateStatus's --add-label
// 404'd). Missing labels are reported by name so the operator knows exactly
// which `gh label create` to run. `gh label list` failing outright (no `gh`,
// no auth, network) is a hard error — the check couldn't run, don't guess.
func (g *GitHub) Preflight(ctx context.Context) ([]PreflightIssue, error) {
	out, err := g.gh(ctx, "label", "list", "--repo", g.Repo, "--json", "name", "--limit", "200")
	if err != nil {
		return nil, fmt.Errorf("github preflight: gh label list %s: %w", g.Repo, err)
	}
	// `gh label list --json name` emits a top-level array: [{"name":"bug"},...].
	var labels []ghIssueLabel
	if err := json.Unmarshal(out, &labels); err != nil {
		return nil, fmt.Errorf("github preflight: parse label list: %w", err)
	}
	existing := make(map[string]bool, len(labels))
	for _, l := range labels {
		existing[l.Name] = true
	}
	var issues []PreflightIssue
	for _, name := range g.requiredLabels() {
		if existing[name] {
			continue
		}
		issues = append(issues, PreflightIssue{
			Code:    "missing-label",
			Target:  name,
			Message: fmt.Sprintf("仓库 %s 缺少标签 %q（引擎会用到；补齐：`gh label create %s --repo %s --force`）", g.Repo, name, name, g.Repo),
		})
	}
	return issues, nil
}

// requiredLabels is the full loop label set Preflight checks for, in a stable
// order: the status family (loopStatusNames order) then the task identity label.
// Stable order → deterministic checklist output.
func (g *GitHub) requiredLabels() []string {
	required := make([]string, 0, len(loopStatusNames)+1)
	for _, s := range loopStatusNames {
		required = append(required, "loop:"+s)
	}
	if g.TaskLabel != "" {
		required = append(required, g.TaskLabel)
	}
	return required
}

// --- Linear ----------------------------------------------------------------

// Preflight checks the three Linear prerequisites (mapping doc §9):
//  1. API key valid — one whoami (viewer) call; a bad key can't reach anything.
//  2. project exists — the configured project (channel.linear.project) resolves.
//  3. status_map targets resolvable — every status_map value names a real
//     WorkflowState in the team's workflow (by name, case-insensitive).
//
// A bad key short-circuits to a single auth issue: subsequent queries would
// just echo the auth failure and bury the real cause in noise. Otherwise every
// gap is collected so the operator sees the whole checklist in one pass.
func (lc *Linear) Preflight(ctx context.Context) ([]PreflightIssue, error) {
	if err := lc.whoami(ctx); err != nil {
		return []PreflightIssue{{
			Code:    "auth",
			Target:  LinearAPIKeyEnv,
			Message: fmt.Sprintf("Linear API key 无效或不可用（whoami 失败：%v；检查环境变量 %s）", err, LinearAPIKeyEnv),
		}}, nil
	}

	var issues []PreflightIssue

	if ok, err := lc.projectExists(ctx); err != nil {
		return nil, fmt.Errorf("linear preflight: project lookup: %w", err)
	} else if !ok {
		issues = append(issues, PreflightIssue{
			Code:    "missing-project",
			Target:  lc.project(),
			Message: fmt.Sprintf("Linear project %q 不存在或不可见（检查 config channel.linear.project；在 Linear 里 Cmd/Ctrl+K → Copy model UUID 取 project UUID）", lc.project()),
		})
	}

	if sm := lc.combinedStatusMap(); len(sm) > 0 {
		states, err := lc.workflowStates(ctx)
		if err != nil {
			return nil, fmt.Errorf("linear preflight: workflowStates: %w", err)
		}
		// Stable order by status name → deterministic checklist.
		statuses := make([]string, 0, len(sm))
		for s := range sm {
			statuses = append(statuses, s)
		}
		slices.Sort(statuses)
		for _, status := range statuses {
			name := sm[status]
			if name == "" || stateNameExists(states, name) {
				continue
			}
			issues = append(issues, PreflightIssue{
				Code:    "unresolvable-status",
				Target:  fmt.Sprintf("status_map[%s]=%q", status, name),
				Message: fmt.Sprintf("status_map[%q]=%q 在 workflowStates 里找不到同名 WorkflowState（检查拼写，或在该 team 里补建该列）", status, name),
			})
		}
	}

	return issues, nil
}

// whoami validates the API key with a single viewer query (Linear's equivalent
// of "is this key live"). A bad key surfaces as a GraphQL error from gql(); an
// empty viewer means the key has no viewer scope.
func (lc *Linear) whoami(ctx context.Context) error {
	const q = `query Whoami { viewer { id name } }`
	var out struct {
		Viewer struct {
			ID string `json:"id"`
		} `json:"viewer"`
	}
	if err := lc.gql(ctx, q, &out); err != nil {
		return err
	}
	if out.Viewer.ID == "" {
		return fmt.Errorf("viewer 为空（key 无 viewer 权限）")
	}
	return nil
}

// projectExists reports whether the configured project resolves via project(id:).
// project(id:) returning null (not an error) means "not found" → config gap,
// surfaced as an issue by the caller, not a hard error.
func (lc *Linear) projectExists(ctx context.Context) (bool, error) {
	const q = `query ProjectExists($project: String!) { project(id: $project) { id name } }`
	var out struct {
		Project struct {
			ID string `json:"id"`
		} `json:"project"`
	}
	if err := lc.gql(ctx, q, map[string]any{"project": lc.project()}, &out); err != nil {
		return false, err
	}
	return out.Project.ID != "", nil
}

// combinedStatusMap merges the exported and unexported status maps. The Linear
// struct keeps both in sync (NewLinear writes both); Preflight must consult
// either source the way runtime resolution (statusName) does.
func (lc *Linear) combinedStatusMap() map[string]string {
	m := make(map[string]string, len(lc.StatusMap)+len(lc.statusMap))
	for k, v := range lc.StatusMap {
		m[k] = v
	}
	for k, v := range lc.statusMap {
		m[k] = v
	}
	return m
}

// stateNameExists reports whether any WorkflowState matches name (case-insensitive),
// matching resolveStateID's name-matching semantics.
func stateNameExists(states []linearState, name string) bool {
	for _, s := range states {
		if strings.EqualFold(s.Name, name) {
			return true
		}
	}
	return false
}

package web

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"loop-eng/internal/state"
)

// ── helpers ──────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// statusOf looks up a task's current status via ListStatuses (mirrors
// tui/detail.go statusOfTask). state.GetTask returns a TaskRow without a Status
// field, so the detail view reads status through here.
func (s *Server) statusOf(id string) string {
	rows, err := s.st.ListStatuses()
	if err != nil {
		return ""
	}
	for _, r := range rows {
		if r.ID == id {
			return r.Status
		}
	}
	return ""
}

func truncateDetail(s string, n int) string {
	rs := []rune(s)
	if len(rs) > n {
		return string(rs[:n]) + "…"
	}
	return s
}

// ── GET /api/overview ────────────────────────────────────────────────────────

type overviewTask struct {
	ID          string `json:"id"`
	IssueRef    string `json:"issue_ref"`
	Description string `json:"description"`
	TaskType    string `json:"task_type"`
	Status      string `json:"status"`
	CreatedAt   string `json:"created_at"`
	LastRunAt   string `json:"last_run_at"`
}

type runningInfo struct {
	TaskID    string `json:"task_id"`
	Phase     string `json:"phase"`
	StartedAt string `json:"started_at"`
	RunID     string `json:"run_id"`
	Retry     int    `json:"retry"`
}

type overviewResponse struct {
	Tasks   []overviewTask `json:"tasks"`             // never nil → empty DB marshals to [], not null
	Counts  map[string]int `json:"counts"`            // never nil → empty DB marshals to {}
	Running *runningInfo   `json:"running,omitempty"` // nil when no active sub-loop
}

// handleOverview mirrors tui/reader.go ReadSnapshot: tasks from TasksByStatus,
// counts bucketed by status, and the in-flight task (if any) with its phase/run/
// retry derived from ActiveRun + the last budget_ledger retry row.
func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	tasks, err := s.st.TasksByStatus()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	out := overviewResponse{
		Tasks:  make([]overviewTask, 0, len(tasks)),
		Counts: make(map[string]int),
	}
	for _, t := range tasks {
		out.Tasks = append(out.Tasks, overviewTask{
			ID: t.ID, IssueRef: t.IssueRef, Description: t.Description,
			TaskType: t.TaskType, Status: t.Status, CreatedAt: t.CreatedAt, LastRunAt: t.LastRunAt,
		})
		out.Counts[t.Status]++
	}
	if ifl, ok, err := s.st.InFlight(); err == nil && ok {
		ri := &runningInfo{TaskID: ifl.TaskID, Phase: ifl.Phase}
		if runID, startedAt, rok, _ := s.st.ActiveRun(ifl.TaskID); rok {
			ri.RunID = runID
			ri.StartedAt = startedAt
			if rows, e := s.st.BudgetLedger(runID); e == nil {
				for i := len(rows) - 1; i >= 0; i-- {
					if rows[i].Kind == "retry" {
						ri.Retry = rows[i].Amount
						break
					}
				}
			}
		}
		out.Running = ri
		// Single-active means at most one running task; mirror ReadSnapshot's
		// defensive override so the count is exactly 1 when a sub-loop is live.
		out.Counts["running"] = 1
	}
	writeJSON(w, http.StatusOK, out)
}

// ── GET /api/tasks/{id} ──────────────────────────────────────────────────────

type tierView struct {
	Tier   int    `json:"tier"`
	Label  string `json:"label"`
	Passed *bool  `json:"passed"` // nil = 未触发（无 verification 行）
	Status string `json:"status"` // "passed" / "<原因>" / "—"
}

type budgetView struct {
	Used  int `json:"used"`
	Limit int `json:"limit"`
}

type detailResponse struct {
	ID          string     `json:"id"`
	IssueRef    string     `json:"issue_ref"`
	Title       string     `json:"title"`
	Description string     `json:"description"`
	TaskType    string     `json:"task_type"`
	Status      string     `json:"status"`
	Criteria    []string   `json:"criteria"` // never nil → empty marshals to []
	Body        string     `json:"body,omitempty"`
	RunID       string     `json:"run_id"`
	ActiveRun   bool       `json:"active_run"`
	Tiers       []tierView `json:"tiers"`
	Budget      budgetView `json:"budget"`
}

// handleDetail mirrors tui/detail.go RenderDetail's assembly: GetTask (404 on
// sql.ErrNoRows — never 500), the run-scoped data (prefer active run, fall back
// to the latest run so done/blocked tasks still show tiers+budget), per-tier
// verify status, and token budget summed from the budget_ledger.
func (s *Server) handleDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	t, err := s.st.GetTask(id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSONError(w, http.StatusNotFound, fmt.Errorf("task %s not found", id))
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	criteria := t.Criteria
	if criteria == nil {
		criteria = []string{}
	}

	// run-scoped data: prefer the active run; done/blocked have none, so fall
	// back to the latest run (RunsOfTask tail) — same as detail.go:31-40.
	runID, _, activeOK, _ := s.st.ActiveRun(id)
	if !activeOK {
		if runs, e := s.st.RunsOfTask(id); e == nil && len(runs) > 0 {
			runID = runs[len(runs)-1].ID
		}
	}
	var vers []state.VerificationRow
	var used, limit int
	if runID != "" {
		vers, _ = s.st.VerificationsByRun(runID)
		if rows, e := s.st.BudgetLedger(runID); e == nil {
			for _, b := range rows {
				if b.Kind == "tokens" {
					used += b.Amount
				}
			}
		}
	}
	if s.cfg != nil {
		limit = s.cfg.Budget.PerTaskTokens
	}

	resp := detailResponse{
		ID: t.ID, IssueRef: t.IssueRef, Title: t.Title, Description: t.Description,
		TaskType: t.TaskType, Status: s.statusOf(id), Criteria: criteria, Body: t.Body,
		RunID: runID, ActiveRun: activeOK,
		Tiers:  s.verifyTiers(vers),
		Budget: budgetView{Used: used, Limit: limit},
	}
	writeJSON(w, http.StatusOK, resp)
}

// verifyTiers mirrors tui/detail.go verifyTiers: tier-1 label from the tier=1
// verification row's Detail (before the colon), tier-2 = cfg.Models.Verify.Name,
// tier-3 only when cfg.Verify.Tier3Human. cfg==nil → defaults (LLM tier-2, no
// tier-3), keeping the handler nil-safe for tests.
func (s *Server) verifyTiers(vers []state.VerificationRow) []tierView {
	out := []tierView{
		{Tier: 1, Label: tier1Label(vers), Status: tierStatus(vers, 1), Passed: tierPassed(vers, 1)},
	}
	name := "LLM"
	if s.cfg != nil && s.cfg.Models.Verify.Name != "" {
		name = s.cfg.Models.Verify.Name
	}
	out = append(out, tierView{Tier: 2, Label: name + " (LLM diff)", Status: tierStatus(vers, 2), Passed: tierPassed(vers, 2)})
	if s.cfg != nil && s.cfg.Verify.Tier3Human {
		out = append(out, tierView{Tier: 3, Label: "人审 (issue 评论)", Status: tierStatus(vers, 3), Passed: tierPassed(vers, 3)})
	}
	return out
}

// tier1Label takes the tier=1 verification row's Detail before the colon (the
// planner-produced acceptance label, e.g. "go test ./...: ok"). Mirrors
// tui/detail.go tier1Label; "(plan 未产出)" when no tier=1 row exists.
func tier1Label(vers []state.VerificationRow) string {
	for _, v := range vers {
		if v.Tier == 1 {
			return strings.TrimSpace(strings.SplitN(v.Detail, ":", 2)[0])
		}
	}
	return "(plan 未产出)"
}

func tierStatus(vers []state.VerificationRow, tier int) string {
	for _, v := range vers {
		if v.Tier == tier {
			if v.Passed {
				return "passed"
			}
			return truncateDetail(v.Detail, 80)
		}
	}
	return "—" // 未触发
}

func tierPassed(vers []state.VerificationRow, tier int) *bool {
	for _, v := range vers {
		if v.Tier == tier {
			b := v.Passed
			return &b
		}
	}
	return nil
}

// ── GET /api/tasks/{id}/trace ────────────────────────────────────────────────

type stepView struct {
	Seq       int    `json:"seq"`
	Role      string `json:"role"`
	Skill     string `json:"skill"`
	ModelRef  string `json:"model_ref"`
	Status    string `json:"status"`
	Error     string `json:"error"`
	TokensIn  int    `json:"tokens_in"`
	TokensOut int    `json:"tokens_out"`
	At        string `json:"at"`
}

type verificationView struct {
	Tier   int    `json:"tier"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}

type budgetRowView struct {
	Scope  string `json:"scope"`
	Kind   string `json:"kind"`
	Amount int    `json:"amount"`
	Limit  int    `json:"limit"`
}

type runTrace struct {
	RunID         string             `json:"run_id"`
	StartedAt     string             `json:"started_at"`
	EndedAt       string             `json:"ended_at"`
	Outcome       string             `json:"outcome"`
	Steps         []stepView         `json:"steps"`         // never nil
	Verifications []verificationView `json:"verifications"` // never nil
	Budget        []budgetRowView    `json:"budget"`        // never nil
}

type transitionView struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
	At     string `json:"at"`
}

type traceResponse struct {
	TaskID      string           `json:"task_id"`
	Runs        []runTrace       `json:"runs"` // never nil
	Transitions []transitionView `json:"transitions"`
}

// handleTrace builds the per-run timeline: each run carries its steps (Replay),
// per-tier verifications, and budget ledger rows, plus the task's full lifecycle
// transitions (already chronological by rowid). A nonexistent task → 404 so the
// SPA can distinguish "no data yet" from "no such task".
func (s *Server) handleTrace(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.st.GetTask(id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSONError(w, http.StatusNotFound, fmt.Errorf("task %s not found", id))
			return
		}
	}
	runs, _ := s.st.RunsOfTask(id)
	out := traceResponse{TaskID: id, Runs: []runTrace{}}
	for _, run := range runs {
		steps, _ := s.st.Replay(run.ID)
		vers, _ := s.st.VerificationsByRun(run.ID)
		budget, _ := s.st.BudgetLedger(run.ID)
		out.Runs = append(out.Runs, runTrace{
			RunID:         run.ID,
			StartedAt:     run.StartedAt,
			EndedAt:       run.EndedAt,
			Outcome:       run.Outcome,
			Steps:         toStepViews(steps),
			Verifications: toVerificationViews(vers),
			Budget:        toBudgetViews(budget),
		})
	}
	trans, _ := s.st.Transitions(id)
	out.Transitions = toTransitionViews(trans)
	writeJSON(w, http.StatusOK, out)
}

func toStepViews(in []state.StepRow) []stepView {
	out := make([]stepView, 0, len(in))
	for _, s := range in {
		out = append(out, stepView{
			Seq: s.Seq, Role: s.Role, Skill: s.Skill, ModelRef: s.ModelRef,
			Status: s.Status, Error: s.Error, TokensIn: s.TokensIn, TokensOut: s.TokensOut, At: s.At,
		})
	}
	return out
}

func toVerificationViews(in []state.VerificationRow) []verificationView {
	out := make([]verificationView, 0, len(in))
	for _, v := range in {
		out = append(out, verificationView{Tier: v.Tier, Passed: v.Passed, Detail: v.Detail})
	}
	return out
}

func toBudgetViews(in []state.BudgetRow) []budgetRowView {
	out := make([]budgetRowView, 0, len(in))
	for _, b := range in {
		out = append(out, budgetRowView{Scope: b.Scope, Kind: b.Kind, Amount: b.Amount, Limit: b.Limit})
	}
	return out
}

func toTransitionViews(in []state.TransitionRow) []transitionView {
	out := make([]transitionView, 0, len(in))
	for _, t := range in {
		out = append(out, transitionView{From: t.From, To: t.To, Reason: t.Reason, At: t.At})
	}
	return out
}

// ── POST /api/tasks/{id}/command ─────────────────────────────────────────────

type commandRequest struct {
	Verb    string `json:"verb"`
	Payload string `json:"payload"`
}

// handleCommand is the dashboard → daemon control channel. verb must be
// "resume" or "cancel" (the exact case set daemon/engine.go applyCommand
// translates into a transition); anything else → 400 and nothing is written.
// A valid verb appends a row via InsertCommand (applied_at NULL) which the
// daemon's drainCommands picks up on its next tick.
func (s *Server) handleCommand(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req commandRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, errors.New("invalid JSON body"))
		return
	}
	switch req.Verb {
	case "resume", "cancel":
	default:
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("unknown verb %q (want resume|cancel)", req.Verb))
		return
	}
	if err := s.st.InsertCommand(id, req.Verb, req.Payload); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "verb": req.Verb})
}

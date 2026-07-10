package state

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }

type TaskRow struct {
	ID, IssueRef, Description, TaskType, Source string
	Criteria                                    []string
}

var schema = []string{
	`CREATE TABLE IF NOT EXISTS tasks(
			id TEXT PRIMARY KEY,
			issue_ref TEXT, description TEXT, task_type TEXT, source TEXT,
			acceptance_criteria_json TEXT, created_at TEXT, updated_at TEXT)`,
	`CREATE TABLE IF NOT EXISTS task_status(
			task_id TEXT PRIMARY KEY, status TEXT, parked_detail TEXT, updated_at TEXT)`,
	`CREATE TABLE IF NOT EXISTS runs(
			id TEXT PRIMARY KEY, task_id TEXT, started_at TEXT, ended_at TEXT,
			outcome TEXT, total_tokens INTEGER, retry_count INTEGER)`,
	`CREATE TABLE IF NOT EXISTS steps(
			id TEXT PRIMARY KEY, run_id TEXT, seq INTEGER, role TEXT, skill TEXT,
			model_ref TEXT, input_hash TEXT, input_json TEXT, output_json TEXT,
			tokens_in INTEGER, tokens_out INTEGER, status TEXT, error TEXT, at TEXT)`,
	`CREATE TABLE IF NOT EXISTS verifications(
			id TEXT PRIMARY KEY, step_id TEXT, tier INTEGER, passed INTEGER, detail TEXT, at TEXT)`,
	`CREATE TABLE IF NOT EXISTS transitions(
			id TEXT PRIMARY KEY, task_id TEXT, from_status TEXT, to_status TEXT, reason TEXT, at TEXT)`,
	`CREATE TABLE IF NOT EXISTS budget_ledger(
			id TEXT PRIMARY KEY, run_id TEXT, scope TEXT, kind TEXT, amount INTEGER, limit_val INTEGER, at TEXT)`,
	`CREATE TABLE IF NOT EXISTS reports(
			id TEXT PRIMARY KEY, task_id TEXT, round INTEGER, channel_ref TEXT, at TEXT)`,
	`CREATE INDEX IF NOT EXISTS idx_steps_run ON steps(run_id, seq)`,
	`CREATE INDEX IF NOT EXISTS idx_trans_task ON transitions(task_id, at)`,
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	for _, q := range schema {
		if _, err := db.Exec(q); err != nil {
			db.Close()
			return nil, fmt.Errorf("migrate: %w", err)
		}
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) InsertTask(t TaskRow) (string, error) {
	id := newID("task")
	crit, _ := json.Marshal(t.Criteria)
	now := nowISO()
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(
		`INSERT INTO tasks(id, issue_ref, description, task_type, source, acceptance_criteria_json, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?,?)`,
		id, t.IssueRef, t.Description, t.TaskType, t.Source, string(crit), now, now); err != nil {
		tx.Rollback()
		return "", err
	}
	if _, err := tx.Exec(`INSERT INTO task_status(task_id, status, updated_at) VALUES(?,?,?)`,
		id, "new", now); err != nil {
		tx.Rollback()
		return "", err
	}
	if err := tx.Commit(); err != nil {
		tx.Rollback()
		return "", err
	}
	return id, nil
}

func (s *Store) GetTask(id string) (TaskRow, error) {
	row := s.db.QueryRow(
		`SELECT id, issue_ref, description, task_type, source, acceptance_criteria_json FROM tasks WHERE id=?`, id)
	var t TaskRow
	var critJSON string
	if err := row.Scan(&t.ID, &t.IssueRef, &t.Description, &t.TaskType, &t.Source, &critJSON); err != nil {
		return t, err
	}
	_ = json.Unmarshal([]byte(critJSON), &t.Criteria)
	return t, nil
}

// StatusRow is one row of the task_status table surfaced to the CLI status
// command. 裁决 F: status.go does not touch the Store's private db field —
// it reads task_status via this method only.
type StatusRow struct {
	ID, Status string
}

// InFlight is the live view of the single active sub-loop (spec §8.7
// observability data layer + §8.3 single-active). TaskID is the task currently
// occupying the active slot; Phase is that task's most recent sub-loop step
// role (triage|plan|execute|verify). Single-active by construction (spec §12)
// means at most one running task, so at most one InFlight — there is no list.
type InFlight struct {
	TaskID string
	Phase  string
}

// InFlight returns the currently active sub-loop: the single task in
// task_status with status="running" (spec §8.7: only running occupies the
// active slot; FIFO ingest order is irrelevant once a task is dispatched) plus
// its latest phase. ok is false when the active slot is empty. Phase is the
// most recent steps.role for that task — the sub-loop writes steps keyed by
// task id (RunID = task id, see subloop.go), so run_id IS the task id here and
// no runs-table join is needed; Phase is "" before the first step lands. This
// is the read side of the single-active invariant and the `loop-eng status
// --watch` live view.
func (s *Store) InFlight() (InFlight, bool, error) {
	var taskID string
	err := s.db.QueryRow(
		`SELECT task_id FROM task_status WHERE status='running' LIMIT 1`).Scan(&taskID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return InFlight{}, false, nil
		}
		return InFlight{}, false, err
	}
	// Phase = newest step role for this task. Steps are append-only; ordering by
	// at (RFC3339Nano → lexical = chronological) with rowid tiebreak is
	// deterministic even when two steps share a timestamp.
	var phase sql.NullString
	_ = s.db.QueryRow(
		`SELECT role FROM steps WHERE run_id=? ORDER BY at DESC, rowid DESC LIMIT 1`, taskID).Scan(&phase)
	return InFlight{TaskID: taskID, Phase: phase.String}, true, nil
}

// ListStatuses returns every task_status row (id + status). Ordered by
// updated_at so the most recently touched tasks surface first.
func (s *Store) ListStatuses() ([]StatusRow, error) {
	rows, err := s.db.Query(`SELECT task_id, status FROM task_status ORDER BY updated_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StatusRow
	for rows.Next() {
		var r StatusRow
		if err := rows.Scan(&r.ID, &r.Status); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// IssueRefs returns the set of issue_ref values already persisted in the tasks
// table. The M3 daemon ingests against it: a task whose ref is already known is
// not re-inserted on a later tick (or after a daemon restart), so the durable
// FIFO never gains duplicates — spec §7.1 step 1 (去重) backed by persisted
// state, per principle 4 (recover from disk), not an in-memory set.
func (s *Store) IssueRefs() (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT issue_ref FROM tasks`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]bool)
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return nil, err
		}
		out[ref] = true
	}
	return out, rows.Err()
}

// NextReadyTask returns the head of the dispatch FIFO: the oldest task whose
// status is "new", ordered by created_at (spec §8.7 — FIFO order is by
// ingested time) with the implicit rowid as a deterministic tiebreak for
// same-timestamp inserts. The bool is false when no new task is ready (empty
// queue). This is the daemon's dispatch pick (spec §7.1 step 3): one tick,
// one task, oldest first.
func (s *Store) NextReadyTask() (TaskRow, bool, error) {
	row := s.db.QueryRow(
		`SELECT t.id, t.issue_ref, t.description, t.task_type, t.source, t.acceptance_criteria_json
		 FROM tasks t
		 JOIN task_status ts ON ts.task_id = t.id
		 WHERE ts.status = 'new'
		 ORDER BY t.created_at ASC, t.rowid ASC
		 LIMIT 1`)
	var t TaskRow
	var critJSON string
	if err := row.Scan(&t.ID, &t.IssueRef, &t.Description, &t.TaskType, &t.Source, &critJSON); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return TaskRow{}, false, nil
		}
		return TaskRow{}, false, err
	}
	_ = json.Unmarshal([]byte(critJSON), &t.Criteria)
	return t, true, nil
}

// ParkedTasks returns every task currently parked on tier-3 human review
// (status="needs-review"), oldest first by created_at with rowid tiebreak —
// same FIFO ordering as NextReadyTask. The daemon polls these each tick (spec
// §7.1 step 2) for new human replies on the channel. A parked task keeps its
// active slot freed (spec §7.2c/§10): tier-3 review released it, and this read
// does not re-occupy it — only a reply (resume) or a fresh dispatch does.
//
// Scope: only needs-review (tier-3) is polled here. The other parked states in
// spec §10 (needs-info/needs-human-decision/blocked) wait on different human
// inputs and land with the triage/gate/help-skills issues.
func (s *Store) ParkedTasks() ([]TaskRow, error) {
	rows, err := s.db.Query(
		`SELECT t.id, t.issue_ref, t.description, t.task_type, t.source, t.acceptance_criteria_json
		 FROM tasks t
		 JOIN task_status ts ON ts.task_id = t.id
		 WHERE ts.status = 'needs-review'
		 ORDER BY t.created_at ASC, t.rowid ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TaskRow
	for rows.Next() {
		var t TaskRow
		var critJSON string
		if err := rows.Scan(&t.ID, &t.IssueRef, &t.Description, &t.TaskType, &t.Source, &critJSON); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(critJSON), &t.Criteria)
		out = append(out, t)
	}
	return out, rows.Err()
}

type StepRow struct {
	RunID, Role, Skill, ModelRef string
	Seq                          int
	InputJSON, OutputJSON        string
	TokensIn, TokensOut          int
	Status, Error                string
}

func (s *Store) AppendStep(r StepRow) error {
	id := newID("step")
	_, err := s.db.Exec(
		`INSERT INTO steps(id, run_id, seq, role, skill, model_ref, input_hash, input_json, output_json,
			           tokens_in, tokens_out, status, error, at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, r.RunID, r.Seq, r.Role, r.Skill, r.ModelRef, "", r.InputJSON, r.OutputJSON,
		r.TokensIn, r.TokensOut, r.Status, r.Error, nowISO())
	return err
}

func (s *Store) AppendTransition(taskID, from, to, reason string) error {
	_, err := s.db.Exec(
		`INSERT INTO transitions(id, task_id, from_status, to_status, reason, at)
		 VALUES(?,?,?,?,?,?)`,
		newID("tr"), taskID, from, to, reason, nowISO())
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE task_status SET status=?, updated_at=? WHERE task_id=?`,
		to, nowISO(), taskID)
	return err
}

// RequeueOrphanedRunning resets every task stuck in status="running" back to
// "new" (with a running→new transition recording why) and returns how many it
// reset. The daemon calls this at startup: a task is "running" only while the
// daemon is mid-dispatch, so any "running" row left at startup is an orphan from
// a crashed/killed previous run (spec principle 4 — recover from disk). Without
// this, a daemon killed mid-task leaves that task wedged in "running" forever
// (NextReadyTask only returns "new" tasks, so it would never be re-dispatched).
func (s *Store) RequeueOrphanedRunning() (int, error) {
	rows, err := s.db.Query(`SELECT task_id FROM task_status WHERE status='running'`)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if err := s.AppendTransition(id, "running", "new", "orphaned by daemon restart; re-queued"); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// TransitionRow is one lifecycle transition in the append-only transitions
// trace (spec §10: which task is active / parked / pending-resume is rebuildable
// from task_status + transitions).
type TransitionRow struct {
	From, To, Reason string
}

// Transitions returns the lifecycle trace for one task — every status change in
// the order it happened. The daemon's park/resume logic and its tests read this:
// it is how a resumed task's human feedback (recorded in the resume transition's
// reason) is recovered, even across a daemon restart (principle 4 — recover from
// disk). Ordered by rowid = insertion order = chronological.
func (s *Store) Transitions(taskID string) ([]TransitionRow, error) {
	rows, err := s.db.Query(
		`SELECT from_status, to_status, reason FROM transitions
		 WHERE task_id=? ORDER BY rowid`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TransitionRow
	for rows.Next() {
		var r TransitionRow
		if err := rows.Scan(&r.From, &r.To, &r.Reason); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) Replay(runID string) ([]StepRow, error) {
	rows, err := s.db.Query(
		`SELECT run_id, seq, role, skill, model_ref, input_json, output_json,
		        tokens_in, tokens_out, status, error
		 FROM steps WHERE run_id=? ORDER BY seq`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StepRow
	for rows.Next() {
		var r StepRow
		if err := rows.Scan(&r.RunID, &r.Seq, &r.Role, &r.Skill, &r.ModelRef, &r.InputJSON,
			&r.OutputJSON, &r.TokensIn, &r.TokensOut, &r.Status, &r.Error); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AppendBudget records one budget event into the durable budget_ledger table.
//
// Spec §8.8: every budget check appends a row. SubLoop.Run emits at each of its
// budget-check sites — the retry brake on loop entry (scope=task, kind=retry,
// amount=attempt, limit=MaxRetries) and the per-call token brake before the
// plan/execute model calls (scope=call, kind=tokens, amount=estimate,
// limit=PerCall). Like AppendStep this is an append-only trace: best-effort,
// never gates the loop. The verify-call token check flows through budget.Client
// (Task 14); its durable row awaits the runID plumbing that lands with the M3
// daemon's centralized client wiring.
func (s *Store) AppendBudget(runID, scope, kind string, amount, limit int) error {
	_, err := s.db.Exec(
		`INSERT INTO budget_ledger(id, run_id, scope, kind, amount, limit_val, at)
		 VALUES(?,?,?,?,?,?,?)`,
		newID("bg"), runID, scope, kind, amount, limit, nowISO())
	return err
}

// BudgetRow is one row of the append-only budget_ledger trace.
type BudgetRow struct {
	RunID, Scope, Kind string
	Amount, Limit      int
}

// BudgetLedger returns the budget trace rows for one run, in insertion order.
// Mirrors Replay: a read over budget_ledger used by run reports and by the
// SubLoop budget-ledger wiring test (spec §8.8).
func (s *Store) BudgetLedger(runID string) ([]BudgetRow, error) {
	rows, err := s.db.Query(
		`SELECT run_id, scope, kind, amount, limit_val FROM budget_ledger
		 WHERE run_id=? ORDER BY rowid`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BudgetRow
	for rows.Next() {
		var r BudgetRow
		if err := rows.Scan(&r.RunID, &r.Scope, &r.Kind, &r.Amount, &r.Limit); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

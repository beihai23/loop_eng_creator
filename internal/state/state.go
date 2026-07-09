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

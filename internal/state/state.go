package state

import (
	"database/sql"
	"encoding/json"
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
// M1 DEFERRAL NOTE: this method is defined but NOT yet wired — no M1 caller
// emits budget_ledger rows. The durable budget trace (per-call/per-task token
// accounting persisted alongside the in-memory Enforcer) is deferred to M3:
// the M3 daemon will emit budget rows alongside the centralized client wiring.
// The schema + method ship now so M3 does not need a migration.
func (s *Store) AppendBudget(runID, scope, kind string, amount, limit int) error {
	_, err := s.db.Exec(
		`INSERT INTO budget_ledger(id, run_id, scope, kind, amount, limit_val, at)
		 VALUES(?,?,?,?,?,?,?)`,
		newID("bg"), runID, scope, kind, amount, limit, nowISO())
	return err
}

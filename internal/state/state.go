package state

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }

type TaskRow struct {
	ID, IssueRef, Description, TaskType, Source string
	Criteria                                    []string
	// CreatedAt is the task's issue-submission time (RFC3339), used to order the
	// dispatch FIFO by submission time rather than by ingest time. InsertTask
	// writes it into the existing created_at column; a zero value falls back to
	// the ingest time (nowISO).
	CreatedAt string
	// UpdatedAt is the row's last-write time (RFC3339). Only populated by
	// TaskSpecsByRef — readers that need "did the spec snapshot change" (e.g.
	// the daemon's no-op-write guard in tests) get it there.
	UpdatedAt string
	// Body is the full raw issue text (背景/约束/上下文), preserved alongside
	// the distilled Description+Criteria and fed to the plan/execute prompts.
	Body string
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
	`CREATE TABLE IF NOT EXISTS in_flight(
			task_id TEXT PRIMARY KEY, phase TEXT, updated_at TEXT)`,
	`CREATE INDEX IF NOT EXISTS idx_steps_run ON steps(run_id, seq)`,
	`CREATE INDEX IF NOT EXISTS idx_trans_task ON transitions(task_id, at)`,
	`CREATE TABLE IF NOT EXISTS commands(
			id TEXT PRIMARY KEY,
			task_id TEXT NOT NULL,
			verb TEXT NOT NULL,
			payload TEXT,
			created_at TEXT NOT NULL,
			applied_at TEXT)`,
	`CREATE INDEX IF NOT EXISTS idx_commands_pending ON commands(applied_at)`,
}

// Open opens (creating if absent) the durable state DB at path and migrates its
// schema. The connection is configured for safe multi-reader/single-writer
// concurrency:
//
//   - journal_mode=WAL — readers (the dashboard process, `status --watch`) never
//     block the writer and vice-versa, and a reader sees the latest committed
//     snapshot. This is what makes a long daemon write (or a task being run)
//     transparent to the dashboard's 2s data tick.
//   - busy_timeout=5000ms — on writer contention (two writers, or a checkpoint)
//     SQLite waits up to 5s for the lock instead of erroring "database is
//     locked". The daemon's background-ingest goroutine shares this Store with
//     the synchronous tick; quick single-row writes serialize within that window.
//   - synchronous=NORMAL — the WAL-safe compromise. FULL would fsync every
//     commit (slowest, survives power loss); NORMAL skips the per-commit fsync
//     and only risks corruption on a power loss *mid-commit* (a crash/kill is
//     still safe — WAL is replayed). For an append-only per-task trace store
//     whose lifecycle is re-derivable from the transitions table (principle 4),
//     that risk is acceptable for the throughput gain.
//
// These run as DSN pragmas so the modernc driver applies them on *every* pooled
// connection at open (busy_timeout / synchronous are per-connection; WAL is also
// persistent in the DB header). See modernc.org/sqlite applyQueryParams.
func Open(path string) (*Store, error) {
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	for _, q := range schema {
		if _, err := db.Exec(q); err != nil {
			db.Close()
			return nil, fmt.Errorf("migrate: %w", err)
		}
	}
	// Best-effort migration: add last_comment_at to task_status for the
	// bidirectional sync (reconcile + poll-signals-on-blocked). Ignored if the
	// column already exists in a DB created by an earlier version.
	db.Exec(`ALTER TABLE task_status ADD COLUMN last_comment_at TEXT`)
	// Best-effort: add run_id to verifications for Task 6 的 per-run 分组。
	db.Exec(`ALTER TABLE verifications ADD COLUMN run_id TEXT`)
	// Best-effort: add body to tasks — 全文保留（issue 原文，plan/execute 的上下文）。
	db.Exec(`ALTER TABLE tasks ADD COLUMN body TEXT`)
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) InsertTask(t TaskRow) (string, error) {
	id := newID("task")
	crit, _ := json.Marshal(t.Criteria)
	now := nowISO()
	// created_at is the issue-submission time (drives the FIFO by submission
	// order, not ingest order); fall back to ingest time when the channel did
	// not report one. updated_at is always the ingest time.
	createdAt := t.CreatedAt
	if createdAt == "" {
		createdAt = now
	}
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(
		`INSERT INTO tasks(id, issue_ref, description, task_type, source, acceptance_criteria_json, created_at, updated_at, body)
		 VALUES(?,?,?,?,?,?,?,?,?)`,
		id, t.IssueRef, t.Description, t.TaskType, t.Source, string(crit), createdAt, now, t.Body); err != nil {
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
		`SELECT id, issue_ref, description, task_type, source, acceptance_criteria_json, COALESCE(body,'') FROM tasks WHERE id=?`, id)
	var t TaskRow
	var critJSON string
	if err := row.Scan(&t.ID, &t.IssueRef, &t.Description, &t.TaskType, &t.Source, &critJSON, &t.Body); err != nil {
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
// Read from the dedicated in_flight table (SetInFlight / ClearInFlight write
// it; spec §8.7: cross-process readable, persisted on disk).
type InFlight struct {
	TaskID string
	Phase  string
}

// InFlight returns the currently active sub-loop from the dedicated in_flight
// table. ok is false when the active slot is empty (no row in in_flight).
// This is the read side of the single-active invariant and the `loop-eng
// status --watch` live view (spec §8.7: cross-process observable).
func (s *Store) InFlight() (InFlight, bool, error) {
	var ifl InFlight
	err := s.db.QueryRow(
		`SELECT task_id, phase FROM in_flight LIMIT 1`).Scan(&ifl.TaskID, &ifl.Phase)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return InFlight{}, false, nil
		}
		return InFlight{}, false, err
	}
	return ifl, true, nil
}

// SetInFlight records the currently active sub-loop's task and phase into the
// durable in_flight table. At most one row exists (single-active, spec §12);
// a DELETE-then-INSERT replaces any prior row so the table always reflects the
// latest active task+phase regardless of which task_id was previously set.
// This is the write side called by SubLoop before each phase
// (plan/execute/verify); it is what makes the live --watch view
// cross-process readable (spec §8.7).
func (s *Store) SetInFlight(taskID, phase string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM in_flight`); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO in_flight(task_id, phase, updated_at) VALUES(?,?,?)`,
		taskID, phase, nowISO()); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// ClearInFlight removes the active sub-loop row from the in_flight table. All
// terminal paths in SubLoop.Run (done, blocked, needs-review, error) must call
// this so the in_flight table is empty when the active slot is free — the
// watch view renders "（无活跃任务）" and the next dispatch can SetInFlight
// afresh. Idempotent: calling on an already-empty table is a no-op.
func (s *Store) ClearInFlight() error {
	_, err := s.db.Exec(`DELETE FROM in_flight`)
	return err
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

// TaskSpecsByRef returns every known task's spec snapshot (id + description +
// acceptance criteria) keyed by issue_ref. The daemon's ingest diffs each
// polled channel.Task against this snapshot: issue bodies get edited after
// ingest, and the poll payload already carries the fresh body (ListNewTasks
// fetches full bodies every tick), so a compare-and-update here re-ingests
// edited specs with zero extra channel reads.
func (s *Store) TaskSpecsByRef() (map[string]TaskRow, error) {
	rows, err := s.db.Query(
		`SELECT id, issue_ref, description, acceptance_criteria_json, updated_at, COALESCE(body,'') FROM tasks`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]TaskRow)
	for rows.Next() {
		var t TaskRow
		var critJSON string
		if err := rows.Scan(&t.ID, &t.IssueRef, &t.Description, &critJSON, &t.UpdatedAt, &t.Body); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(critJSON), &t.Criteria)
		out[t.IssueRef] = t
	}
	return out, rows.Err()
}

// UpdateTaskSpec re-ingests an edited issue body: replaces the stored spec
// snapshot (description + acceptance criteria + full body) and bumps
// updated_at. Called by the daemon's ingest only when the polled body actually
// differs from the snapshot — never a no-op write. Runs already dispatched
// keep their snapshot (channel.Task was built at dispatch); the new spec takes
// effect on the next dispatch/resume.
func (s *Store) UpdateTaskSpec(id, description string, criteria []string, body string) error {
	crit, _ := json.Marshal(criteria)
	_, err := s.db.Exec(
		`UPDATE tasks SET description=?, acceptance_criteria_json=?, body=?, updated_at=? WHERE id=?`,
		description, string(crit), body, nowISO(), id)
	return err
}

// NextReadyTask returns the head of the dispatch FIFO: the oldest task whose
// status is "new", ordered by created_at (spec §8.7 — FIFO order is by issue
// submission time, NOT ingest time) with the implicit rowid as a deterministic
// tiebreak for same-timestamp inserts. created_at holds the channel-reported
// submission time (see InsertTask), so a newer issue ingested before an older
// one still dispatches after it. The bool is false when no new task is ready
// (empty queue). This is the daemon's dispatch pick (spec §7.1 step 3): one
// tick, one task, oldest-submitted first.
func (s *Store) NextReadyTask() (TaskRow, bool, error) {
	row := s.db.QueryRow(
		`SELECT t.id, t.issue_ref, t.description, t.task_type, t.source, t.acceptance_criteria_json, COALESCE(t.body,'')
		 FROM tasks t
		 JOIN task_status ts ON ts.task_id = t.id
		 WHERE ts.status = 'new'
		 ORDER BY t.created_at ASC, t.rowid ASC
		 LIMIT 1`)
	var t TaskRow
	var critJSON string
	if err := row.Scan(&t.ID, &t.IssueRef, &t.Description, &t.TaskType, &t.Source, &critJSON, &t.Body); err != nil {
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
	return s.listTasksByStatus("needs-review")
}

// BlockedTasks returns every task whose status is "blocked" (retries exhausted,
// verify rejection, etc.), oldest first. The daemon polls these alongside
// needs-review tasks for new human replies — a reply on a blocked task signals
// "I've addressed the block; retry this task."
func (s *Store) BlockedTasks() ([]TaskRow, error) {
	return s.listTasksByStatus("blocked")
}

// TerminalTasks returns every task whose status is "done" or "blocked" — the
// terminal states that the daemon's reconcile step checks against the channel
// side for human-driven reversals (reopen, un-label).
func (s *Store) TerminalTasks() ([]TaskRow, error) {
	rows, err := s.db.Query(
		`SELECT t.id, t.issue_ref, t.description, t.task_type, t.source, t.acceptance_criteria_json, COALESCE(t.body,'')
		 FROM tasks t
		 JOIN task_status ts ON ts.task_id = t.id
		 WHERE ts.status IN ('done', 'blocked')
		 ORDER BY t.created_at ASC, t.rowid ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTaskRows(rows)
}

func (s *Store) listTasksByStatus(status string) ([]TaskRow, error) {
	rows, err := s.db.Query(
		`SELECT t.id, t.issue_ref, t.description, t.task_type, t.source, t.acceptance_criteria_json, COALESCE(t.body,'')
		 FROM tasks t
		 JOIN task_status ts ON ts.task_id = t.id
		 WHERE ts.status = ?
		 ORDER BY t.created_at ASC, t.rowid ASC`, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTaskRows(rows)
}

func scanTaskRows(rows *sql.Rows) ([]TaskRow, error) {
	var out []TaskRow
	for rows.Next() {
		var t TaskRow
		var critJSON string
		if err := rows.Scan(&t.ID, &t.IssueRef, &t.Description, &t.TaskType, &t.Source, &critJSON, &t.Body); err != nil {
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
	At                           string // 落盘时间（spec §5[3] trace 用）
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
// Also clears in_flight: an orphaned running task's in_flight row (if any) must
// not survive a daemon restart.
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
	// Clear stale in_flight: the orphan's in_flight row (if any) must not survive
	// the restart — the watch view shows no active task until the next dispatch.
	_ = s.ClearInFlight()
	return len(ids), nil
}

// LastCommentAt returns the last time the daemon posted a comment for this
// task (stored in task_status.last_comment_at). A zero time means the daemon
// has not posted any comment yet. The daemon's pollSignals step uses this to
// filter for human replies that arrived after the daemon's last interaction.
func (s *Store) LastCommentAt(taskID string) (time.Time, error) {
	var t sql.NullString
	err := s.db.QueryRow(`SELECT last_comment_at FROM task_status WHERE task_id=?`, taskID).Scan(&t)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return time.Time{}, nil
		}
		return time.Time{}, err
	}
	if !t.Valid || t.String == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, t.String)
}

// SetLastCommentAt records the timestamp of the daemon's most recent comment on
// this task. Called from the SubLoop's report() after a successful PostComment,
// so the daemon's pollSignals step knows which human replies are new.
func (s *Store) SetLastCommentAt(taskID string, t time.Time) error {
	_, err := s.db.Exec(`UPDATE task_status SET last_comment_at=? WHERE task_id=?`,
		t.Format(time.RFC3339Nano), taskID)
	return err
}

// SetResumeFeedback stores the human feedback that triggered a resume (parked →
// new re-queue) in the task_status.parked_detail column, so the SubLoop's next
// Run can read it as the initial priorFailure for the Plan skill's BattleReport.
func (s *Store) SetResumeFeedback(taskID, feedback string) error {
	_, err := s.db.Exec(`UPDATE task_status SET parked_detail=? WHERE task_id=?`, feedback, taskID)
	return err
}

// PopResumeFeedback reads and clears the resume feedback for a task. Returns ""
// if no feedback was set. Called by SubLoop at the start of each Run so the
// human's reply from the previous parked round feeds into the Plan skill.
func (s *Store) PopResumeFeedback(taskID string) (string, error) {
	var fb sql.NullString
	err := s.db.QueryRow(`SELECT parked_detail FROM task_status WHERE task_id=?`, taskID).Scan(&fb)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	s.db.Exec(`UPDATE task_status SET parked_detail='' WHERE task_id=?`, taskID)
	if !fb.Valid {
		return "", nil
	}
	return fb.String, nil
}

// TransitionRow is one lifecycle transition in the append-only transitions
// trace (spec §10: which task is active / parked / pending-resume is rebuildable
// from task_status + transitions).
type TransitionRow struct {
	From, To, Reason string
	At               string // 新增：落盘时间
}

// Transitions returns the lifecycle trace for one task — every status change in
// the order it happened. The daemon's park/resume logic and its tests read this:
// it is how a resumed task's human feedback (recorded in the resume transition's
// reason) is recovered, even across a daemon restart (principle 4 — recover from
// disk). Ordered by rowid = insertion order = chronological.
func (s *Store) Transitions(taskID string) ([]TransitionRow, error) {
	rows, err := s.db.Query(
		`SELECT from_status, to_status, reason, at FROM transitions
		 WHERE task_id=? ORDER BY rowid`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TransitionRow
	for rows.Next() {
		var r TransitionRow
		if err := rows.Scan(&r.From, &r.To, &r.Reason, &r.At); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RunRow is one row of the runs table surfaced to readers (TUI detail/trace).
// RetryCount and TotalTokens are derived by the reader from budget_ledger /
// steps; the runs row itself only carries the run lifecycle.
type RunRow struct {
	ID, TaskID, StartedAt, EndedAt, Outcome string
}

// StartRun opens a new run for a task: inserts a row with started_at=now,
// ended_at=NULL, and returns the new run id. Called by SubLoop at Run entry
// (spec §4.1). A task may have many runs across park/resume.
func (s *Store) StartRun(taskID string) (string, error) {
	id := newID("run")
	_, err := s.db.Exec(
		`INSERT INTO runs(id, task_id, started_at, ended_at, outcome, total_tokens, retry_count)
		 VALUES(?,?,?,NULL,'',0,0)`,
		id, taskID, nowISO())
	if err != nil {
		return "", err
	}
	return id, nil
}

// EndRun closes a run with its terminal outcome (spec §4.1). Idempotent in
// spirit: SubLoop calls it exactly once per run via defer. outcome is one of
// done|blocked|needs-review|cancelled|error.
func (s *Store) EndRun(runID, outcome string) error {
	_, err := s.db.Exec(`UPDATE runs SET ended_at=?, outcome=? WHERE id=?`,
		nowISO(), outcome, runID)
	return err
}

// ActiveRun returns the open run (ended_at IS NULL) for a task — the run a
// running task currently occupies. ok is false when no open run exists.
func (s *Store) ActiveRun(taskID string) (runID, startedAt string, ok bool, err error) {
	err = s.db.QueryRow(
		`SELECT id, started_at FROM runs WHERE task_id=? AND ended_at IS NULL
		 ORDER BY rowid DESC LIMIT 1`, taskID).Scan(&runID, &startedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", false, nil
		}
		return "", "", false, err
	}
	return runID, startedAt, true, nil
}

// RunsOfTask returns every run of a task, oldest first (rowid = insertion
// order). Used by the TUI trace tab to group steps per run (spec §4.1/§5[3]).
func (s *Store) RunsOfTask(taskID string) ([]RunRow, error) {
	rows, err := s.db.Query(
		`SELECT id, task_id, started_at, COALESCE(ended_at,''), COALESCE(outcome,'')
		 FROM runs WHERE task_id=? ORDER BY rowid`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunRow
	for rows.Next() {
		var r RunRow
		if err := rows.Scan(&r.ID, &r.TaskID, &r.StartedAt, &r.EndedAt, &r.Outcome); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// InitialPrompts 返回某 run 的初始提示词：该 run 里首个带 input_json 的 plan step
// 与 execute step 的输入——即任务这次启动时喂给两个模型的首轮提示词（dashboard 详情页
// 的「初始提示词」读它）。无记录时对应返回空串（如 plan 失败未走到 execute，或旧数据
// 的 step 未落 input_json）。
func (s *Store) InitialPrompts(runID string) (plan, execute string, err error) {
	if plan, err = s.firstStepInput(runID, "plan"); err != nil {
		return "", "", err
	}
	if execute, err = s.firstStepInput(runID, "execute"); err != nil {
		return "", "", err
	}
	return plan, execute, nil
}

// firstStepInput 取 run 内某 role 的首个非空 input_json（按 seq 升序 = attempt 顺序，
// rowid 兜底同 seq 时的插入序）。
func (s *Store) firstStepInput(runID, role string) (string, error) {
	var in string
	err := s.db.QueryRow(
		`SELECT input_json FROM steps WHERE run_id=? AND role=? AND input_json != ''
		 ORDER BY seq, rowid LIMIT 1`, runID, role).Scan(&in)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return in, nil
}

func (s *Store) Replay(runID string) ([]StepRow, error) {
	rows, err := s.db.Query(
		`SELECT run_id, seq, role, skill, model_ref, input_json, output_json,
		        tokens_in, tokens_out, status, error, at
		 FROM steps WHERE run_id=? ORDER BY seq`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanStepRows(rows)
}

// scanStepRows 把 steps 表的逐行扫描抽出来，供 Replay（按 seq 排单 run）与
// StepsOfTask（按 at 排跨 run）共用。调用方负责构造查询并关闭 rows。这行
// SELECT 列表与 steps 表的可读列一一对应（与 AppendStep 写入的列同序）。
func scanStepRows(rows *sql.Rows) ([]StepRow, error) {
	var out []StepRow
	for rows.Next() {
		var r StepRow
		if err := rows.Scan(&r.RunID, &r.Seq, &r.Role, &r.Skill, &r.ModelRef, &r.InputJSON,
			&r.OutputJSON, &r.TokensIn, &r.TokensOut, &r.Status, &r.Error, &r.At); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TaskView 是 tasks+task_status join 出的一行，喂给 TUI 概览列表（spec §5[1]）。
// 一条查询喂整张列表（无 N+1）：每个 task 带它当前的 status。Phase B 的 TUI
// reader 直接消费。
type TaskView struct {
	ID, IssueRef, Description, TaskType, Status string
	CreatedAt                                   string
}

// TasksByStatus 返回全部 task 及其当前 status，按 TUI 概览优先级排序：
// new → needs-review → needs-info → blocked → done → cancelled（其余垫底），
// 组内按 issue 号数值升序（CAST(issue_ref AS INTEGER)，列表显示 10,11,13…
// 而非入库序 11,10,14,13）；非数字 ref CAST 得 0，并列时按 created_at 兜底。
// 正在 running 的 task 由 ActiveRun 单独透出，不在此列表里参与 status 分组。
func (s *Store) TasksByStatus() ([]TaskView, error) {
	rows, err := s.db.Query(
		`SELECT t.id, t.issue_ref, t.description, t.task_type, ts.status, t.created_at
		 FROM tasks t JOIN task_status ts ON ts.task_id = t.id
		 ORDER BY CASE ts.status
		     WHEN 'new' THEN 1 WHEN 'needs-review' THEN 2 WHEN 'needs-info' THEN 3
		     WHEN 'blocked' THEN 4 WHEN 'done' THEN 5 WHEN 'cancelled' THEN 6
		     ELSE 7 END, CAST(t.issue_ref AS INTEGER) ASC, t.created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TaskView
	for rows.Next() {
		var v TaskView
		if err := rows.Scan(&v.ID, &v.IssueRef, &v.Description, &v.TaskType, &v.Status, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// StepsOfTask 返回某 task 所有 run 的全部 step，按 steps.at（时间戳）排序——
// 这是跨 run 的轨迹视图（spec §5[3]）。按 at 排而非按 seq 排，是为了规避 Task 2
// 修复过的 per-run seq 冲突（每次 attempt 重置 seq，跨 run 拼接会错序）。
func (s *Store) StepsOfTask(taskID string) ([]StepRow, error) {
	rows, err := s.db.Query(
		`SELECT run_id, seq, role, skill, model_ref, input_json, output_json,
		        tokens_in, tokens_out, status, error, at
		 FROM steps WHERE run_id IN (SELECT id FROM runs WHERE task_id=?)
		 ORDER BY at`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanStepRows(rows)
}

// VerificationRow 是 verifications 表的一行，供 TUI 详情面板的逐 tier 状态展示（spec §4.6）。
type VerificationRow struct {
	Tier   int
	Passed bool
	Detail string
}

// AppendVerification 落盘一个 tier 的 verify 结果（spec §4.6）。SubLoop 在 verify.Chain
// 之后对每个实际跑过的 tier 调一次。step_id 留 NULL（legacy 列）；run_id 列（Open 的 ALTER
// 已加）才是 run 维度的关联键。best-effort：trace，不 gate loop。
func (s *Store) AppendVerification(runID string, tier int, passed bool, detail string) error {
	_, err := s.db.Exec(
		`INSERT INTO verifications(id, step_id, tier, passed, detail, at, run_id)
		 VALUES(?,NULL,?,?,?,?,?)`,
		newID("ver"), tier, passed, detail, nowISO(), runID)
	return err
}

// VerificationsByRun 返回某 run 的逐 tier verify 行，按 tier 升序（spec §4.6）。
func (s *Store) VerificationsByRun(runID string) ([]VerificationRow, error) {
	rows, err := s.db.Query(
		`SELECT tier, passed, detail FROM verifications WHERE run_id=? ORDER BY tier`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VerificationRow
	for rows.Next() {
		var v VerificationRow
		if err := rows.Scan(&v.Tier, &v.Passed, &v.Detail); err != nil {
			return nil, err
		}
		out = append(out, v)
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

// CommandRow is one row of the commands table (the TUI → daemon control
// channel, spec §4.2). AppliedAt is "" while pending.
type CommandRow struct {
	ID, TaskID, Verb, Payload, CreatedAt, AppliedAt string
}

// InsertCommand appends a TUI-issued command (spec §4.2). verb is "resume" or
// "cancel"; payload is the optional resume feedback. The daemon's drainCommands
// step picks up rows where applied_at IS NULL.
func (s *Store) InsertCommand(taskID, verb, payload string) error {
	_, err := s.db.Exec(
		`INSERT INTO commands(id, task_id, verb, payload, created_at, applied_at)
		 VALUES(?,?,?,?,?,NULL)`,
		newID("cmd"), taskID, verb, payload, nowISO())
	return err
}

// PendingCommands returns commands not yet applied (applied_at IS NULL), in
// insertion order. drainCommands drains this each tick (spec §7).
func (s *Store) PendingCommands() ([]CommandRow, error) {
	rows, err := s.db.Query(
		`SELECT id, task_id, verb, COALESCE(payload,''), created_at, COALESCE(applied_at,'')
		 FROM commands WHERE applied_at IS NULL ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CommandRow
	for rows.Next() {
		var c CommandRow
		if err := rows.Scan(&c.ID, &c.TaskID, &c.Verb, &c.Payload, &c.CreatedAt, &c.AppliedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// MarkCommandApplied records that the daemon applied a command (spec §7).
func (s *Store) MarkCommandApplied(cmdID string) error {
	_, err := s.db.Exec(`UPDATE commands SET applied_at=? WHERE id=?`, nowISO(), cmdID)
	return err
}

// CancelRequested reports whether a pending (unapplied) cancel command exists
// for a task. SubLoop self-checks this at phase boundaries so a running task
// stops cooperatively at the next phase (spec §4.5/§7).
func (s *Store) CancelRequested(taskID string) (bool, error) {
	var x int
	err := s.db.QueryRow(
		`SELECT 1 FROM commands WHERE task_id=? AND verb='cancel' AND applied_at IS NULL LIMIT 1`,
		taskID).Scan(&x)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

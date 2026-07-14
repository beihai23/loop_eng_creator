# loop-eng Dashboard 数据层补全 —— Implementation Plan (Phase A)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 补全 loop-eng 看板 TUI 依赖的数据层——`runs`（修潜伏 bug）、`verifications`（逐 tier 落盘）、`commands`（控制通道）、`cancelled` 终态、协作式 cancel——全部 TDD，不碰冻结签名。

**Architecture:** 全部改动落在 `internal/state`（表 + 读写方法）、`internal/loop/subloop.go`（run_id 透传 + 协作 cancel + 逐 tier 落盘）、`internal/daemon/engine.go`（drainCommands）、`internal/verify`（VerifyResult 加字段）。无新包、无新依赖、无 CGO。

**Tech Stack:** Go 1.25, modernc.org/sqlite（纯 Go），表驱动测试，`go test ./...`。

## Global Constraints

- `CGO_ENABLED=0 go build ./...` 必须成功；`go test ./...` 全绿（含现有 M1/M2/e2e）。
- **冻结签名不动**：`channel.Channel`、`model.Client.Call`、`verify.Tier.Check`、`state.Store.AppendStep(StepRow) error`。`verify.Chain` 签名不动（只给 `VerifyResult` struct 加字段）。
- trace 行只追加（`steps`/`transitions`/`verifications`/`budget_ledger`/`commands`/`runs` 的 `ended_at` 更新除外——run 的生命周期本就允许 `ended_at` 从 NULL 写一次）。
- 文档/注释中文；时间戳一律 `nowISO()`（RFC3339Nano）。
- 不引入 anthropic SDK / CGO / 新外部依赖。

**Spec 出处：** `docs/superpowers/specs/2026-07-14-loop-eng-dashboard-tui-design.md` §4.1（runs）、§4.2（commands）、§4.3（cancelled）、§4.5（SubLoop）、§4.6（verifications）。

---

## File Structure

- `internal/state/state.go` —— 新表 `commands`；`verifications` 加 `run_id` 列；新增读写方法（见各任务）。
- `internal/state/state_test.go` —— 新方法的表驱动测试。
- `internal/loop/subloop.go` —— `Run` 改命名返回 + `defer EndRun`；`AppendStep/AppendBudget` 传真 `runID`；verify 后逐 tier `AppendVerification`；每个 phase 前 `CancelRequested` 自查。
- `internal/loop/subloop_test.go` —— 回归测试（两 run 不交错）+ 协作 cancel 测试。
- `internal/verify/verify.go` —— `VerifyResult` 加 `Tiers []TierOutcome`；新增 `TierOutcome` 类型。
- `internal/verify/chain.go` —— `Chain` 求值时填充 `Tiers`。
- `internal/verify/verify_test.go` —— `Chain` 填充 `Tiers` 的测试（含短路）。
- `internal/daemon/engine.go` —— `tick` 加 step 3.5 `drainCommands`；新增 `drainCommands`/`applyCommand`。
- `internal/daemon/engine_test.go` —— drain resume/cancel 测试。

---

## Task 1: `runs` 写入器 + 读取器

**Files:**
- Modify: `internal/state/state.go`（新增方法）
- Test: `internal/state/state_test.go`

**Interfaces:**
- Produces: `Store.StartRun(taskID string) (runID string, err error)`、`Store.EndRun(runID, outcome string) error`、`Store.ActiveRun(taskID string) (runID, startedAt string, ok bool, err error)`、`Store.RunsOfTask(taskID string) ([]RunRow, error)`、`type RunRow`。

**设计说明（重要）：** `EndRun` 只写 `ended_at + outcome`；`retry_count` / `total_tokens` 由读取方从 `budget_ledger` / `steps` 派生（spec §4.1 已允许 total_tokens best-effort）。这避免把 `attempt` 透传到每条终态路径。

- [ ] **Step 1: 写失败测试**

追加到 `internal/state/state_test.go`：

```go
func TestStartEndRun(t *testing.T) {
	st := mustOpenState(t) // 既有 helper：开临时 state.db
	defer st.Close()

	tid, err := st.InsertTask(state.TaskRow{IssueRef: "o/r#1", Description: "d"})
	if err != nil {
		t.Fatal(err)
	}

	rid, err := st.StartRun(tid)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if rid == "" || !strings.HasPrefix(rid, "run_") {
		t.Fatalf("runID want run_ prefix, got %q", rid)
	}

	// ActiveRun 看到未结束的 run
	gotID, _, ok, err := st.ActiveRun(tid)
	if err != nil || !ok || gotID != rid {
		t.Fatalf("ActiveRun = %q %v %v, want %q true nil", gotID, ok, err, rid)
	}

	if err := st.EndRun(rid, "done"); err != nil {
		t.Fatalf("EndRun: %v", err)
	}

	// 结束后 ActiveRun 无活跃 run
	if _, _, ok, err := st.ActiveRun(tid); err != nil || ok {
		t.Fatalf("ActiveRun after EndRun: ok=%v err=%v, want false nil", ok, err)
	}

	runs, err := st.RunsOfTask(tid)
	if err != nil {
		t.Fatalf("RunsOfTask: %v", err)
	}
	if len(runs) != 1 || runs[0].Outcome != "done" || runs[0].EndedAt == "" {
		t.Fatalf("RunsOfTask = %+v, want 1 done run with EndedAt", runs)
	}
}

func TestRunsOfTaskMultipleRuns(t *testing.T) {
	st := mustOpenState(t)
	defer st.Close()
	tid, _ := st.InsertTask(state.TaskRow{IssueRef: "o/r#1", Description: "d"})

	r1, _ := st.StartRun(tid)
	_ = st.EndRun(r1, "needs-review")
	r2, _ := st.StartRun(tid)
	_ = st.EndRun(r2, "done")

	runs, _ := st.RunsOfTask(tid)
	if len(runs) != 2 {
		t.Fatalf("want 2 runs, got %d", len(runs))
	}
}
```

> 若 `mustOpenState` 名不同，沿用 `state_test.go` 里既有的开库 helper（如 `openTestStore`）。`import "strings"` 按需补。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/state/ -run TestStartEndRun -v`
Expected: FAIL（`StartRun` undefined）。

- [ ] **Step 3: 实现**

在 `internal/state/state.go` 加类型与方法（放在 `Replay` 附近）：

```go
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
		 ORDER BY rowid DESC LIMIT 1`).Scan(&runID, &startedAt)
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
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/state/ -run 'TestStartEndRun|TestRunsOfTaskMultipleRuns' -v`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/state/state.go internal/state/state_test.go
git commit -m "feat(state): runs 表写入器/读取器 (StartRun/EndRun/ActiveRun/RunsOfTask)"
```

---

## Task 2: SubLoop 透传真 run_id（修 replay 交错 bug）

**Files:**
- Modify: `internal/loop/subloop.go`（`Run` 签名改命名返回 + defer EndRun；`AppendStep`/`AppendBudget` 的 runID 改真值）
- Test: `internal/loop/subloop_test.go`

**Interfaces:**
- Consumes: `Store.StartRun`、`Store.EndRun`（Task 1）。
- Produces: SubLoop 每次跑写一行 run；`steps`/`budget_ledger` 的 `run_id` 是真 run id，不再等于 taskID。

**背景：** 现状 `AppendStep{RunID: taskID}` + `attempt` 每次 Run 从 1 重计 ⇒ 同任务第二次 run 的 step seq 与第一次撞、且 run_id 都是 taskID ⇒ `Replay` 交错。本任务用 `StartRun` 拿真 runID 透传，并用 `defer EndRun` 保证每条终态路径都关 run。

- [ ] **Step 1: 写回归测试**

追加到 `internal/loop/subloop_test.go`（构造一个会 park 再 resume 两次 run 的场景；若现有 fixture 不好复用，用最简的真 SubLoop 跑两次 Run）：

```go
// TestReplayNoInterleaveAfterResume 钉死 spec §4.1 的 bug：同任务两次 run，
// 各自 attempt=1 的 plan step seq 都是 11，但 run_id 不同 ⇒ Replay(runID)
// 只返回该 run 的 step，不交错。
func TestReplayNoInterleaveAfterResume(t *testing.T) {
	sl, store, task := setupSubLoop(t)        // 既有 helper：装好 plan/exec/verify 的 SubLoop
	taskID, _ := store.InsertTask(task)
	sl.PreinsertedTaskID = taskID

	// run 1
	if _, err := sl.Run(ctx, task); err != nil {
		t.Fatal(err)
	}
	// run 2（模拟 resume 后再次 dispatch）
	if _, err := sl.Run(ctx, task); err != nil {
		t.Fatal(err)
	}

	runs, err := store.RunsOfTask(taskID)
	if err != nil || len(runs) != 2 {
		t.Fatalf("want 2 runs, got %v (%v)", runs, err)
	}

	for _, r := range runs {
		steps, err := store.Replay(r.ID)
		if err != nil {
			t.Fatalf("Replay(%s): %v", r.ID, err)
		}
		// 每个 run 的 plan step（seq=11）至多一条；两次 run 不交错 ⇒ 不应出现两条 seq=11
		var planSeq11 int
		for _, s := range steps {
			if s.Seq == 11 && s.Role == "plan" {
				planSeq11++
			}
		}
		if planSeq11 > 1 {
			t.Fatalf("run %s: got %d plan seq=11 steps (interleaved), want ≤1", r.ID, planSeq11)
		}
	}
}
```

> `setupSubLoop` / `ctx` 沿用 `subloop_test.go` 既有 helper 与变量；若名字不同，照本文件里现有测试的写法对齐。关键是**跑两次 Run**再分 run Replay。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/loop/ -run TestReplayNoInterleaveAfterResume -v`
Expected: FAIL（两次 run 的 step 交错，或 RunsOfTask 行数对但 Replay 串了）。

- [ ] **Step 3: 改 `Run` 为命名返回 + defer EndRun + 透传 runID**

在 `internal/loop/subloop.go`：

**(a)** `Run` 签名改命名返回：
```go
func (sl *SubLoop) Run(ctx context.Context, task channel.Task) (out Outcome, err error) {
```

**(b)** 在 taskID 解析完、`AppendTransition(taskID, "", "running", …)` **之前或之后**插入 StartRun + defer EndRun：
```go
	sl.Store.AppendTransition(taskID, "", "running", "dispatched")

	// 开一行 run（spec §4.1）。defer 保证每条终态路径都关 run，out.Status 即结局。
	runID, rerr := sl.Store.StartRun(taskID)
	if rerr != nil {
		_ = sl.Store.ClearInFlight()
		return Outcome{Status: "error"}, rerr
	}
	defer func() { _ = sl.Store.EndRun(runID, out.Status) }()
```

> 注意：`defer` 引用命名返回 `out`。现有所有 `return sl.report(...), nil` 会自动赋值 `out`；`return Outcome{Status:"error",...}, err` 同理。无需改这些 return。

**(c)** 把 attempt 循环里所有 `RunID: taskID` 改成 `RunID: runID`，所有 `sl.Store.AppendBudget(taskID, …)` 第一个实参改成 `runID`。涉及行（按当前 subloop.go）：
- `AppendBudget(taskID, "task", "retry", ...)` → `AppendBudget(runID, ...)`
- `AppendBudget(taskID, "call", "tokens", ...)`（plan 前、execute 前各一）→ `runID`
- `AppendStep(state.StepRow{RunID: taskID, Seq: attempt*10 + 1, Role: "plan", ...})` → `runID`
- `AppendStep(... RunID: taskID, Seq: attempt*10 + 2, Role: "execute", ...})`（fail / ok 两处）→ `runID`
- `AppendStep(... RunID: taskID, Seq: attempt*10 + 3, Role: "verify", ...})` → `runID`

> `taskID` 仍用于 `AppendTransition` / `SetInFlight` / `report(ctx, taskID, …)` / `PopResumeFeedback` / `CancelRequested`（Task 5）—— 这些按 task 维度，不改。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/loop/ -run TestReplayNoInterleaveAfterResume -v`
Expected: PASS。再跑全量确认无回归：
Run: `go test ./internal/loop/ ./internal/state/`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/loop/subloop.go internal/loop/subloop_test.go
git commit -m "fix(subloop): 透传真 run_id (StartRun/defer EndRun)，修 resume 后 replay 交错"
```

---

## Task 3: `commands` 表 + 控制通道读写

**Files:**
- Modify: `internal/state/state.go`（schema 加 `commands` 表；新增方法）
- Test: `internal/state/state_test.go`

**Interfaces:**
- Produces: `type CommandRow`、`Store.InsertCommand(taskID, verb, payload string) error`、`Store.PendingCommands() ([]CommandRow, error)`、`Store.MarkCommandApplied(cmdID string) error`、`Store.CancelRequested(taskID string) (bool, error)`。

- [ ] **Step 1: 写失败测试**

追加到 `internal/state/state_test.go`：

```go
func TestCommandsInsertPendingApply(t *testing.T) {
	st := mustOpenState(t)
	defer st.Close()
	tid, _ := st.InsertTask(state.TaskRow{IssueRef: "o/r#1", Description: "d"})

	if err := st.InsertCommand(tid, "resume", "fix the thing"); err != nil {
		t.Fatalf("InsertCommand: %v", err)
	}
	if err := st.InsertCommand(tid, "cancel", ""); err != nil {
		t.Fatalf("InsertCommand cancel: %v", err)
	}

	pending, err := st.PendingCommands()
	if err != nil {
		t.Fatalf("PendingCommands: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("want 2 pending, got %d", len(pending))
	}

	// CancelRequested 看到 pending cancel
	if ok, err := st.CancelRequested(tid); err != nil || !ok {
		t.Fatalf("CancelRequested=%v err=%v, want true nil", ok, err)
	}

	// 应用第一条（resume），回写 applied_at
	if err := st.MarkCommandApplied(pending[0].ID); err != nil {
		t.Fatalf("MarkCommandApplied: %v", err)
	}
	// cancel 仍在 pending（第二条）
	rest, _ := st.PendingCommands()
	if len(rest) != 1 || rest[0].Verb != "cancel" {
		t.Fatalf("after apply resume: pending=%+v, want only cancel", rest)
	}
	if err := st.MarkCommandApplied(rest[0].ID); err != nil {
		t.Fatal(err)
	}
	if ok, _ := st.CancelRequested(tid); ok {
		t.Fatalf("CancelRequested after apply, want false")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/state/ -run TestCommandsInsertPendingApply -v`
Expected: FAIL（`InsertCommand` undefined）。

- [ ] **Step 3: 实现 schema + 方法**

在 `state.go` 的 `schema` 切片末尾（`CREATE INDEX … idx_trans_task` 之后）加：

```go
	`CREATE TABLE IF NOT EXISTS commands(
			id TEXT PRIMARY KEY,
			task_id TEXT NOT NULL,
			verb TEXT NOT NULL,
			payload TEXT,
			created_at TEXT NOT NULL,
			applied_at TEXT)`,
	`CREATE INDEX IF NOT EXISTS idx_commands_pending ON commands(applied_at)`,
```

在 `Open` 里现有的 `ALTER TABLE task_status ADD COLUMN last_comment_at` 那行**之后**，加 verifications 挂 run（Task 6 会用到，先一并迁移）：

```go
	db.Exec(`ALTER TABLE verifications ADD COLUMN run_id TEXT`)
```

再加方法（放在 `Commands` 相关位置）：

```go
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
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/state/ -run TestCommandsInsertPendingApply -v`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/state/state.go internal/state/state_test.go
git commit -m "feat(state): commands 表 + 控制通道读写 (InsertCommand/PendingCommands/MarkCommandApplied/CancelRequested)"
```

---

## Task 4: daemon `drainCommands`（resume / cancel）

**Files:**
- Modify: `internal/daemon/engine.go`（`tick` 加 step 3.5；新增 `drainCommands`/`applyCommand`）
- Test: `internal/daemon/engine_test.go`

**Interfaces:**
- Consumes: `Store.PendingCommands`、`Store.MarkCommandApplied`、`Store.AppendTransition`、`Store.SetResumeFeedback`（Task 3 + 既有）。
- Produces: daemon 每 tick 抽干 `commands`，把 resume/cancel 翻成 transition。

**语义（spec §7）：**
- `resume`（needs-review/blocked）→ `X→new` + `SetResumeFeedback(payload)`。
- `cancel`（new/needs-info/needs-review/blocked）→ `X→cancelled`。
- running/done/cancelled → 不动作（running 由 SubLoop 自查处理；其余已终态），只回写 `applied_at`。幂等。

- [ ] **Step 1: 写失败测试**

追加到 `internal/daemon/engine_test.go`（沿用本文件既有的 fake channel + 临时 store 装配 helper）：

```go
func TestDrainCommandsResumeAndCancel(t *testing.T) {
	e, store := setupEngine(t) // 既有 helper：装好 Engine + 临时 store + fake channel + nil RunTask
	t1, _ := store.InsertTask(state.TaskRow{IssueRef: "o/r#1", Description: "d"})
	t2, _ := store.InsertTask(state.TaskRow{IssueRef: "o/r#2", Description: "d"})
	_ = store.AppendTransition(t1, "new", "needs-review", "parked")
	_ = store.AppendTransition(t2, "new", "blocked", "exhausted")

	_ = store.InsertCommand(t1, "resume", "please retry with fix X")
	_ = store.InsertCommand(t2, "cancel", "")

	if err := e.DrainCommandsForTest(); err != nil { // 见 Step 3 的导出说明
		t.Fatalf("drain: %v", err)
	}

	// t1: needs-review → new，反馈落盘
	got1, _ := statusOf(store, t1)
	if got1 != "new" {
		t.Fatalf("t1 status=%q want new", got1)
	}
	if fb, _ := store.PopResumeFeedback(t1); !strings.Contains(fb, "fix X") {
		t.Fatalf("t1 feedback=%q want contains 'fix X'", fb)
	}
	// t2: blocked → cancelled
	got2, _ := statusOf(store, t2)
	if got2 != "cancelled" {
		t.Fatalf("t2 status=%q want cancelled", got2)
	}
	// 全部已应用
	if pend, _ := store.PendingCommands(); len(pend) != 0 {
		t.Fatalf("pending=%d want 0", len(pend))
	}
}

func TestDrainCommandsIdempotent(t *testing.T) {
	e, store := setupEngine(t)
	t1, _ := store.InsertTask(state.TaskRow{IssueRef: "o/r#1", Description: "d"})
	_ = store.AppendTransition(t1, "new", "done", "ran")
	_ = store.InsertCommand(t1, "cancel", "") // 对已 done 的任务 cancel
	if err := e.DrainCommandsForTest(); err != nil {
		t.Fatalf("drain: %v", err)
	}
	got, _ := statusOf(store, t1)
	if got != "done" {
		t.Fatalf("done task flipped to %q", got)
	}
}

// statusOf 读单任务态（测试 helper；若 engine 包已有同名内部函数，改用其导出版或本地的）。
func statusOf(st *state.Store, taskID string) (string, error) {
	rows, _ := st.ListStatuses()
	for _, r := range rows {
		if r.ID == taskID {
			return r.Status, nil
		}
	}
	return "", nil
}
```

> 若 `engine_test.go` 已有同名的包内 `statusOf`，删掉本测试里的局部版，复用既有的。`setupEngine` 沿用既有装配；若名字不同，对齐现有 engine 测试。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/daemon/ -run TestDrainCommands -v`
Expected: FAIL（`DrainCommandsForTest` undefined）。

- [ ] **Step 3: 实现**

在 `internal/daemon/engine.go` 的 `tick()` 里，`pollSignals` 之后、dispatch（`if e.RunTask == nil`）之前插入：

```go
	// ---- step 3.5: drain TUI commands (spec §4.2/§7) ----
	if err := e.drainCommands(ctx); err != nil {
		e.logf("[daemon] drain commands error: %v", err)
	}
```

新增方法：

```go
// drainCommands applies every pending TUI command (resume/cancel) and marks it
// applied (spec §4.2/§7). Idempotent: a command whose target is already
// terminal (or running — handled cooperatively by SubLoop) is just marked
// applied, no transition. Errors halt the drain so the next tick retries from
// the un-applied row.
func (e *Engine) drainCommands(ctx context.Context) error {
	cmds, err := e.Store.PendingCommands()
	if err != nil {
		return err
	}
	for _, c := range cmds {
		if err := e.applyCommand(ctx, c); err != nil {
			return err
		}
		if err := e.Store.MarkCommandApplied(c.ID); err != nil {
			return err
		}
	}
	if len(cmds) > 0 {
		e.logf("[daemon] tick commands: drained %d", len(cmds))
	}
	return nil
}

// DrainCommandsForTest exports drainCommands for engine_test (tick is
// integrated; tests drive the drain step in isolation).
func (e *Engine) DrainCommandsForTest() error { return e.drainCommands(context.Background()) }

// applyCommand translates one TUI command into a transition (spec §7).
func (e *Engine) applyCommand(ctx context.Context, c state.CommandRow) error {
	cur, _ := e.statusOf(c.TaskID)
	switch c.Verb {
	case "resume":
		if cur == "needs-review" || cur == "blocked" {
			if err := e.Store.AppendTransition(c.TaskID, cur, "new", "tui resume: "+c.Payload); err != nil {
				return err
			}
			return e.Store.SetResumeFeedback(c.TaskID, c.Payload)
		}
	case "cancel":
		switch cur {
		case "new", "needs-info", "needs-review", "blocked":
			return e.Store.AppendTransition(c.TaskID, cur, "cancelled", "cancelled by TUI")
		}
		// running → SubLoop 自查处理；done/cancelled → 已终态
	}
	return nil
}
```

> `engine.statusOf` 已存在（`engine.go` 现有方法）。`context` 已 import。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/daemon/ -run TestDrainCommands -v`
Expected: PASS。全量：`go test ./internal/daemon/` PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/daemon/engine.go internal/daemon/engine_test.go
git commit -m "feat(daemon): drainCommands (step 3.5) — resume/cancel 控制通道"
```

---

## Task 5: SubLoop 协作式 cancel

**Files:**
- Modify: `internal/loop/subloop.go`（每个 phase 前加 `CancelRequested` 自查）
- Test: `internal/loop/subloop_test.go`

**Interfaces:**
- Consumes: `Store.CancelRequested`（Task 3）。
- Produces: pending cancel ⇒ SubLoop 在下个 phase 边界 return `cancelled`（defer EndRun 自动记 `cancelled`）。

- [ ] **Step 1: 写失败测试**

追加到 `internal/loop/subloop_test.go`：

```go
// TestCooperativeCancel：在 SubLoop 跑起来后插入一条 pending cancel，断言它
// 在下一个 phase 边界提前以 cancelled 结束（而非继续跑完整轮）。
func TestCooperativeCancel(t *testing.T) {
	sl, store, task := setupSubLoop(t)
	taskID, _ := store.InsertTask(task)
	sl.PreinsertedTaskID = taskID

	// 用一个 hook：plan 第一次跑完就插入 cancel，使 execute 前的自查命中。
	// 若 setupSubLoop 的 plan 是可控的 fake，让它在第一次调用后写 cancel：
	sl.Plan = cancelAfterFirstCall(t, store, taskID, sl.Plan)

	out, err := sl.Run(ctx, task)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Status != "cancelled" {
		t.Fatalf("status=%q want cancelled", out.Status)
	}
	// 终态后 InFlight 清空
	if ifl, ok, _ := store.InFlight(); ok && ifl.TaskID == taskID {
		t.Fatalf("InFlight still set after cancel: %+v", ifl)
	}
	// run 关闭为 cancelled
	runs, _ := store.RunsOfTask(taskID)
	if len(runs) != 1 || runs[0].Outcome != "cancelled" {
		t.Fatalf("runs=%+v want 1 cancelled", runs)
	}
}
```

> `cancelAfterFirstCall` 是个测试 helper：包住原 Plan skill，第一次 Run 后调 `store.InsertCommand(taskID,"cancel","")`，其余透传。按 `subloop_test.go` 里现有的 fake-skill 包装写法实现（典型：实现 `skill.Skill[PlanInput,PlanOutput]` 的薄包装）。若现有 plan fake 本身就能注入副作用，直接用。

一个最小 helper 示例（若需要新建，放 `subloop_test.go`）：

```go
type cancelOncePlan struct {
	inner skill.Skill[skill.PlanInput, skill.PlanOutput]
	store *state.Store
	taskID string
	done  bool
	t      *testing.T
}
func (c *cancelOncePlan) Run(ctx context.Context, in skill.PlanInput) (skill.PlanOutput, skill.Usage, error) {
	out, u, err := c.inner.Run(ctx, in)
	if !c.done {
		c.done = true
		if e := c.store.InsertCommand(c.taskID, "cancel", ""); e != nil {
			c.t.Fatal(e)
		}
	}
	return out, u, err
}
func cancelAfterFirstCall(t *testing.T, store *state.Store, taskID string, inner skill.Skill[skill.PlanInput, skill.PlanOutput]) skill.Skill[skill.PlanInput, skill.PlanOutput] {
	return &cancelOncePlan{inner: inner, store: store, taskID: taskID, t: t}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/loop/ -run TestCooperativeCancel -v`
Expected: FAIL（status 不是 cancelled——SubLoop 还没自查 cancel）。

- [ ] **Step 3: 实现 phase 前自查**

在 `internal/loop/subloop.go` 的 attempt 循环里，**三个 phase 各自的 `sl.logf("… phase=X start")` 之前**插一段（plan/execute/verify 各一处）：

```go
		// 协作式 cancel：phase 边界自查（spec §4.5/§7）。命中则提前以 cancelled 收尾。
		if ok, _ := sl.Store.CancelRequested(taskID); ok {
			sl.logf("[subloop] %s cancelled by TUI at phase boundary", sid)
			_ = sl.Store.ClearInFlight()
			return sl.report(ctx, taskID, task, "cancelled", "cancelled by TUI"), nil
		}
```

具体三个插入点（按当前 subloop.go）：
1. `// ---- plan ----` 之前（循环体开头，`AppendBudget(… "retry" …)` 之前）。
2. `// ---- execute (in a fresh worktree) ----` 之前。
3. `// ---- verify … ----` 之前。

> `defer EndRun`（Task 2）会用 `out.Status="cancelled"` 关 run，所以这里只需 `return sl.report(...)`。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/loop/ -run TestCooperativeCancel -v`
Expected: PASS。全量回归：`go test ./...` PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/loop/subloop.go internal/loop/subloop_test.go
git commit -m "feat(subloop): 协作式 cancel — phase 边界自查 CancelRequested"
```

---

## Task 6: `verifications` 逐 tier 落盘

**Files:**
- Modify: `internal/verify/verify.go`（`VerifyResult` 加 `Tiers`；新增 `TierOutcome`）
- Modify: `internal/verify/chain.go`（`Chain` 填充 `Tiers`）
- Modify: `internal/state/state.go`（`AppendVerification` / `VerificationsByRun`）
- Modify: `internal/loop/subloop.go`（verify 后遍历 `res.Tiers` 落盘）
- Test: `internal/verify/verify_test.go`、`internal/state/state_test.go`、`internal/loop/subloop_test.go`

**Interfaces:**
- Consumes: `verifications.run_id` 列（Task 3 的 ALTER 已加）。
- Produces: `verify.TierOutcome`、`VerifyResult.Tiers`、`Store.AppendVerification(runID, tier int, passed bool, detail string) error`、`Store.VerificationsByRun(runID) ([]VerificationRow, error)`。

- [ ] **Step 1: 写 verify.Chain 填充 Tiers 的失败测试**

追加到 `internal/verify/verify_test.go`：

```go
type stubTier struct { pass bool; detail string }
func (s stubTier) Check(context.Context, string, []string, string) (VerifyResult, error) {
	return VerifyResult{Passed: s.pass, Detail: s.detail}, nil
}

func TestChainFillsTiersAndShortCircuits(t *testing.T) {
	// tier1 过、tier2 挂 ⇒ Tiers 有两条（tier2 失败），tier3 不跑不出现。
	tiers := []Tier{stubTier{pass: true, detail: "t1 ok"}, stubTier{pass: false, detail: "t2 nope"}, stubTier{pass: true, detail: "t3"}}
	res, err := Chain(context.Background(), tiers, "diff", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Passed {
		t.Fatalf("want not passed")
	}
	if len(res.Tiers) != 2 {
		t.Fatalf("want 2 tier outcomes (short-circuit), got %d", len(res.Tiers))
	}
	if res.Tiers[0].Tier != 1 || !res.Tiers[0].Passed || res.Tiers[1].Tier != 2 || res.Tiers[1].Passed {
		t.Fatalf("Tiers=%+v", res.Tiers)
	}
}

func TestChainFillsTiersAllPass(t *testing.T) {
	tiers := []Tier{stubTier{pass: true, detail: "t1"}, stubTier{pass: true, detail: "t2"}}
	res, _ := Chain(context.Background(), tiers, "diff", nil, "")
	if !res.Passed || len(res.Tiers) != 2 {
		t.Fatalf("want passed + 2 tiers, got %+v", res)
	}
}
```

- [ ] **Step 2: 跑确认失败**

Run: `go test ./internal/verify/ -run TestChainFills -v`
Expected: FAIL（`res.Tiers` 不存在）。

- [ ] **Step 3: 改 `VerifyResult` + `Chain`**

`internal/verify/verify.go`：

```go
package verify

import "context"

// TierOutcome is one tier's result within a Chain evaluation, surfaced for
// per-tier persistence (spec §4.6). Tier is 1-based; only evaluated tiers
// appear (Chain short-circuits on first failure, so later tiers are absent).
type TierOutcome struct {
	Tier       int
	Passed     bool
	NeedsHuman bool
	Detail     string
}

type VerifyResult struct {
	Passed          bool
	Detail          string
	FailingCriteria []string
	NeedsHuman      bool
	Tiers           []TierOutcome // 逐 tier 求值结果（spec §4.6）
}

type Tier interface {
	Check(ctx context.Context, diff string, criteria []string, priorFailure string) (VerifyResult, error)
}
```

`internal/verify/chain.go` —— `Chain` 填充 `Tiers`：

```go
func Chain(ctx context.Context, tiers []Tier, diff string, criteria []string, priorFailure string) (VerifyResult, error) {
	var last VerifyResult
	for i, t := range tiers {
		r, err := t.Check(ctx, diff, criteria, priorFailure)
		if err != nil {
			return VerifyResult{}, err
		}
		last = r
		last.Tiers = append(last.Tiers, TierOutcome{
			Tier: i + 1, Passed: r.Passed, NeedsHuman: r.NeedsHuman, Detail: r.Detail,
		})
		if !r.Passed {
			return last, nil // 短路：未到的 tier 不进 Tiers
		}
	}
	return last, nil
}
```

> 注意 `last.Tiers = append(last.Tiers, …)`：每次迭代 `last` 被新 tier 的结果覆盖，但 Tiers 累积在当次返回值里（短路时随 `last` 一起返回；全过时随最后一次 `last` 返回）。由于每个 tier 的 `r.Tiers` 本应为空（stub tier 不填），这里安全。

- [ ] **Step 4: 跑 verify 测试通过**

Run: `go test ./internal/verify/ -run TestChainFills -v`
Expected: PASS。再 `go test ./internal/verify/` 全绿。

- [ ] **Step 5: Store `AppendVerification` / `VerificationsByRun` 失败测试**

追加到 `internal/state/state_test.go`：

```go
func TestAppendVerificationAndRead(t *testing.T) {
	st := mustOpenState(t)
	defer st.Close()
	tid, _ := st.InsertTask(state.TaskRow{IssueRef: "o/r#1", Description: "d"})
	rid, _ := st.StartRun(tid)

	if err := st.AppendVerification(rid, 1, true, "go test: ok"); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendVerification(rid, 2, false, "LLM: diff unrelated"); err != nil {
		t.Fatal(err)
	}

	got, err := st.VerificationsByRun(rid)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Tier != 1 || got[0].Passed != true || got[1].Tier != 2 || got[1].Passed != false {
		t.Fatalf("verifications=%+v", got)
	}
}
```

- [ ] **Step 6: 实现 Store 方法**

`internal/state/state.go`：

```go
// VerificationRow is one row of the verifications table, surfaced for the TUI
// detail panel's per-tier status (spec §4.6).
type VerificationRow struct {
	Tier                  int
	Passed                bool
	Detail                string
}

// AppendVerification records one tier's verify result for a run (spec §4.6).
// Called by SubLoop after verify.Chain, once per evaluated tier. step_id is left
// NULL (legacy); the run_id column (added by ALTER in Open) is the link.
func (s *Store) AppendVerification(runID string, tier int, passed bool, detail string) error {
	_, err := s.db.Exec(
		`INSERT INTO verifications(id, step_id, tier, passed, detail, at, run_id)
		 VALUES(?,NULL,?,?,?,?)`,
		newID("ver"), tier, passed, detail, nowISO(), runID)
	return err
}

// VerificationsByRun returns the per-tier verification rows for one run, in tier
// order (spec §4.6).
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
```

- [ ] **Step 7: 跑 Store 测试通过**

Run: `go test ./internal/state/ -run TestAppendVerificationAndRead -v`
Expected: PASS。

- [ ] **Step 8: SubLoop 落盘逐 tier**

`internal/loop/subloop.go` —— 在 verify `AppendStep(... Role: "verify" …)` **之后**、`if err != nil` 之前，遍历 `res.Tiers` 落盘：

```go
		sl.Store.AppendStep(state.StepRow{
			RunID: runID, Seq: attempt*10 + 3, Role: "verify",
			Status:     statusOf2(res.Passed),
			OutputJSON: string(vt),
		})
		// 逐 tier 落盘 verifications（spec §4.6）
		for _, to := range res.Tiers {
			_ = sl.Store.AppendVerification(runID, to.Tier, to.Passed, to.Detail)
		}
```

> `runID` 来自 Task 2 的透传。`AppendVerification` 失败是 best-effort（trace，不 gate loop），故忽略 err。

- [ ] **Step 9: 跑全量回归**

Run: `go test ./...`
Expected: PASS（含 verify / state / loop / daemon / e2e）。

- [ ] **Step 10: 提交**

```bash
git add internal/verify/verify.go internal/verify/chain.go internal/verify/verify_test.go internal/state/state.go internal/state/state_test.go internal/loop/subloop.go
git commit -m "feat(verify,state,loop): verifications 逐 tier 落盘 (VerifyResult.Tiers + AppendVerification)"
```

---

## Task 7: TUI 读取侧 helper（`TasksByStatus` / `StepsOfTask`）

**Files:**
- Modify: `internal/state/state.go`（两个读取方法）
- Test: `internal/state/state_test.go`

**Interfaces:**
- Produces: `type TaskView`、`Store.TasksByStatus() ([]TaskView, error)`、`Store.StepsOfTask(taskID string) ([]StepRow, error)`。Phase B 的 TUI reader 直接消费。

- [ ] **Step 1: 写失败测试**

追加到 `internal/state/state_test.go`：

```go
func TestTasksByStatusJoinsAndOrders(t *testing.T) {
	st := mustOpenState(t)
	defer st.Close()
	// new、needs-review、done 各一
	a, _ := st.InsertTask(state.TaskRow{IssueRef: "#a", Description: "desc a"})
	b, _ := st.InsertTask(state.TaskRow{IssueRef: "#b", Description: "desc b"})
	c, _ := st.InsertTask(state.TaskRow{IssueRef: "#c", Description: "desc c"})
	_ = st.AppendTransition(b, "new", "needs-review", "park")
	_ = st.AppendTransition(c, "new", "done", "ran")

	got, err := st.TasksByStatus()
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]TaskView{}
	for _, v := range got {
		byID[v.ID] = v
	}
	if byID[a].Status != "new" || byID[a].Description != "desc a" || byID[a].IssueRef != "#a" {
		t.Fatalf("a = %+v", byID[a])
	}
	if byID[b].Status != "needs-review" || byID[c].Status != "done" {
		t.Fatalf("b=%+v c=%+v", byID[b], byID[c])
	}
}

func TestStepsOfTask(t *testing.T) {
	st := mustOpenState(t)
	defer st.Close()
	tid, _ := st.InsertTask(state.TaskRow{IssueRef: "#a", Description: "d"})
	rid, _ := st.StartRun(tid)
	_ = st.AppendStep(state.StepRow{RunID: rid, Seq: 11, Role: "plan", Status: "ok"})
	_ = st.AppendStep(state.StepRow{RunID: rid, Seq: 12, Role: "execute", Status: "ok"})
	got, err := st.StepsOfTask(tid)
	if err != nil || len(got) != 2 {
		t.Fatalf("StepsOfTask=%+v err=%v want 2", got, err)
	}
}
```

- [ ] **Step 2: 跑确认失败**

Run: `go test ./internal/state/ -run 'TestTasksByStatusJoinsAndOrders|TestStepsOfTask' -v`
Expected: FAIL（undefined）。

- [ ] **Step 3: 实现**

`internal/state/state.go`：

```go
// TaskView is a tasks+task_status join row for the TUI overview (spec §5[1]).
// One query feeds the whole list (no N+1).
type TaskView struct {
	ID, IssueRef, Description, TaskType, Status string
	CreatedAt                                   string
}

// TasksByStatus returns every task with its current status, ordered for the TUI
// overview: running(new→running not here; running surfaced via ActiveRun) → new
// → needs-review → needs-info → blocked → done/cancelled. Within a group, oldest
// first by created_at.
func (s *Store) TasksByStatus() ([]TaskView, error) {
	rows, err := s.db.Query(
		`SELECT t.id, t.issue_ref, t.description, t.task_type, ts.status, t.created_at
		 FROM tasks t JOIN task_status ts ON ts.task_id = t.id
		 ORDER BY CASE ts.status
		     WHEN 'new' THEN 1 WHEN 'needs-review' THEN 2 WHEN 'needs-info' THEN 3
		     WHEN 'blocked' THEN 4 WHEN 'done' THEN 5 WHEN 'cancelled' THEN 6
		     ELSE 7 END, t.created_at`)
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

// StepsOfTask returns every step across all of a task's runs, ordered by the
// step timestamp (steps.at) — the cross-run trace view (spec §5[3]). Ordering
// by at avoids the attempt-resets-per-run seq collision (spec §4.1).
func (s *Store) StepsOfTask(taskID string) ([]StepRow, error) {
	rows, err := s.db.Query(
		`SELECT run_id, seq, role, skill, model_ref, input_json, output_json,
		        tokens_in, tokens_out, status, error
		 FROM steps WHERE run_id IN (SELECT id FROM runs WHERE task_id=?)
		 ORDER BY at`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanStepRows(rows)
}
```

> `scanStepRows` 复用 `Replay` 里既有的扫描逻辑——若当前 `Replay` 是内联扫描，把那段抽成 `func scanStepRows(rows *sql.Rows) ([]StepRow, error)` 并让 `Replay` 也调用它（DRY）。这是本任务唯一的小重构。

- [ ] **Step 4: 跑测试通过**

Run: `go test ./internal/state/ -run 'TestTasksByStatusJoinsAndOrders|TestStepsOfTask' -v`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/state/state.go internal/state/state_test.go
git commit -m "feat(state): TUI 读取侧 helper (TasksByStatus/StepsOfTask)"
```

---

## Self-Review（已核对）

**Spec coverage（§4）：** §4.1 runs → Task 1+2；§4.2 commands → Task 3；§4.3 cancelled → Task 4/5（drain + 协作 cancel 都产 cancelled）；§4.4 Store 方法 → Task 1/3/6/7 全覆盖（`ActiveRun`/`RunsOfTask`/`StartRun`/`EndRun`/`InsertCommand`/`PendingCommands`/`MarkCommandApplied`/`CancelRequested`/`AppendVerification`/`VerificationsByRun`/`TasksByStatus`/`StepsOfTask`）；§4.5 SubLoop → Task 2/5；§4.6 verifications → Task 6。✓

**Placeholder：** 无 TBD/TODO；每步有可运行代码或命令。测试里的 `setupSubLoop`/`setupEngine`/`mustOpenState` 明确标注「沿用既有 helper」，并在名字不一致时给了对齐说明——这是引用本仓库既有测试基建，非占位。

**类型一致：** `runID`（Task 1 产出）→ Task 2/5/6 消费，类型 `string` 一致；`CommandRow`（Task 3）→ Task 4 `applyCommand(c state.CommandRow)` 一致；`TierOutcome`/`VerifyResult.Tiers`（Task 6 verify 侧）→ SubLoop `for _, to := range res.Tiers` 字段名 `Tier/Passed/Detail` 一致；`TaskView`/`RunRow`/`VerificationRow` 定义与消费点字段名一致。✓

**冻结签名：** `AppendStep(StepRow) error` 未动（只改传入的 `RunID` 值）；`Tier.Check` 未动；`Chain` 签名未动（只给 struct 加字段）；`channel.Channel` 未触。✓

**已知的 spec 微调（实现时遵循）：** `EndRun(runID, outcome string)`——比 spec 的 `(runID, outcome, retryCount)` 简化，retry_count 由 reader 从 `budget_ledger` 派生；spec §4.1 本就允许 total_tokens best-effort。Plan 2（TUI）会实现该派生。

---

## 全部完成后

- `CGO_ENABLED=0 go build ./...` 成功；`go test ./...` 全绿。
- 数据层就绪：`runs`/`verifications` 不再是空壳、`commands` 控制通道可用、`cancelled` 终态 + 协作 cancel 生效、replay 交错 bug 修复。
- 下一步：**Plan 2 = TUI 本体**（bubbletea + 三 Tab + 颜色/呼吸灯 + reader + commands 写入 + `loop-eng dashboard` 命令），消费本 Plan 产出的全部 Store 方法。

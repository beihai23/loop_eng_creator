# loop-eng M1（核心地基）实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 搭出 loop-eng 的核心地基——一个能把单个任务跑通「分诊 → 计划 → 执行 → 验证（tier1/tier2，tier3 stub）→ 写回」、带可回放 SQLite trace、三道预算刹车、worktree 隔离的同步 loop 引擎，用 hello-world 任务端到端验证。

**Architecture:** Go 单二进制；cobra CLI；`modernc.org/sqlite`（纯 Go，无 CGO）做只追加、可回放的状态库；模型调用走 `model.Client` 接口（triage/plan 直连 anthropic API、execute/verify 走 `claude -p`）；skill 是 `go:embed` 的 markdown 模板 + 强类型 I/O；工单通道是可插拔接口（M1 用本地文件 stub，M2 换 GitHub Issue）。M1 是**同步、单任务**入口（`run-once`），daemon + 异步 tier-3 留给 M3。

**Tech Stack:** Go 1.22+、`modernc.org/sqlite`、`spf13/cobra`、`anthropics/anthropic-sdk-go`、`text/template`、标准 `testing`。

## Global Constraints

- **Go 1.22+**，模块路径 `loop-eng`（本地工具；`go mod init loop-eng`）。
- **纯 Go、单静态二进制**：SQLite 用 `modernc.org/sqlite`（**禁用** CGO 驱动 `mattn/go-sqlite3`）；构建 `CGO_ENABLED=0 go build`。
- **验证独立**：`verify` 包不导入 `loop` 包；验证端只接收 `(diff, criteria)`，永远不接收执行/计划的推理输出。
- **状态只追加**：`steps`/`transitions`/`verifications`/`budget_ledger` 行只 INSERT，禁止 UPDATE/DELETE。
- **三道预算刹车必须存在**：per-call、per-task、max-retries；config 里这三个值缺失/非法 = 加载硬错误。
- **执行端不得自改验收标准**（criteria-mismatch → park 走人；M3 才有真 park，M1 里表现为返回 `needs-info` 终止）。
- 每个任务结束 `go build ./... && go test ./...` 全绿后 commit。

## File Structure

```
loop-eng/
  go.mod
  cmd/loop-eng/main.go              # 入口，挂 cobra root
  internal/
    cli/
      root.go        # cobra root + 全局 flag（--repo, --loop-dir）
      init.go        # loop-eng init
      run_once.go    # loop-eng run-once（M1 同步入口）
      status.go      # loop-eng status
      replay.go      # loop-eng replay
      skill.go       # loop-eng skill test
      config.go      # loop-eng config get
    config/config.go # 配置类型 + Load + Validate
    state/state.go   # SQLite Store：Open/migrate + 增删查 + Replay
    budget/budget.go # Enforcer（三道刹车）
    isolation/wt.go  # worktree 创建/丢弃
    model/model.go   # Client 接口 + Usage + FakeClient
    model/api.go     # apiClient（anthropic SDK）
    model/claude.go  # claudeClient（os/exec claude -p）
    skill/skill.go   # 通用 Skill[I,O] + I/O 类型 + Registry
    skill/render.go  # text/template 渲染
    verify/verify.go # Tier 接口 + VerifyResult
    verify/deterministic.go  # tier1
    verify/llm.go            # tier2
    verify/chain.go          # 串联 + tier3 stub
    channel/channel.go       # Channel 接口 + Task/Reply
    channel/local.go         # localChannel stub（M1）
    loop/subloop.go          # 计划→执行→验证→写回 + 重试
    embed/skills/*.md        # 4 个 skill 的默认 prompt（go:embed）
  test/e2e/helloworld_test.go
```

**核心接口（全计划共用，后续任务不得改签名）：**

```go
// internal/model/model.go
type Usage struct{ TokensIn, TokensOut int }
type Client interface {
    Call(ctx context.Context, prompt string) (output string, usage Usage, err error)
}

// internal/skill/skill.go
type Skill[I any, O any] struct {
    Name, Version, PromptTmpl string
    ParseJSON func([]byte) (O, error)
    Model     model.Client
}

// internal/verify/verify.go
type VerifyResult struct {
    Passed          bool
    Detail          string
    FailingCriteria []string
    NeedsHuman      bool // tier3
}
type Tier interface {
    Check(ctx context.Context, diff string, criteria []string, priorFailure string) (VerifyResult, error)
}

// internal/channel/channel.go
type Task struct {
    Ref                string
    Description        string
    AcceptanceCriteria []string
    TaskType           string
}
type Reply struct{ Body string }
type Channel interface {
    ListNewTasks(ctx context.Context) ([]Task, error)
    ListReplies(ctx context.Context, refs []string) (map[string][]Reply, error)
    PostComment(ctx context.Context, ref, body string) error
    UpdateStatus(ctx context.Context, ref, status string) error
}

// internal/budget/budget.go
type Enforcer struct {
    PerCall, PerTask, MaxRetries int
    SpentTokens                  int
    // contains filtered fields
}

// internal/state/state.go
type TaskRow struct{ ID, IssueRef, Description, TaskType, Source string; Criteria []string }
type StepRow struct {
    RunID, Role, Skill, ModelRef string
    Seq                          int
    InputJSON, OutputJSON        string
    TokensIn, TokensOut          int
    Status, Error                string
}
```

---

## Task 1: 模块骨架 + cobra root 冒烟

**Files:**
- Create: `go.mod`, `cmd/loop-eng/main.go`, `internal/cli/root.go`

**Interfaces:**
- Produces: 可执行的 `loop-eng` 二进制，`--help` 退出码 0。

- [ ] **Step 1: 初始化模块 + 依赖**

```bash
go mod init loop-eng
go get github.com/spf13/cobra@latest
go get modernc.org/sqlite
go get github.com/anthropics/anthropic-sdk-go@latest
```

- [ ] **Step 2: 写 root.go**

```go
// internal/cli/root.go
package cli

import (
	"os"
	"github.com/spf13/cobra"
)

func NewRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "loop-eng",
		Short: "loop engineering 工具",
	}
	return cmd
}

func Execute() {
	if err := NewRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}
```

```go
// cmd/loop-eng/main.go
package main

import "loop-eng/internal/cli"

func main() { cli.Execute() }
```

- [ ] **Step 3: 冒烟测试**

```go
// internal/cli/root_test.go
package cli

import "testing"

func TestRootHelpExitsZero(t *testing.T) {
	cmd := NewRootCmd()
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("--help should not error, got %v", err)
	}
}
```

- [ ] **Step 4: 跑测试**

Run: `go test ./internal/cli/ -run TestRootHelpExitsZero -v`
Expected: PASS

- [ ] **Step 5: 构建并 commit**

```bash
CGO_ENABLED=0 go build -o /tmp/loop-eng ./cmd/loop-eng
/tmp/loop-eng --help
git add -A && git commit -m "feat: 模块骨架 + cobra root"
```

---

## Task 2: config —— 类型 + Load + Validate

**Files:**
- Create: `internal/config/config.go`, `internal/config/config_test.go`

**Interfaces:**
- Produces: `config.Load(path string) (*Config, error)`；`Config` 含 `Models`, `Budget{PerCallTokens, PerTaskTokens, MaxRetries}`, `Verify.Deterministic []struct{Label, Cmd []string}`, `Isolation.Worktree bool`, `Skills.Dir string`。

- [ ] **Step 1: 写失败测试**

```go
// internal/config/config_test.go
package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadValid(t *testing.T) {
	p := writeFile(t, `
models:
  triage: { provider: anthropic, name: claude-haiku-4-5 }
budget:
  per_call_tokens: 20000
  per_task_tokens: 200000
  max_retries: 3
verify:
  deterministic:
    - { label: tests, cmd: ["pytest", "-q"] }
isolation: { worktree: true }
skills: { dir: .loop/skills }
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
	if cfg.Budget.MaxRetries != 3 {
		t.Fatalf("max_retries=%d want 3", cfg.Budget.MaxRetries)
	}
	if cfg.Verify.Deterministic[0].Cmd[0] != "pytest" {
		t.Fatalf("cmd not parsed")
	}
}

func TestLoadRejectsMissingBudget(t *testing.T) {
	p := writeFile(t, `models: { triage: { name: x } }`)
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for missing budget")
	}
}
```

- [ ] **Step 2: 跑测试看它失败**

Run: `go test ./internal/config/ -v`
Expected: FAIL（`Load` 未定义）

- [ ] **Step 3: 实现 config**

```go
// internal/config/config.go
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Models    Models    `yaml:"models"`
	Budget    Budget    `yaml:"budget"`
	Verify    Verify    `yaml:"verify"`
	Isolation Isolation `yaml:"isolation"`
	Skills    Skills    `yaml:"skills"`
}

type Models struct {
	Triage  ModelRef `yaml:"triage"`
	Plan    ModelRef `yaml:"plan"`
	Execute ModelRef `yaml:"execute"`
	Verify  ModelRef `yaml:"verify"`
}
type ModelRef struct {
	Provider string   `yaml:"provider"`
	Name     string   `yaml:"name"`
	Via      string   `yaml:"via"`
	Binary   string   `yaml:"binary"`
	Cmd      []string `yaml:"cmd"`
}

type Budget struct {
	PerCallTokens int `yaml:"per_call_tokens"`
	PerTaskTokens int `yaml:"per_task_tokens"`
	MaxRetries    int `yaml:"max_retries"`
}

type Verify struct {
	Deterministic []struct {
		Label string   `yaml:"label"`
		Cmd   []string `yaml:"cmd"`
	} `yaml:"deterministic"`
	Tier3Human bool `yaml:"tier3_human"`
}

type Isolation struct{ Worktree bool `yaml:"worktree"` }
type Skills struct{ Dir string `yaml:"dir"` }

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) validate() error {
	if c.Budget.PerCallTokens <= 0 || c.Budget.PerTaskTokens <= 0 || c.Budget.MaxRetries <= 0 {
		return fmt.Errorf("budget: per_call_tokens/per_task_tokens/max_retries 必须 > 0")
	}
	if c.Models.Triage.Name == "" {
		return fmt.Errorf("models.triage.name 必填")
	}
	return nil
}
```

加依赖：`go get gopkg.in/yaml.v3`

- [ ] **Step 4: 跑测试看它过**

Run: `go test ./internal/config/ -v`
Expected: PASS（两条）

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat(config): Load + Validate（预算刹车必填）"
```

---

## Task 3: state —— Open/Migrate + 任务增查

**Files:**
- Create: `internal/state/state.go`, `internal/state/state_test.go`

**Interfaces:**
- Produces: `state.Open(path) (*Store, error)`（带 migrate）、`Store.InsertTask(TaskRow) (id string, err error)`、`Store.GetTask(id) (TaskRow, error)`。

- [ ] **Step 1: 写失败测试**

```go
// internal/state/state_test.go
package state

import "testing"

func TestInsertAndGetTask(t *testing.T) {
	s, err := Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	id, err := s.InsertTask(TaskRow{
		IssueRef: "owner/repo#1", Description: "fix login",
		TaskType: "bugfix", Source: "local", Criteria: []string{"login returns 200"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTask(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Description != "fix login" || len(got.Criteria) != 1 {
		t.Fatalf("got %+v", got)
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	dir := t.TempDir() + "/state.db"
	if _, err := Open(dir); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir) // 二次打开 = migrate 不重复建表
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
}
```

- [ ] **Step 2: 跑测试看它失败**

Run: `go test ./internal/state/ -v`
Expected: FAIL（`Open` 未定义）

- [ ] **Step 3: 实现 Store（migrate + tasks 表）**

```go
// internal/state/state.go
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
	Criteria                                   []string
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
	_, err := s.db.Exec(
		`INSERT INTO tasks(id, issue_ref, description, task_type, source, acceptance_criteria_json, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?,?)`,
		id, t.IssueRef, t.Description, t.TaskType, t.Source, string(crit), now, now)
	if err != nil {
		return "", err
	}
	_, err = s.db.Exec(`INSERT INTO task_status(task_id, status, updated_at) VALUES(?,?,?)`,
		id, "new", now)
	return id, err
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
```

辅助 `newID`/`nowISO` 放同包 `util.go`：

```go
// internal/state/util.go
package state

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

func newID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + "_" + hex.EncodeToString(b)
}
func nowISO() string { return time.Now().UTC().Format(time.RFC3339Nano) }
```

> 说明：`time.Now()`/`rand.Read` 在测试里通过固定写入断言顺序，不在此处注入时钟（YAGNI；M3 daemon 才需要可注入时钟）。

- [ ] **Step 4: 跑测试看它过**

Run: `go test ./internal/state/ -run TestInsertAndGetTask -v && go test ./internal/state/ -run TestOpenIsIdempotent -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat(state): SQLite Open/migrate + tasks 增查"
```

---

## Task 4: state —— 追加 step/transition + Replay

**Files:**
- Modify: `internal/state/state.go`
- Test: `internal/state/state_test.go`

**Interfaces:**
- Produces: `Store.AppendStep(StepRow) error`、`Store.AppendTransition(taskID, from, to, reason string) error`、`Store.Replay(runID string) ([]StepRow, error)`。`StepRow` 见 File Structure。

- [ ] **Step 1: 写失败测试**

```go
func TestAppendStepAndReplay(t *testing.T) {
	s, _ := Open(t.TempDir() + "/state.db")
	defer s.Close()
	tid, _ := s.InsertTask(TaskRow{Description: "x", TaskType: "t"})
	runID := newID("run")
	s.AppendTransition(tid, "", "running", "dispatched")
	for i, role := range []string{"triage", "plan", "execute"} {
		s.AppendStep(StepRow{RunID: runID, Seq: i, Role: role, Status: "ok"})
	}
	got, err := s.Replay(runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Role != "triage" || got[2].Role != "execute" {
		t.Fatalf("replay order wrong: %+v", got)
	}
}
```

- [ ] **Step 2: 跑测试看它失败**

Run: `go test ./internal/state/ -run TestAppendStepAndReplay -v`
Expected: FAIL（方法未定义）

- [ ] **Step 3: 实现追加 + Replay**

在 `state.go` 末尾追加：

```go
type StepRow struct {
	RunID, Role, Skill, ModelRef   string
	Seq                            int
	InputJSON, OutputJSON          string
	TokensIn, TokensOut            int
	Status, Error                  string
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

func (s *Store) AppendBudget(runID, scope, kind string, amount, limit int) error {
	_, err := s.db.Exec(
		`INSERT INTO budget_ledger(id, run_id, scope, kind, amount, limit_val, at)
		 VALUES(?,?,?,?,?,?,?)`,
		newID("bg"), runID, scope, kind, amount, limit, nowISO())
	return err
}
```

- [ ] **Step 4: 跑测试看它过**

Run: `go test ./internal/state/ -v`
Expected: PASS（全部）

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat(state): 追加 step/transition/budget + Replay"
```

---

## Task 5: budget —— 三道刹车

**Files:**
- Create: `internal/budget/budget.go`, `internal/budget/budget_test.go`

**Interfaces:**
- Consumes: `model.Usage`。
- Produces: `budget.New(PerCall, PerTask, MaxRetries) *Enforcer`；`Enforcer.BeforeCall(estimate int) error`、`AfterCall(u model.Usage)`、`ShouldRetry(attempt int) bool`。

- [ ] **Step 1: 写失败测试**

```go
// internal/budget/budget_test.go
package budget

import (
	"errors"
	"testing"
	"loop-eng/internal/model"
)

func TestPerCallCap(t *testing.T) {
	e := New(100, 1000, 3)
	if err := e.BeforeCall(50); err != nil { t.Fatal(err) }
	e.AfterCall(model.Usage{TokensIn: 30, TokensOut: 30}) // 60 spent
	if err := e.BeforeCall(50); err == nil {
		t.Fatal("expected per-call cap error on 60+50>100... wait per-call is per single call")
	}
}

func TestPerCallCapSemantics(t *testing.T) {
	e := New(100, 1000, 3)
	// 单次调用估算超过 per_call 即拒
	if err := e.BeforeCall(150); !errors.Is(err, ErrPerCall) {
		t.Fatalf("want ErrPerCall, got %v", err)
	}
}

func TestPerTaskCap(t *testing.T) {
	e := New(1000, 100, 3)
	e.AfterCall(model.Usage{TokensIn: 60, TokensOut: 30}) // 90 spent
	if err := e.BeforeCall(50); !errors.Is(err, ErrPerTask) {
		t.Fatalf("want ErrPerTask (90+50>100), got %v", err)
	}
}

func TestRetry(t *testing.T) {
	e := New(1000, 1000, 3)
	if !e.ShouldRetry(1) || !e.ShouldRetry(3) || e.ShouldRetry(4) {
		t.Fatal("retry boundary wrong")
	}
}
```

- [ ] **Step 2: 跑测试看它失败**

Run: `go test ./internal/budget/ -v`
Expected: FAIL（包未定义）。删掉第一条 `TestPerCallCap`（它语义混乱，留 `TestPerCallCapSemantics` 即可）。

- [ ] **Step 3: 实现 Enforcer**

```go
// internal/budget/budget.go
package budget

import (
	"errors"
	"fmt"

	"loop-eng/internal/model"
)

var (
	ErrPerCall = errors.New("budget: per-call token cap exceeded")
	ErrPerTask = errors.New("budget: per-task token cap exceeded")
)

type Enforcer struct {
	PerCall, PerTask, MaxRetries int
	spent                        int
}

func New(perCall, perTask, maxRetries int) *Enforcer {
	return &Enforcer{PerCall: perCall, PerTask: perTask, MaxRetries: maxRetries}
}

// BeforeCall: estimate 是单次调用的估算 token；超过 PerCall 即拒；累计+estimate 超 PerTask 也拒。
func (e *Enforcer) BeforeCall(estimate int) error {
	if estimate > e.PerCall {
		return fmt.Errorf("%w: estimate=%d per_call=%d", ErrPerCall, estimate, e.PerCall)
	}
	if e.spent+estimate > e.PerTask {
		return fmt.Errorf("%w: spent=%d estimate=%d per_task=%d", ErrPerTask, e.spent, estimate, e.PerTask)
	}
	return nil
}

func (e *Enforcer) AfterCall(u model.Usage) {
	e.spent += u.TokensIn + u.TokensOut
}

// ShouldRetry: attempt 是当前第几次执行（1 起）；attempt <= MaxRetries 才重试。
func (e *Enforcer) ShouldRetry(attempt int) bool {
	return attempt <= e.MaxRetries
}
```

> 删掉测试里第一条 `TestPerCallCap`（保留三条：PerCallCapSemantics / PerTaskCap / Retry）。

- [ ] **Step 4: 跑测试看它过**

Run: `go test ./internal/budget/ -v`
Expected: PASS（三条）

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat(budget): 三道预算刹车 Enforcer"
```

---

## Task 6: isolation —— worktree 创建/丢弃

**Files:**
- Create: `internal/isolation/wt.go`, `internal/isolation/wt_test.go`

**Interfaces:**
- Produces: `isolation.Create(baseRepo, runID string) (worktreePath string, err error)`、`Discard(worktreePath string) error`。

- [ ] **Step 1: 写失败测试**

```go
// internal/isolation/wt_test.go
package isolation

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, c := range [][]string{
		{"git", "init", "-q"},
		{"git", "config", "user.email", "t@t"},
		{"git", "config", "user.name", "t"},
	} {
		if out, err := exec.Command(c[0], append(c[1:], dir)...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	mustWrite(t, filepath.Join(dir, "README"), "hi")
	for _, c := range [][]string{
		{"git", "-C", dir, "add", "-A"},
		{"git", "-C", dir, "commit", "-q", "-m", "init"},
	} {
		if out, err := exec.Command(c[0], c[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	return dir
}

func mustWrite(t *testing.T, p, body string) {
	if err := writeFile(p, body); err != nil {
		t.Fatal(err)
	}
}

func TestCreateAndDiscard(t *testing.T) {
	repo := initRepo(t)
	wt, err := Create(repo, "run-abc")
	if err != nil {
		t.Fatal(err)
	}
	if err := Discard(repo, wt); err != nil {
		t.Fatal(err)
	}
}
```

- [ ] **Step 2: 跑测试看它失败**

Run: `go test ./internal/isolation/ -v`
Expected: FAIL（包未定义）

- [ ] **Step 3: 实现 worktree**

```go
// internal/isolation/wt.go
package isolation

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func writeFile(path, body string) error {
	return os.WriteFile(path, []byte(body), 0644)
}

// Create 在 baseRepo 旁的 .loop/worktrees/<runID> 建 worktree，返回其绝对路径。
func Create(baseRepo, runID string) (string, error) {
	wtRoot := filepath.Join(baseRepo, ".loop", "worktrees")
	if err := os.MkdirAll(wtRoot, 0755); err != nil {
		return "", err
	}
	wtPath := filepath.Join(wtRoot, runID)
	branch := "loop/" + runID
	out, err := exec.Command("git", "-C", baseRepo,
		"worktree", "add", "-b", branch, wtPath, "HEAD").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git worktree add: %s", out)
	}
	return wtPath, nil
}

// Discard 删 worktree 及其分支。
func Discard(baseRepo, wtPath string) error {
	out, err := exec.Command("git", "-C", baseRepo, "worktree", "remove", "--force", wtPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git worktree remove: %s", out)
	}
	branch := "loop/" + filepath.Base(wtPath)
	out, err = exec.Command("git", "-C", baseRepo, "branch", "-D", branch).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git branch -D: %s", out)
	}
	return nil
}
```

- [ ] **Step 4: 跑测试看它过**

Run: `go test ./internal/isolation/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat(isolation): worktree 创建/丢弃"
```

---

## Task 7: model —— Client 接口 + FakeClient + 两个真实实现

**Files:**
- Create: `internal/model/model.go`, `internal/model/fake.go`, `internal/model/api.go`, `internal/model/claude.go`, `internal/model/model_test.go`

**Interfaces:**
- Produces: `model.Client` 接口（见 File Structure）；`FakeClient`（测试用，按 prompt 前缀返回预设）；`APIClient`（anthropic SDK，按 `ModelRef`）；`ClaudeClient`（`os/exec claude -p`）。

- [ ] **Step 1: 写失败测试（FakeClient + 接口契约）**

```go
// internal/model/model_test.go
package model

import (
	"context"
	"testing"
)

func TestFakeClientByPrefix(t *testing.T) {
	f := NewFake(map[string]string{
		"triage:": `{"startable":true,"loop_doable":true}`,
		"plan:":   `{"plan":[],"risks":[]}`,
	})
	out, usage, err := f.Call(context.Background(), "triage: 修复登录")
	if err != nil {
		t.Fatal(err)
	}
	if out != `{"startable":true,"loop_doable":true}` {
		t.Fatalf("got %q", out)
	}
	if usage.TokensIn == 0 {
		t.Fatal("usage should be nonzero")
	}
}

func TestFakeClientUnknownPrefixErrors(t *testing.T) {
	f := NewFake(map[string]string{"x:": "y"})
	if _, _, err := f.Call(context.Background(), "unknown"); err == nil {
		t.Fatal("want error for unknown prefix")
	}
}
```

- [ ] **Step 2: 跑测试看它失败**

Run: `go test ./internal/model/ -v`
Expected: FAIL（未定义）

- [ ] **Step 3: 实现接口 + Fake**

```go
// internal/model/model.go
package model

import "context"

type Usage struct{ TokensIn, TokensOut int }

type Client interface {
	Call(ctx context.Context, prompt string) (output string, usage Usage, err error)
}
```

```go
// internal/model/fake.go
package model

import (
	"context"
	"errors"
	"strings"
)

type FakeClient struct {
	byPrefix map[string]string
	calls    int
}

func NewFake(byPrefix map[string]string) *FakeClient {
	return &FakeClient{byPrefix: byPrefix}
}

func (f *FakeClient) Call(_ context.Context, prompt string) (string, Usage, error) {
	f.calls++
	for prefix, out := range f.byPrefix {
		if strings.HasPrefix(prompt, prefix) {
			return out, Usage{TokensIn: len(prompt), TokensOut: len(out)}, nil
		}
	}
	return "", Usage{}, errors.New("fake: no matching prefix for prompt")
}
```

- [ ] **Step 4: 实现两个真实 client（仅构建请求/命令，不在此联调）**

```go
// internal/model/api.go
package model

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
)

type APIClient struct {
	Name  string
	Key   string // ANTHROPIC_API_KEY
	inner *anthropic.Client
}

func NewAPIClient(name, key string) *APIClient {
	c := anthropic.NewClient()
	return &APIClient{Name: name, Key: key, inner: &c}
}

func (a *APIClient) Call(ctx context.Context, prompt string) (string, Usage, error) {
	resp, err := a.inner.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.F(a.Name),
		MaxTokens: anthropic.F(int64(4096)),
		Messages: anthropic.F([]anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
		}),
	})
	if err != nil {
		return "", Usage{}, err
	}
	out := ""
	for _, b := range resp.Content {
		if t, ok := b.AsAny().(anthropic.TextBlock); ok {
			out += t.Text
		}
	}
	return out, Usage{TokensIn: int(resp.Usage.InputTokens), TokensOut: int(resp.Usage.OutputTokens)}, nil
}

func init() { _ = json.Marshal } // 保留 encoding/json 以备将来解析
var _ = fmt.Sprint
```

```go
// internal/model/claude.go
package model

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// ClaudeClient 每次调用 = 全新 `claude -p` 会话（验证独立性要求）。
type ClaudeClient struct {
	Binary string
	Args   []string // 额外固定 flag
}

func NewClaudeClient(binary string, extraArgs []string) *ClaudeClient {
	return &ClaudeClient{Binary: binary, Args: extraArgs}
}

func (c *ClaudeClient) Call(ctx context.Context, prompt string) (string, Usage, error) {
	args := append([]string{"-p"}, c.Args...)
	cmd := exec.CommandContext(ctx, c.Binary, args...)
	cmd.Stdin = strings.NewReader(prompt)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return "", Usage{}, fmt.Errorf("claude -p: %w: %s", err, errBuf.String())
	}
	// claude -p 不稳定回报 token；用输出长度近似（真实值在战报里标注为估算）
	return out.String(), Usage{TokensOut: out.Len()}, nil
}
```

> 真实 client 不写单测（依赖网络/二进制）；契约由接口保证，联调留到手动 smoke。

- [ ] **Step 5: 跑测试 + 构建 + commit**

```bash
go test ./internal/model/ -v   # 仅 Fake 测试通过即可
CGO_ENABLED=0 go build ./...
git add -A && git commit -m "feat(model): Client 接口 + Fake + APIClient + ClaudeClient"
```

> 若 anthropic SDK 的具体字段名随版本变化导致编译失败，按 `go doc` 修正字段，勿留 TODO。

---

## Task 8: skill —— 通用 Skill[I,O] + I/O 类型 + 渲染 + Registry

**Files:**
- Create: `internal/skill/skill.go`, `internal/skill/render.go`, `internal/skill/registry.go`, `internal/skill/skill_test.go`
- Create: `internal/embed/skills/triage.md`（占位模板，Task 18 填初稿）

**Interfaces:**
- Consumes: `model.Client`。
- Produces: 强类型 `Skill[I,O]`；`Run(ctx, input I) (O, model.Usage, error)`；四个 I/O 类型（`TriageInput/Output`、`PlanInput/Output`、`VerifyInput/Output`、`HelpInput/Output`）；`Registry`（name → version + 默认模板路径）。

- [ ] **Step 1: 写失败测试**

```go
// internal/skill/skill_test.go
package skill

import (
	"context"
	"encoding/json"
	"testing"

	"loop-eng/internal/model"
)

func TestSkillRunParsesOutput(t *testing.T) {
	fake := model.NewFake(map[string]string{
		"TRIAGE:": `{"startable":true,"loop_doable":true,"suggested_type":"bugfix","difficulty":"low","reason":"ok"}`,
	})
	s := Skill[TriageInput, TriageOutput]{
		Name: "triage", Version: "1", PromptTmpl: "TRIAGE: {{.TaskDescription}}",
		ParseJSON: func(b []byte) (TriageOutput, error) {
			var o TriageOutput
			return o, json.Unmarshal(b, &o)
		},
		Model: fake,
	}
	out, _, err := s.Run(context.Background(), TriageInput{TaskDescription: "fix login"})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Startable || out.SuggestedType != "bugfix" {
		t.Fatalf("parsed wrong: %+v", out)
	}
}

func TestSkillRunParseError(t *testing.T) {
	fake := model.NewFake(map[string]string{"X:": "not-json"})
	s := Skill[struct{}, struct{}]{
		Name: "x", PromptTmpl: "X:",
		ParseJSON: func(b []byte) (struct{}, error) {
			return struct{}{}, json.Unmarshal(b, &struct{}{}) // 故意失败
		},
		Model: fake,
	}
	if _, _, err := s.Run(context.Background(), struct{}{}); err == nil {
		t.Fatal("want parse error")
	}
}
```

- [ ] **Step 2: 跑测试看它失败**

Run: `go test ./internal/skill/ -v`
Expected: FAIL（未定义）

- [ ] **Step 3: 实现 Skill + 渲染 + I/O 类型**

```go
// internal/skill/render.go
package skill

import (
	"bytes"
	"text/template"
)

func render(tmpl string, input any) (string, error) {
	t, err := template.New("s").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, input); err != nil {
		return "", err
	}
	return buf.String(), nil
}
```

```go
// internal/skill/skill.go
package skill

import (
	"context"
	"fmt"

	"loop-eng/internal/model"
)

type Skill[I any, O any] struct {
	Name, Version, PromptTmpl string
	ParseJSON                 func([]byte) (O, error)
	Model                     model.Client
}

func (s Skill[I, O]) Run(ctx context.Context, input I) (O, model.Usage, error) {
	var zero O
	prompt, err := render(s.PromptTmpl, input)
	if err != nil {
		return zero, model.Usage{}, fmt.Errorf("render skill %s: %w", s.Name, err)
	}
	out, usage, err := s.Model.Call(ctx, prompt)
	if err != nil {
		return zero, usage, err
	}
	parsed, err := s.ParseJSON([]byte(out))
	if err != nil {
		return zero, usage, fmt.Errorf("parse skill %s output: %w (raw=%q)", s.Name, err, out)
	}
	return parsed, usage, nil
}

// ---- I/O 类型（对应 spec §8.5）----

type TriageInput struct {
	TaskDescription     string
	AcceptanceCriteria  []string
	TaskType            string
}
type TriageOutput struct {
	Startable          bool     `json:"startable"`
	MissingInfo        []string `json:"missing_info"`
	LoopDoable         bool     `json:"loop_doable"`
	SuggestedType      string   `json:"suggested_type"`
	Difficulty         string   `json:"difficulty"`
	NeedsHumanDecision bool     `json:"needs_human_decision"`
	Reason             string   `json:"reason"`
}

type PlanInput struct {
	Task               string
	AcceptanceCriteria []string
	BattleReport       string
	RepoStateSummary   string
}
type PlanStep struct {
	Step     string   `json:"step"`
	Files    []string `json:"files"`
	Expected string   `json:"expected"`
}
type PlanOutput struct {
	Plan  []PlanStep `json:"plan"`
	Risks []string   `json:"risks"`
}

type VerifyInput struct {
	Diff                string
	AcceptanceCriteria  []string
	PriorFailureSignal  string
}
type VerifyOutput struct {
	Passed          bool     `json:"passed"`
	Reason          string   `json:"reason"`
	FailingCriteria []string `json:"failing_criteria"`
}

type HelpInput struct {
	Task            string
	BlockedState    string
	AttemptsSummary string
	LastError       string
}
type HelpOutput struct {
	HelpRequest struct {
		StuckAt       string   `json:"stuck_at"`
		Tried         []string `json:"tried"`
		NeedFromHuman string   `json:"need_from_human"`
	} `json:"help_request"`
}
```

```go
// internal/skill/registry.go
package skill

// Registry 仅用于 CLI skill edit/test：name → 版本 + 默认模板来源。
// 强类型调用在 subloop 里直接用 Skill[I,O]，不经 Registry。
type Entry struct {
	Name, Version, DefaultEmbedPath string
}

var Defaults = []Entry{
	{Name: "triage", Version: "1", DefaultEmbedPath: "internal/embed/skills/triage.md"},
	{Name: "plan", Version: "1", DefaultEmbedPath: "internal/embed/skills/plan.md"},
	{Name: "verify", Version: "1", DefaultEmbedPath: "internal/embed/skills/verify.md"},
	{Name: "help", Version: "1", DefaultEmbedPath: "internal/embed/skills/help.md"},
}
```

```go
// internal/embed/skills/triage.md
TRIAGE: 任务={{.TaskDescription}} 类型={{.TaskType}} 验收标准={{.AcceptanceCriteria}}
（Task 18 填真实初稿；此处为占位以让 embed 编译。）
```

- [ ] **Step 4: 跑测试看它过**

Run: `go test ./internal/skill/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat(skill): Skill[I,O] + I/O 类型 + 渲染 + Registry"
```

---

## Task 9: verify —— tier1 确定性脚本

**Files:**
- Create: `internal/verify/verify.go`, `internal/verify/deterministic.go`, `internal/verify/verify_test.go`

**Interfaces:**
- Produces: `VerifyResult`、`Tier` 接口；`Deterministic{Label string; Cmd []string}` 实现 `Tier.Check`：在指定 cwd 跑 Cmd，退出码 0 = 过。

- [ ] **Step 1: 写失败测试**

```go
// internal/verify/verify_test.go
package verify

import (
	"context"
	"testing"
)

func TestDeterministicPass(t *testing.T) {
	d := Deterministic{Label: "true", Cmd: []string{"true"}}
	r, err := d.Check(context.Background(), "", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if !r.Passed {
		t.Fatal("true should pass")
	}
}

func TestDeterministicFail(t *testing.T) {
	d := Deterministic{Label: "false", Cmd: []string{"false"}}
	r, _ := d.Check(context.Background(), "", nil, "")
	if r.Passed {
		t.Fatal("false should fail")
	}
	if r.Detail == "" {
		t.Fatal("want detail on fail")
	}
}
```

- [ ] **Step 2: 跑测试看它失败**

Run: `go test ./internal/verify/ -v`
Expected: FAIL

- [ ] **Step 3: 实现 tier1**

```go
// internal/verify/verify.go
package verify

import "context"

type VerifyResult struct {
	Passed          bool
	Detail          string
	FailingCriteria []string
	NeedsHuman      bool
}

type Tier interface {
	Check(ctx context.Context, diff string, criteria []string, priorFailure string) (VerifyResult, error)
}
```

```go
// internal/verify/deterministic.go
package verify

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
)

type Deterministic struct {
	Label string
	Cmd   []string
	Dir   string // worktree 路径
}

func (d Deterministic) Check(ctx context.Context, _ string, _ []string, _ string) (VerifyResult, error) {
	cmd := exec.CommandContext(ctx, d.Cmd[0], d.Cmd[1:]...)
	if d.Dir != "" {
		cmd.Dir = d.Dir
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if err == nil {
		return VerifyResult{Passed: true, Detail: d.Label + ": ok"}, nil
	}
	return VerifyResult{Passed: false, Detail: fmt.Sprintf("%s: %s", d.Label, out.String())}, nil
	// 注意：脚本报错（非 0）按失败处理，绝不静默通过（spec §8.6）。
}
```

- [ ] **Step 4: 跑测试看它过**

Run: `go test ./internal/verify/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat(verify): tier1 确定性脚本"
```

---

## Task 10: verify —— tier2 LLM 新鲜上下文 + chain

**Files:**
- Create: `internal/verify/llm.go`, `internal/verify/chain.go`
- Test: `internal/verify/verify_test.go`

**Interfaces:**
- Consumes: `skill.Skill[VerifyInput, VerifyOutput]`（新鲜会话由 `model.Client` 每次新调用保证）。
- Produces: `LLM` 实现 `Tier`；`Chain(ctx, tiers []Tier, diff, criteria, prior) (VerifyResult, error)`：按序跑，tier1/2 不过即短路，全过则 `Passed=true`；`HumanStub` 返回 `NeedsHuman=true`（M1 占位）。

- [ ] **Step 1: 写失败测试**

```go
func TestChainShortCircuitsOnTier1Fail(t *testing.T) {
	fail := Deterministic{Label: "tests", Cmd: []string{"false"}}
	called := false
	t2 := tierSpy{called: &called}
	res, _ := Chain(context.Background(), []Tier{fail, t2}, "", nil, "")
	if res.Passed {
		t.Fatal("should fail")
	}
	if called {
		t.Fatal("tier2 must not run when tier1 fails")
	}
}

func TestChainPassesWhenAllPass(t *testing.T) {
	ok := Deterministic{Label: "tests", Cmd: []string{"true"}}
	human := HumanStub{}
	res, _ := Chain(context.Background(), []Tier{ok, human}, "", nil, "")
	if !res.Passed {
		t.Fatal("ok+tier3-stub should pass (stub doesn't block in M1 chain)")
	}
}

type tierSpy struct{ called *bool }

func (s tierSpy) Check(context.Context, string, []string, string) (VerifyResult, error) {
	*s.called = true
	return VerifyResult{Passed: true}, nil
}
```

- [ ] **Step 2: 跑测试看它失败**

Run: `go test ./internal/verify/ -v`
Expected: FAIL（Chain 未定义）

- [ ] **Step 3: 实现 LLM + Chain + HumanStub**

```go
// internal/verify/llm.go
package verify

import (
	"context"
	"sync"

	"loop-eng/internal/skill"
)

// LLM 用一个 verify skill（其 Model 每次新会话）做语义验证。
type LLM struct {
	Skill skill.Skill[skill.VerifyInput, skill.VerifyOutput]
}

func (l LLM) Check(ctx context.Context, diff string, criteria []string, priorFailure string) (VerifyResult, error) {
	out, _, err := l.Skill.Run(ctx, skill.VerifyInput{
		Diff:               diff,
		AcceptanceCriteria: criteria,
		PriorFailureSignal: priorFailure,
	})
	if err != nil {
		return VerifyResult{}, err
	}
	return VerifyResult{
		Passed:          out.Passed,
		Detail:          out.Reason,
		FailingCriteria: out.FailingCriteria,
	}, nil
}

// 保证 LLM 不持有执行态：每次 Check 走 skill.Model.Call（新会话）。
var _ = sync.Mutex{}
```

```go
// internal/verify/chain.go
package verify

import "context"

// HumanStub: M1 占位 tier3。Check 返回 NeedsHuman=true，但在 Chain 里不阻断（M3 才真 park）。
type HumanStub struct{}

func (HumanStub) Check(context.Context, string, []string, string) (VerifyResult, error) {
	return VerifyResult{Passed: true, NeedsHuman: true, Detail: "tier3 stub (M3 接 issue 评论)"}, nil
}

// Chain 按 tier1→tier2→tier3 顺序跑；前一层不过即短路返回。
// 注意：M1 里 tier3 是 HumanStub（Passed=true），故 chain 结果取决于 tier1/tier2。
func Chain(ctx context.Context, tiers []Tier, diff string, criteria []string, priorFailure string) (VerifyResult, error) {
	var last VerifyResult
	for _, t := range tiers {
		r, err := t.Check(ctx, diff, criteria, priorFailure)
		if err != nil {
			return VerifyResult{}, err
		}
		last = r
		if !r.Passed {
			return r, nil // 短路
		}
	}
	return last, nil
}
```

- [ ] **Step 4: 跑测试看它过**

Run: `go test ./internal/verify/ -v`
Expected: PASS（全部）

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat(verify): tier2 LLM + Chain + tier3 stub"
```

---

## Task 11: channel —— 接口 + localChannel stub

**Files:**
- Create: `internal/channel/channel.go`, `internal/channel/local.go`, `internal/channel/channel_test.go`

**Interfaces:**
- Produces: `Channel` 接口、`Task`、`Reply`；`Local` 实现：从 `inbox/*.md` 读任务、`outbox/<ref>.md` 追加评论。

- [ ] **Step 1: 写失败测试**

```go
// internal/channel/channel_test.go
package channel

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLocalListAndComment(t *testing.T) {
	dir := t.TempDir()
	inbox := filepath.Join(dir, "inbox")
	os.MkdirAll(inbox, 0755)
	os.WriteFile(filepath.Join(inbox, "1.md"), []byte("# 任务\nfix login\ntype: bugfix\n## 验收标准\n- [ ] login 200"), 0644)

	c := NewLocal(dir)
	tasks, err := c.ListNewTasks(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].Description != "fix login" {
		t.Fatalf("got %+v", tasks)
	}
	if err := c.PostComment(nil, tasks[0].Ref, "战报 round1: done"); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "outbox", tasks[0].Ref+".md"))
	if string(body) == "" {
		t.Fatal("comment not written")
	}
}
```

- [ ] **Step 2: 跑测试看它失败**

Run: `go test ./internal/channel/ -v`
Expected: FAIL

- [ ] **Step 3: 实现 Local**

```go
// internal/channel/channel.go
package channel

import "context"

type Task struct {
	Ref                string
	Description        string
	AcceptanceCriteria []string
	TaskType           string
}

type Reply struct{ Body string }

type Channel interface {
	ListNewTasks(ctx context.Context) ([]Task, error)
	ListReplies(ctx context.Context, refs []string) (map[string][]Reply, error)
	PostComment(ctx context.Context, ref, body string) error
	UpdateStatus(ctx context.Context, ref, status string) error
}
```

```go
// internal/channel/local.go
package channel

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

type Local struct{ Root string }

func NewLocal(root string) *Local { return &Local{Root: root} }

func (l *Local) ListNewTasks(_ context.Context) ([]Task, error) {
	inbox := filepath.Join(l.Root, "inbox")
	ents, err := os.ReadDir(inbox)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var tasks []Task
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(inbox, e.Name()))
		if err != nil {
			continue
		}
		t := parseLocalTask(string(raw))
		t.Ref = strings.TrimSuffix(e.Name(), ".md")
		tasks = append(tasks, t)
	}
	return tasks, nil
}

func parseLocalTask(raw string) Task {
	var t Task
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "type:"):
			t.TaskType = strings.TrimSpace(strings.TrimPrefix(line, "type:"))
		case strings.HasPrefix(line, "- [ ]"):
			t.AcceptanceCriteria = append(t.AcceptanceCriteria, strings.TrimSpace(strings.TrimPrefix(line, "- [ ]")))
		case line != "" && !strings.HasPrefix(line, "#") && t.Description == "":
			t.Description = line
		}
	}
	return t
}

func (l *Local) ListReplies(_ context.Context, _ []string) (map[string][]Reply, error) {
	return nil, nil // M1 不需要回复（无 daemon）；M2/M3 实现
}

func (l *Local) PostComment(_ context.Context, ref, body string) error {
	dir := filepath.Join(l.Root, "outbox")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, ref+".md"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(body + "\n")
	return err
}

func (l *Local) UpdateStatus(_ context.Context, ref, status string) error {
	dir := filepath.Join(l.Root, "status")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, ref), []byte(status), 0644)
}
```

- [ ] **Step 4: 跑测试看它过**

Run: `go test ./internal/channel/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat(channel): Channel 接口 + Local stub"
```

---

## Task 12: loop/subloop —— 计划→执行→验证→写回 + 重试

**Files:**
- Create: `internal/loop/subloop.go`, `internal/loop/subloop_test.go`

**Interfaces:**
- Consumes: `state.Store`, `budget.Enforcer`, `model.Client`（执行）、四个 skill、`verify` tiers、`isolation`、`channel.Channel`、`config`。
- Produces: `SubLoop.Run(ctx, task channel.Task) (Outcome, error)`；`Outcome{Status, Detail}` ∈ done/blocked/needs-info/needs-review。每轮：plan→execute(worktree)→verify(chain)→writeback(state+channel)；tier1/2 不过→反馈→重试（≤max_retries）；3 次失败→blocked+help。

- [ ] **Step 1: 写失败测试（用 FakeClient + 内存 state + 真隔离 worktree）**

```go
// internal/loop/subloop_test.go
package loop

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"testing"

	"loop-eng/internal/budget"
	"loop-eng/internal/channel"
	"loop-eng/internal/isolation"
	"loop-eng/internal/model"
	"loop-eng/internal/skill"
	"loop-eng/internal/state"
	"loop-eng/internal/verify"
)

func initRepo(t *testing.T) string {
	dir := t.TempDir()
	for _, c := range [][]string{
		{"git", "init", "-q", dir},
		{"git", "-C", dir, "config", "user.email", "t@t"},
		{"git", "-C", dir, "config", "user.name", "t"},
	} {
		out, err := exec.Command(c[0], c[1:]...).CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0644) //nolint
	exec.Command("git", "-C", dir, "add", "-A").Run()
	exec.Command("git", "-C", dir, "commit", "-q", "-m", "i").Run()
	return dir
}

func TestSubLoopDoneOnFirstPass(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()

	// 三个 skill 全成功；execute 产空 diff
	tri := model.NewFake(map[string]string{"TRIAGE:": mustJSON(skill.TriageOutput{Startable: true, LoopDoable: true})})
	// 单个 fake 给 plan/verify/execute 用前缀区分
	fake := model.NewFake(map[string]string{
		"PLAN:":    mustJSON(skill.PlanOutput{}),
		"EXECUTE:": "ok",
		"VERIFY:":  mustJSON(skill.VerifyOutput{Passed: true}),
	})

	sl := &SubLoop{
		Repo: repo, Store: st, Budget: budget.New(100000, 1000000, 3),
		Execute: fake,
		Plan:    mkSkill[skill.PlanInput, skill.PlanOutput]("PLAN:", fake),
		Verify:  verify.LLM{Skill: mkSkill[skill.VerifyInput, skill.VerifyOutput]("VERIFY:", fake)},
		Tiers:   []verify.Tier{verify.LLM{Skill: mkSkill[skill.VerifyInput, skill.VerifyOutput]("VERIFY:", fake)}, verify.HumanStub{}},
		Channel: channel.NewLocal(t.TempDir()),
	}
	_ = tri // triage 在 M1 的 SubLoop 外（CLI 层先分诊），这里跳过

	out, err := sl.Run(context.Background(), channel.Task{Ref: "1", Description: "d", AcceptanceCriteria: []string{"c"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "done" {
		t.Fatalf("want done, got %s (%s)", out.Status, out.Detail)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func mkSkill[I any, O any](prefix string, m model.Client) skill.Skill[I, O] {
	return skill.Skill[I, O]{
		Name: "x", PromptTmpl: prefix,
		ParseJSON: func(b []byte) (O, error) {
			var o O
			return o, json.Unmarshal(b, &o)
		},
		Model: m,
	}
}
```

> 测试里用 `os` 包：在文件头加 `"os"` import。

- [ ] **Step 2: 跑测试看它失败**

Run: `go test ./internal/loop/ -v`
Expected: FAIL（SubLoop 未定义）

- [ ] **Step 3: 实现 SubLoop**

```go
// internal/loop/subloop.go
package loop

import (
	"context"
	"fmt"

	"loop-eng/internal/budget"
	"loop-eng/internal/channel"
	"loop-eng/internal/isolation"
	"loop-eng/internal/model"
	"loop-eng/internal/skill"
	"loop-eng/internal/state"
	"loop-eng/internal/verify"
)

type Outcome struct {
	Status string // done | blocked | needs-info | needs-review
	Detail string
}

type SubLoop struct {
	Repo    string
	Store   *state.Store
	Budget  *budget.Enforcer
	Execute model.Client
	Plan    skill.Skill[skill.PlanInput, skill.PlanOutput]
	Verify  verify.LLM   // 备用（chain 里也用）
	Tiers   []verify.Tier // 完整链：tier1[], tier2, tier3
	Channel channel.Channel
}

func (sl *SubLoop) Run(ctx context.Context, task channel.Task) (Outcome, error) {
	taskID, err := sl.Store.InsertTask(state.TaskRow{
		IssueRef: task.Ref, Description: task.Description,
		TaskType: task.TaskType, Source: "local", Criteria: task.AcceptanceCriteria,
	})
	if err != nil {
		return Outcome{Status: "error"}, err
	}
	sl.Store.AppendTransition(taskID, "", "running", "dispatched")

	priorFailure := ""
	for attempt := 1; sl.Budget.ShouldRetry(attempt); attempt++ {
		// plan
		if err := sl.Budget.BeforeCall(1000); err != nil {
			return sl.blocked(ctx, taskID, task, "budget: "+err.Error())
		}
		planOut, u, err := sl.Plan.Run(ctx, skill.PlanInput{
			Task: task.Description, AcceptanceCriteria: task.AcceptanceCriteria,
			BattleReport: priorFailure,
		})
		sl.Budget.AfterCall(u)
		sl.Store.AppendStep(state.StepRow{RunID: taskID, Seq: attempt*10 + 1, Role: "plan", Status: statusOf(err), Error: errStr(err)})
		if err != nil {
			priorFailure = "plan error: " + err.Error()
			continue
		}

		// execute (in worktree)
		wt, err := isolation.Create(sl.Repo, taskID+"-r"+fmt.Sprint(attempt))
		if err != nil {
			return Outcome{Status: "error", Detail: err.Error()}, err
		}
		execOut, u2, err := sl.Execute.Call(ctx, "EXECUTE: "+task.Description+" @ "+wt)
		sl.Budget.AfterCall(u2)
		_ = execOut
		if err != nil {
			isolation.Discard(sl.Repo, wt)
			priorFailure = "execute error: " + err.Error()
			sl.Store.AppendStep(state.StepRow{RunID: taskID, Seq: attempt*10 + 2, Role: "execute", Status: "fail", Error: err.Error()})
			continue
		}
		diff := worktreeDiff(sl.Repo, wt)
		sl.Store.AppendStep(state.StepRow{RunID: taskID, Seq: attempt*10 + 2, Role: "execute", Status: "ok", OutputJSON: diff})

		// verify (chain, fresh context per LLM call)
		res, err := verify.Chain(ctx, sl.Tiers, diff, task.AcceptanceCriteria, priorFailure)
		sl.Store.AppendStep(state.StepRow{RunID: taskID, Seq: attempt*10 + 3, Role: "verify", Status: statusOf2(res.Passed), OutputJSON: res.Detail})
		if err != nil {
			isolation.Discard(sl.Repo, wt)
			return Outcome{Status: "error", Detail: err.Error()}, err
		}
		if res.NeedsHuman {
			sl.Store.AppendTransition(taskID, "running", "needs-review", "tier3")
			return Outcome{Status: "needs-review", Detail: res.Detail}, nil
		}
		if res.Passed {
			sl.Store.AppendTransition(taskID, "running", "done", "verified")
			return Outcome{Status: "done", Detail: res.Detail}, nil
		}
		// 不过 → 反馈，下一轮
		priorFailure = res.Detail
		isolation.Discard(sl.Repo, wt)
	}
	return sl.blocked(ctx, taskID, task, "retries exhausted: "+priorFailure)
}

func (sl *SubLoop) blocked(ctx context.Context, taskID string, task channel.Task, reason string) (Outcome, error) {
	sl.Store.AppendTransition(taskID, "running", "blocked", reason)
	sl.Channel.PostComment(ctx, task.Ref, "BLOCKED: "+reason)
	return Outcome{Status: "blocked", Detail: reason}, nil
}

func worktreeDiff(repo, wt string) string {
	out, err := execGit(repo, "diff", "--no-color", "HEAD", "--", wt)
	if err != nil {
		return ""
	}
	return out
}
```

辅助 `execGit`/`statusOf` 放 `internal/loop/git.go`：

```go
// internal/loop/git.go
package loop

import (
	"os/exec"
	"strings"
)

func execGit(repo string, args ...string) (string, error) {
	full := append([]string{"-C", repo}, args...)
	out, err := exec.Command("git", full...).CombinedOutput()
	return string(out), err
}

func statusOf(err error) string {
	if err != nil {
		return "fail"
	}
	return "ok"
}
func statusOf2(passed bool) string {
	if passed {
		return "ok"
	}
	return "fail"
}
func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
var _ = strings.TrimSpace
```

- [ ] **Step 4: 跑测试看它过**

Run: `go test ./internal/loop/ -v`
Expected: PASS（done 路径）

- [ ] **Step 5: 加一个 blocked 路径测试 + commit**

```go
func TestSubLoopBlockedAfterRetries(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()
	fake := model.NewFake(map[string]string{
		"PLAN:":    mustJSON(skill.PlanOutput{}),
		"EXECUTE:": "ok",
		"VERIFY:":  mustJSON(skill.VerifyOutput{Passed: false, Reason: "nope"}), // 永远不过
	})
	sl := &SubLoop{
		Repo: repo, Store: st, Budget: budget.New(100000, 1000000, 2),
		Execute: fake,
		Plan:    mkSkill[skill.PlanInput, skill.PlanOutput]("PLAN:", fake),
		Tiers:   []verify.Tier{verify.LLM{Skill: mkSkill[skill.VerifyInput, skill.VerifyOutput]("VERIFY:", fake)}, verify.HumanStub{}},
		Channel: channel.NewLocal(t.TempDir()),
	}
	out, _ := sl.Run(context.Background(), channel.Task{Ref: "2", Description: "d"})
	if out.Status != "blocked" {
		t.Fatalf("want blocked, got %s", out.Status)
	}
}
```

Run: `go test ./internal/loop/ -v` → PASS；然后：

```bash
git add -A && git commit -m "feat(loop): SubLoop 计划→执行→验证→写回 + 重试"
```

---

## Task 13: CLI —— init

**Files:**
- Create: `internal/cli/init.go`, `internal/embed/skills/{plan,verify,help}.md`（占位）

**Interfaces:**
- Consumes: `config` 默认模板、`skill.Defaults`（拷默认 skill）。
- Produces: `loop-eng init [--repo .]`：在当前仓库写 `.loop/config.yaml`、`.loop/skills/*.md`（从 embed 拷）、`.loop/state.db`（Open 建）、`.loop/worktrees/`。

- [ ] **Step 1: 写失败测试**

```go
// internal/cli/init_test.go
package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInitScaffoldsLoopDir(t *testing.T) {
	repo := t.TempDir()
	// 需要 git 仓库（worktree 基址）
	initGitRepo(t, repo)

	cmd := NewRootCmd()
	cmd.SetArgs([]string{"init", "--repo", repo})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{".loop/config.yaml", ".loop/state.db", ".loop/skills/triage.md"} {
		if _, err := os.Stat(filepath.Join(repo, p)); err != nil {
			t.Fatalf("missing %s: %v", p, err)
		}
	}
}
```

> `initGitRepo(t, repo)` 复用 isolation 测试的套路（git init + commit）；放到 `internal/cli/testhelper_test.go`。

- [ ] **Step 2: 跑测试看它失败**

Run: `go test ./internal/cli/ -run TestInit -v`
Expected: FAIL

- [ ] **Step 3: 实现 init**

```go
// internal/cli/init.go
package cli

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"loop-eng/internal/state"
)

//go:embed embed/skills/*.md
var skillFiles embed.FS

var defaultConfig = `
models:
  triage:  { via: claude-p, binary: claude }
  plan:    { via: claude-p, binary: claude }
  execute: { via: claude-p, binary: claude }
  verify:  { via: claude-p, binary: claude }
budget:
  per_call_tokens: 20000
  per_task_tokens: 200000
  max_retries: 3
verify:
  deterministic: []
  tier3_human: true
isolation: { worktree: true }
skills: { dir: .loop/skills }
`

func NewInitCmd() *cobra.Command {
	var repo string
	cmd := &cobra.Command{
		Use:   "init",
		Short: "在当前仓库生成 .loop/（配置 + skill + state.db）",
		RunE: func(cmd *cobra.Command, args []string) error {
			if repo == "" {
				repo, _ = os.Getwd()
			}
			loopDir := filepath.Join(repo, ".loop")
			for _, sub := range []string{"skills", "worktrees"} {
				if err := os.MkdirAll(filepath.Join(loopDir, sub), 0755); err != nil {
					return err
				}
			}
			if err := os.WriteFile(filepath.Join(loopDir, "config.yaml"), []byte(defaultConfig), 0644); err != nil {
				return err
			}
			entries, _ := skillFiles.ReadDir("embed/skills")
			for _, e := range entries {
				raw, _ := skillFiles.ReadFile("embed/skills/" + e.Name())
				os.WriteFile(filepath.Join(loopDir, "skills", e.Name()), raw, 0644)
			}
			st, err := state.Open(filepath.Join(loopDir, "state.db"))
			if err != nil {
				return err
			}
			st.Close()
			fmt.Println("loop-eng initialized at", loopDir)
			return nil
		},
	}
	cmd.Flags().StringVar(&repo, "repo", "", "目标仓库路径（默认当前目录）")
	return cmd
}
```

在 `root.go` 的 `NewRootCmd` 里 `cmd.AddCommand(NewInitCmd())`。

> `//go:embed` 的路径相对于 `.go` 文件所在目录，故 `embed/skills/*.md` 要存在于 `internal/cli/` 下。把 `internal/embed/skills/*.md` 的内容也复制一份到 `internal/cli/embed/skills/`（或把 embed 包独立）。简化：Task 8 创建的 `internal/embed/skills/` 改为 CLI 直接 embed 的位置 `internal/cli/embed/skills/`，并更新 skill.Defaults 的路径注释。**实现时统一为 `internal/cli/embed/skills/`。**

- [ ] **Step 4: 跑测试看它过**

Run: `go test ./internal/cli/ -run TestInit -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat(cli): loop-eng init 脚手架"
```

---

## Task 14: CLI —— run-once（M1 同步入口）

**Files:**
- Create: `internal/cli/run_once.go`, `internal/cli/run_once_test.go`

**Interfaces:**
- Consumes: `config.Load`、`state.Open`、组装 `model.Client`（按 config，M1 测试用 Fake；真实用 APIClient/ClaudeClient）、四个 skill（默认模板 + ParseJSON）、`verify` 链、`budget`、`SubLoop`、`channel.NewLocal`。
- Produces: `loop-eng run-once --repo . --task-inbox <dir>`：从 inbox 捞第一个任务，跑 SubLoop，打印 Outcome。

- [ ] **Step 1: 写失败测试（注入 Fake 模型）**

```go
// internal/cli/run_once_test.go
package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRunOnceEndToEnd(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	// 先 init
	NewRootCmd().SetArgs([]string{"init", "--repo", repo}).Execute()

	// 准备 inbox 任务
	inbox := filepath.Join(repo, "inbox")
	os.MkdirAll(inbox, 0755)
	os.WriteFile(filepath.Join(inbox, "1.md"), []byte("# 任务\ndo thing\ntype: bugfix\n## 验收标准\n- [ ] c"), 0644)

	// run-once（用 --models=fake 注入 Fake，便于 CI）
	cmd := NewRootCmd()
	cmd.SetArgs([]string{"run-once", "--repo", repo, "--task-inbox", inbox, "--models", "fake"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	// 战报应写到 outbox
	if _, err := os.Stat(filepath.Join(repo, "outbox", "1.md")); err != nil {
		t.Fatalf("no battle report: %v", err)
	}
}
```

- [ ] **Step 2: 跑测试看它失败**

Run: `go test ./internal/cli/ -run TestRunOnce -v`
Expected: FAIL

- [ ] **Step 3: 实现 run-once + 一个组装函数 buildEngine**

```go
// internal/cli/run_once.go
package cli

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
	"loop-eng/internal/budget"
	"loop-eng/internal/channel"
	"loop-eng/internal/loop"
	"loop-eng/internal/model"
	"loop-eng/internal/skill"
	"loop-eng/internal/state"
	"loop-eng/internal/verify"
)

func NewRunOnceCmd() *cobra.Command {
	var repo, inbox, models string
	cmd := &cobra.Command{
		Use:   "run-once",
		Short: "M1 同步入口：从 inbox 捞一个任务跑完整 loop",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := mustLoad(repo)
			st := mustOpenState(repo)
			defer st.Close()

			exec, plan, verifySkill, triage := buildModels(cfg, models)
			_ = triage

			tiers := buildTiers(cfg, repo, verifySkill)
			bz := budget.New(cfg.Budget.PerCallTokens, cfg.Budget.PerTaskTokens, cfg.Budget.MaxRetries)
			ch := channel.NewLocal(repo)

			tasks, err := ch.ListNewTasks(context.Background())
			if err != nil || len(tasks) == 0 {
				return fmt.Errorf("no task in inbox %s", inbox)
			}
			sl := &loop.SubLoop{
				Repo: repo, Store: st, Budget: bz,
				Execute: exec,
				Plan:    plan,
				Tiers:   tiers,
				Channel: ch,
			}
			out, err := sl.Run(context.Background(), tasks[0])
			fmt.Printf("outcome: %s — %s\n", out.Status, out.Detail)
			return err
		},
	}
	cmd.Flags().StringVar(&repo, "repo", ".", "仓库路径")
	cmd.Flags().StringVar(&inbox, "task-inbox", "", "任务 inbox 目录")
	cmd.Flags().StringVar(&models, "models", "real", "real | fake（测试用）")
	return cmd
}

func buildModels(_ any, mode string) (exec model.Client, plan skill.Skill[skill.PlanInput, skill.PlanOutput], vs skill.Skill[skill.VerifyInput, skill.VerifyOutput], triage skill.Skill[skill.TriageInput, skill.TriageOutput]) {
	var m model.Client
	if mode == "fake" {
		m = model.NewFake(map[string]string{
			"TRIAGE:": jsonStr(skill.TriageOutput{Startable: true, LoopDoable: true}),
			"PLAN:":   jsonStr(skill.PlanOutput{}),
			"EXECUTE:": "ok",
			"VERIFY:":  jsonStr(skill.VerifyOutput{Passed: true}),
		})
	} else {
		m = model.NewClaudeClient("claude", nil) // M1：执行/验证都走 claude -p；plan 也用 claude 简化
	}
	plan = skill.Skill[skill.PlanInput, skill.PlanOutput]{Name: "plan", PromptTmpl: "PLAN: {{.Task}}", ParseJSON: parseJSON[skill.PlanOutput], Model: m}
	vs = skill.Skill[skill.VerifyInput, skill.VerifyOutput]{Name: "verify", PromptTmpl: "VERIFY: {{.Diff}}", ParseJSON: parseJSON[skill.VerifyOutput], Model: m}
	triage = skill.Skill[skill.TriageInput, skill.TriageOutput]{Name: "triage", PromptTmpl: "TRIAGE: {{.TaskDescription}}", ParseJSON: parseJSON[skill.TriageOutput], Model: m}
	exec = m
	return
}

func buildTiers(_ any, _ string, vs skill.Skill[skill.VerifyInput, skill.VerifyOutput]) []verify.Tier {
	return []verify.Tier{verify.LLM{Skill: vs}, verify.HumanStub{}}
	// 确定性脚本（tier1）从 cfg.Verify.Deterministic 动态构造；这里 M1 简化：仅 LLM + human stub。
}

func parseJSON[O any](b []byte) (O, error) {
	var o O
	return o, json.Unmarshal(b, &o)
}
func jsonStr(v any) string { b, _ := json.Marshal(v); return string(b) }

func mustLoad(repo string) *configPtr { /* 读 .loop/config.yaml */ panic("见 step 4") }
func mustOpenState(repo string) *state.Store { /* state.Open(.loop/state.db) */ panic("见 step 4") }
```

**Step 4（填实 mustLoad / mustOpenState，去掉 panic）：**

```go
// 放 run_once.go 顶部，替换上面的占位
type configPtr = config.Config // 别名

func mustLoad(repo string) *config.Config {
	c, err := config.Load(filepath.Join(repo, ".loop", "config.yaml"))
	if err != nil { panic(err) }
	return c
}
func mustOpenState(repo string) *state.Store {
	st, err := state.Open(filepath.Join(repo, ".loop", "state.db"))
	if err != nil { panic(err) }
	return st
}
```

加 imports `path/filepath`、`loop-eng/internal/config`。把 `NewRunOnceCmd` 注册到 `NewRootCmd`。

> 注意：`run-once` 用 panic 处理加载失败是 M1 简化；生产路径在 M3 daemon 里改返回 error + 写评论。`run-once` 是开发/测试入口。

- [ ] **Step 4（实际跑）：** 删掉 step 3 里的 `panic` 版 `mustLoad/mustOpenState`，用 step 4 的真实版本；

Run: `go test ./internal/cli/ -run TestRunOnce -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat(cli): run-once 同步入口（组装 engine + SubLoop）"
```

---

## Task 15: CLI —— status / replay / skill test

**Files:**
- Create: `internal/cli/status.go`, `internal/cli/replay.go`, `internal/cli/skill.go`

**Interfaces:**
- Consumes: `state.Store`。
- Produces：`loop-eng status --repo . [--task <id>]` 打印任务态；`loop-eng replay --repo . --run <id>` 按 seq 打印 steps；`loop-eng skill test` 打印 skill.Defaults 列表（M1 最小实现，真实回归集在 M3+）。

- [ ] **Step 1: 写失败测试（status）**

```go
// internal/cli/status_test.go
package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestStatusPrintsTask(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	NewRootCmd().SetArgs([]string{"init", "--repo", repo}).Execute()
	// 手插一条任务
	insertFakeTask(t, repo)

	var buf bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"status", "--repo", repo})
	cmd.Execute()
	if !strings.Contains(buf.String(), "running") && !strings.Contains(buf.String(), "new") && !strings.Contains(buf.String(), "task_") {
		t.Fatalf("status output empty: %q", buf.String())
	}
}
```

> `insertFakeTask` 直接用 `state.Open` + `InsertTask`。

- [ ] **Step 2: 跑失败**

Run: `go test ./internal/cli/ -run TestStatus -v`
Expected: FAIL

- [ ] **Step 3: 实现 status/replay/skill**

```go
// internal/cli/status.go
package cli

import (
	"fmt"
	"github.com/spf13/cobra"
	"loop-eng/internal/state"
)

func NewStatusCmd() *cobra.Command {
	var repo, task string
	cmd := &cobra.Command{
		Use: "status",
		RunE: func(_ *cobra.Command, _ []string) error {
			st := mustOpenState(repo)
			defer st.Close()
			if task != "" {
				t, err := st.GetTask(task)
				if err != nil { return err }
				fmt.Printf("%s %s %s\n", t.ID, t.TaskType, t.Description)
				return nil
			}
			rows, err := st.db.Query(`SELECT task_id, status FROM task_status`)
			if err != nil { return err }
			defer rows.Close()
			for rows.Next() {
				var id, s string
				rows.Scan(&id, &s)
				fmt.Printf("%s %s\n", id, s)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&repo, "repo", ".", "")
	cmd.Flags().StringVar(&task, "task", "", "")
	return cmd
}
```

> `st.db` 是私有字段：给 `state.Store` 加一个 `func (s *Store) ListStatuses() ([]struct{ID,Status string}, error)` 方法（小改 `state.go`），status 命令改用它。**实现时加这个方法，不要在 cli 里戳私有字段。**

```go
// internal/cli/replay.go
package cli

import (
	"fmt"
	"github.com/spf13/cobra"
	"loop-eng/internal/state"
)

func NewReplayCmd() *cobra.Command {
	var repo, run string
	cmd := &cobra.Command{
		Use: "replay",
		RunE: func(_ *cobra.Command, _ []string) error {
			st := mustOpenState(repo)
			defer st.Close()
			steps, err := st.Replay(run)
			if err != nil { return err }
			for _, s := range steps {
				fmt.Printf("%d %s %s\n", s.Seq, s.Role, s.Status)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&repo, "repo", ".", "")
	cmd.Flags().StringVar(&run, "run", "", "")
	return cmd
}
```

```go
// internal/cli/skill.go
package cli

import (
	"fmt"
	"github.com/spf13/cobra"
	"loop-eng/internal/skill"
)

func NewSkillCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "skill"}
	cmd.AddCommand(&cobra.Command{
		Use: "test",
		Short: "列出内置 skill（M1 占位；完整回归集见 spec §11）",
		RunE: func(_ *cobra.Command, _ []string) error {
			for _, e := range skill.Defaults {
				fmt.Printf("%s v%s\n", e.Name, e.Version)
			}
			return nil
		},
	})
	return cmd
}
```

注册三个命令到 root。

- [ ] **Step 4: 跑测试**

Run: `go test ./internal/cli/ -v && go test ./... `
Expected: PASS（全包）

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat(cli): status / replay / skill test"
```

---

## Task 16: 端到端 hello-world 测试（验收）

**Files:**
- Create: `test/e2e/helloworld_test.go`

**Interfaces:**
- Consumes: `SubLoop` + Fake 模型 + 真隔离 worktree。
- Produces：一个用 Fake 模型跑通「plan→execute→verify→writeback→done」的端到端测试，断言 state 里有 done transition、outbox 有战报。

- [ ] **Step 1: 写测试**

```go
// test/e2e/helloworld_test.go
package e2e

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"testing"

	"loop-eng/internal/budget"
	"loop-eng/internal/channel"
	"loop-eng/internal/loop"
	"loop-eng/internal/model"
	"loop-eng/internal/skill"
	"loop-eng/internal/state"
	"loop-eng/internal/verify"
)

func gitRepo(t *testing.T) string {
	dir := t.TempDir()
	for _, c := range [][]string{
		{"git", "init", "-q", dir},
		{"git", "-C", dir, "config", "user.email", "t@t"},
		{"git", "-C", dir, "config", "user.name", "t"},
	} {
		out, err := exec.Command(c[0], c[1:]...).CombinedOutput()
		if err != nil { t.Fatalf("%v: %s", err, out) }
	}
	exec.Command("sh", "-c", "echo hi > "+filepath.Join(dir, "README")).Run()
	exec.Command("git", "-C", dir, "add", "-A").Run()
	exec.Command("git", "-C", dir, "commit", "-q", "-m", "i").Run()
	return dir
}

func TestHelloWorldEndToEnd(t *testing.T) {
	repo := gitRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()

	m := model.NewFake(map[string]string{
		"PLAN:":    j(skill.PlanOutput{}),
		"EXECUTE:": "done",
		"VERIFY:":  j(skill.VerifyOutput{Passed: true, Reason: "file created"}),
	})
	vskill := skill.Skill[skill.VerifyInput, skill.VerifyOutput]{
		Name: "verify", PromptTmpl: "VERIFY: {{.Diff}}",
		ParseJSON: func(b []byte) (skill.VerifyOutput, error) {
			var o skill.VerifyOutput
			return o, json.Unmarshal(b, &o)
		}, Model: m,
	}
	sl := &loop.SubLoop{
		Repo: repo, Store: st, Budget: budget.New(100000, 1000000, 3),
		Execute: m,
		Plan: skill.Skill[skill.PlanInput, skill.PlanOutput]{
			Name: "plan", PromptTmpl: "PLAN: {{.Task}}",
			ParseJSON: func(b []byte) (skill.PlanOutput, error) {
				var o skill.PlanOutput
				return o, json.Unmarshal(b, &o)
			}, Model: m,
		},
		Tiers:   []verify.Tier{verify.LLM{Skill: vskill}, verify.HumanStub{}},
		Channel: channel.NewLocal(t.TempDir()),
	}

	out, err := sl.Run(context.Background(), channel.Task{
		Ref: "1", Description: "创建 greet.txt 内容 hello",
		AcceptanceCriteria: []string{"文件 greet.txt 存在且内容为 hello"},
		TaskType: "feature",
	})
	if err != nil { t.Fatal(err) }
	if out.Status != "done" {
		t.Fatalf("want done, got %s (%s)", out.Status, out.Detail)
	}
	steps, _ := st.Replay("task_") // Replay 按 runID；这里宽松断言有 plan/execute/verify step
	_ = steps
}

func j(v any) string { b, _ := json.Marshal(v); return string(b) }
```

> `Replay("task_")` 是宽松断言；如需精确，把 `SubLoop.Run` 里 runID 固定为 taskID 并在测试里取出。M1 接受宽松断言（status==done + 无 error 即过）。

- [ ] **Step 2: 跑测试**

Run: `go test ./test/e2e/ -v`
Expected: PASS

- [ ] **Step 3: 全量构建 + 测试**

Run: `CGO_ENABLED=0 go build ./... && go test ./...`
Expected: 全绿

- [ ] **Step 4: Commit**

```bash
git add -A && git commit -m "test(e2e): hello-world 端到端验收（M1 完成）"
```

---

## Task 17: skill 初稿 prompt（4 个 markdown）

**Files:**
- Modify: `internal/cli/embed/skills/{triage,plan,verify,help}.md`

**Interfaces:**
- Produces：四个 skill 的真实初稿 prompt（用户后续在 `.loop/skills/` 覆盖、用 seed 任务调）。

- [ ] **Step 1: 写 triage 初稿**

```markdown
TRIAGE: 你是 loop 的分诊器。判断下面任务能不能开始、loop 能不能干、要不要人。
任务: {{.TaskDescription}}
类型: {{.TaskType}}
验收标准: {{.AcceptanceCriteria}}

只输出 JSON：{"startable":bool,"missing_info":[...],"loop_doable":bool,"suggested_type":"...","difficulty":"low|med|high","needs_human_decision":bool,"reason":"..."}
规则：验收标准无法程序化判断也不可人审 → startable=false（missing_info 写缺什么）。部署类 → needs_human_decision=true。
```

- [ ] **Step 2: 写 plan 初稿**

```markdown
PLAN: 你是 loop 的计划器。只规划、不写代码。
任务: {{.Task}}
验收标准: {{.AcceptanceCriteria}}
前几轮战报/失败: {{.BattleReport}}

只输出 JSON：{"plan":[{"step":"...","files":["..."],"expected":"..."}],"risks":["..."]}
```

- [ ] **Step 3: 写 verify 初稿（新鲜上下文，绝不看执行推理）**

```markdown
VERIFY: 你是独立的验证器。只看 diff 和验收标准，判断是否满足。上一轮失败（若有）: {{.PriorFailureSignal}}
diff:
{{.Diff}}
验收标准:
{{.AcceptanceCriteria}}

只输出 JSON：{"passed":bool,"reason":"...","failing_criteria":["..."]}
铁律：你不知道、也不关心执行端怎么想的；只对 diff 和标准负责。
```

- [ ] **Step 4: 写 help 初稿**

```markdown
HELP: 子 loop 卡住了，把它转成结构化求助。
任务: {{.Task}}
卡在: {{.BlockedState}}
试过: {{.AttemptsSummary}}
最后错误: {{.LastError}}

只输出 JSON：{"help_request":{"stuck_at":"...","tried":[...],"need_from_human":"..."}}
另：若是验收标准本身错了/不全（criteria-mismatch），need_from_human 写「需更新验收标准：...」，不要自己改标准。
```

- [ ] **Step 5: 确认 embed 编译 + commit**

Run: `CGO_ENABLED=0 go build ./...`
Expected: 成功（embed 路径无误）

```bash
git add -A && git commit -m "feat(skill): 四个 skill 的初稿 prompt"
```

---

## M1 完成标准（Definition of Done）

- [ ] `CGO_ENABLED=0 go build ./...` 产出单一二进制 `loop-eng`。
- [ ] `go test ./...` 全绿（含 hello-world e2e）。
- [ ] `loop-eng init` 能在某仓库生成 `.loop/`。
- [ ] `loop-eng run-once --models fake` 能把 inbox 任务跑出 done 并写战报。
- [ ] tier1/tier2 真实路径：tier1 跑确定性脚本、tier2 走 `claude -p` 新会话（手动 smoke 验证）。
- [ ] 三道预算刹车存在、config 缺失即硬错误。
- [ ] state 可回放（`replay` 打印 steps）。

**M1 不做（留给 M2/M3）：** 真 GitHub Issue 通道；常驻 daemon；并发；park/resume；异步 tier-3；priority/依赖/scope；看板 TUI。

---

## 自检（Self-Review）

**1. Spec 覆盖（M1 范围）：**
- 主循环/子循环编排 → Task 12 ✓
- 三层验证（tier1/tier2 真、tier3 stub）→ Task 9/10 ✓
- 4 skill（含初稿）→ Task 8/17 ✓
- 两条模型路径（API + claude-p）→ Task 7 ✓
- 工单通道接口 + stub → Task 11 ✓
- 可回放 SQLite trace → Task 3/4 ✓
- 三道预算刹车 → Task 5 ✓
- worktree 隔离 → Task 6 ✓
- skill 回归框架（M1 最小：`skill test` 列表；完整 fixture 集 M3+）→ Task 15 部分覆盖，已注明
- 可恢复（M1：state 落盘；daemon 重启恢复留 M3）→ Task 3/4 数据层就绪
- criteria-mismatch → help skill 初稿含提示（Task 17）；真 park 走 M3
- CLI init/run-once/status/replay/skill → Task 13/14/15 ✓

**2. 占位符扫描：** 无 TBD/TODO；`mustLoad/mustOpenState` 在 Task 14 给了真实实现（Step 4）；anthropic SDK 字段名留了「按 go doc 修正」的实现指引（非占位，是合理的版本适配说明）。

**3. 类型一致性：** `model.Client.Call`、`skill.Skill[I,O].Run`、`verify.Tier.Check`、`channel.Channel`、`budget.Enforcer`、`state.Store` 签名在所有任务里一致。`SubLoop` 字段名（Repo/Store/Budget/Execute/Plan/Tiers/Channel）Task 12 定义、Task 14/16 消费一致。

**已知 M1 简化（实现者注意）：**
- `run-once` 用 panic 处理加载失败（开发入口；M3 daemon 改 error+评论）。
- `SubLoop.Run` 的 runID 复用 taskID（Replay 测试宽松断言）。
- tier1 确定性脚本在 `buildTiers` 里暂未从 config 动态装配（M1 链 = LLM + HumanStub）；`Deterministic` 已实现并可单测，M3 在 daemon 组装时接入 config 列表。

---

## 实现裁决（2026-07-08 预检 + 设计意图对齐）

> 预检发现 plan 字面代码与 spec 意图 / DoD 在若干处自相矛盾。以下按 spec 意图裁决，**实现时以此为准**（优先级高于上方各 Task 的字面样例代码）。每条标注 spec 依据。

**A. tier-3 stub 不得阻断 M1 成功路径 —— 改 `SubLoop`，不改 `HumanStub`。**
- 矛盾：`HumanStub.Check` 返回 `{Passed:true, NeedsHuman:true}`（Task 10），`Chain` 返回末层结果故带 `NeedsHuman:true`；而 `SubLoop.Run` 先判 `NeedsHuman`→`needs-review`，永远到不了 `done`——与 Task 12/14/16 的 `done` 断言、DoD 2702、plan 注释 1539（"M1 里 tier3 不阻断"）全冲突。
- spec 依据：§2（M1 = tier1/2 + stub）、§7.2/§8.6（tier-3 = **异步**人审 + park + 释放活跃位，**全靠 daemon**）、plan Global Constraint（M1 不做 daemon/park/异步 tier-3）。M1 无 daemon ⇒ tier-3 无法真正执行 ⇒ `HumanStub` 仅占位 + 供 `Chain` 单测。
- **裁决 A1**：`SubLoop.Run` 尾部，`res.Passed==true` → `done`，**不**因 `res.NeedsHuman` 走 needs-review（那是 M3 park 路由）。把 `NeedsHuman` 记进该 verify step 的 trace（信息不丢），留 `// M3: NeedsHuman → park via daemon` 钩子。`HumanStub` / `Chain` / 各任务 `Tiers` 接线**不变**。`needs-review`/`needs-info` 保留为 `Outcome.Status` 枚举（M3 才产生）。

**B. `done` 必须写战报 —— `SubLoop` 每个终态 PostComment。**
- 矛盾：`SubLoop` 仅 `blocked` 时 `PostComment`（plan:1948），`done` 不写；但 DoD 2702（"跑出 done **并写战报**"）、Task 14 `outbox/1.md` 断言都要求写。
- spec 依据：§7.2d（写回 = 落盘 trace + 战报(issue 评论) + 摄取回主循环）、§14（issue 评论即战报）——写回是**每个终态**的动作。
- **裁决 B1**：`SubLoop` 在每个终态 PostComment：`done`→`DONE: <detail>`、`blocked`→`BLOCKED: <reason>`（已有）、`needs-info`/`needs-review` 各写一条（M3 用）。建议抽 `sl.report(ctx, taskID, task, status, detail)` 统一 `AppendTransition`+`PostComment`。

**C. `worktreeDiff` 在 worktree 内取 —— 修 Task 12。**
- 矛盾：plan:1953 `git -C repo diff HEAD -- wt` 抓不到 worktree 的改动。
- spec 依据：§8.9（"验证通过时，worktree 的 diff 就是产物"）。
- **裁决**：实现为 `git -C wt --no-pager diff HEAD`（worktree 内、相对 base HEAD 的改动；M1 execute 不 commit，足够）。commit 场景的 diff 留 M3。

**D. embed 统一在 `internal/cli/embed/skills/` —— Task 8 起即在此建。**
- 矛盾：Task 8 写 `internal/embed/skills/`、Task 13（plan:2159）改 `internal/cli/embed/skills/`。
- **裁决**：Task 8 直接在 `internal/cli/embed/skills/` 建 4 个 md；`skill.Defaults.DefaultEmbedPath` 同步改为 `internal/cli/embed/skills/<name>.md`。免来回挪。

**E. tier-1 动态装配确认延后到 M3 —— 非 M1 疏漏。**
- spec 依据：§8.6/§8.2（tier-1 = 配置的 `verify.deterministic` 在 worktree 里跑）。但 `Deterministic.Dir` 需 = 每轮新建的 worktree 路径，而 `Tier.Check` 签名被 Global Constraint 冻结、`buildTiers` 在 run-once 起点调用时还不知道 worktree —— 干净接入要动签名或重构 SubLoop，超出 M1 同步单任务切片。
- **裁决**：M1 运行时链 = `[tier-2 LLM, HumanStub]`（`Deterministic` 原语在 Task 9 完整实现+单测；M3 daemon 组装时按 config 动态接入并注入 worktree Dir）。DoD 2703「tier1 跑确定性脚本（手动 smoke 验证）」由 Task 9 单测 + 手动 smoke 满足，不接入 run-once 自动链。

**F. Task 15 不戳私有字段 —— 加 `state.Store.ListStatuses()`。**
- spec 依据：§6.2（`state.Store` 只追加写入器 + 回放读取器 + 生命周期读写，通过类型化接口）。plan:2441 已注。
- **裁决**：给 `state.Store` 加 `ListStatuses() ([]struct{ ID, Status string }, error)`（读 `task_status`）；status 命令用它，不在 cli 戳 `st.db`。

**G. Task 7 api.go 清理无用 import。**
- **裁决**：去掉 dummy `init(){ _ = json.Marshal }` / `var _ = fmt.Sprint`，直接删未用的 `encoding/json`、`fmt` import。anthropic SDK 字段名按 `go doc` 对齐所 pin 版本，不留 TODO。

**H. init 把 `.loop/` 加进 `.gitignore` —— Task 13 小改。**
- 理由：worktree 在 `.loop/worktrees/`，不 ignore 会污染用户仓库 `git status`。init 时 append `.loop/`（无 `.gitignore` 则新建）。

**I. 单路径 claude-p —— 移除 anthropic SDK 直连（v3.1 决策，覆盖 §8.10 原两条路径）。**
- 用户决策（2026-07-08）：triage/plan/execute/verify 全走 `claude -p`，**不**直连 API。
- 理由：① 安装/配置最简（只需 `claude` CLI，无需配 `ANTHROPIC_API_KEY`）；② 不碰用户的 key（`claude` 自管认证）；③ agent 可替换（shell-out 是 provider 中立的，换 agent 只改 config 里的 `binary`）。
- 代价：triage/plan 轻量判断也要拉起 `claude` 会话，比直连 API 重——个人工具接受。
- **落地（覆盖 Task 7/2/13/14 的字面样例）：**
  - Task 7：删除 `internal/model/api.go` + 移除 `anthropics/anthropic-sdk-go` 依赖（`go mod tidy`）；只留 `Client` 接口 + `Usage` + `FakeClient`（测试）+ `ClaudeClient`（唯一真实实现）。裁决 G（api.go 清理）随 api.go 一并消失。
  - Task 2：config validate 放宽——model 只要有 `Name` 或 `Binary` 之一即视为已配置（不再强制 `triage.name` 非空）。
  - Task 13：默认 config 四个角色全 `{ via: claude-p, binary: claude }`。
  - Task 14：`buildModels` real 模式已是全 `ClaudeClient`（原注释「plan 也用 claude 简化」现改为设计本意）；不再有任何 API 分支。

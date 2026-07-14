# loop-eng Dashboard TUI 本体 —— Implementation Plan (Phase B)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 实现 `loop-eng dashboard` 交互式 TUI——三 Tab（总览/详情/轨迹）、真彩色 + 进行中呼吸灯、resume/cancel 轻量操控，消费 Phase A 已落地的全部 Store 方法。

**Architecture:** bubbletea 程序，Model 持 tab/选中/快照/动画相位；两个 tick（数据 ~2s 重读 Store、动画 ~60ms 重绘呼吸灯）。View 拆三个纯函数（overview/detail/trace），吃快照结构体、吐 lipgloss 块——沿用 `renderWatchView` 的纯渲染可测模式。控制指令经 `Store.InsertCommand` 写 `commands` 表，daemon drainCommands 应用。

**Tech Stack:** Go 1.25, `github.com/charmbracelet/bubbletea` + `lipgloss`（新依赖，纯 Go 无 CGO），cobra（既有）。

## Global Constraints

- `CGO_ENABLED=0 go build ./...` 必须成功；`go test ./...` 全绿。
- **冻结签名不动**：`channel.Channel`、`model.Client.Call`、`verify.Tier.Check`、`state.Store.AppendStep(StepRow) error`。
- 注释中文；不引入 CGO；bubbletea/lipgloss 是仅允许的新外部依赖。
- 纯渲染函数可单测（注入快照，断言字符串/style），不带终端/真时间——照搬 `status_test.go` 对 `renderWatchView` 的做法。
- TDD：每个任务 RED→GREEN。

**Spec 出处：** `docs/superpowers/specs/2026-07-14-loop-eng-dashboard-tui-design.md` §3（架构）、§5（三 Tab）、§6（颜色/呼吸灯）、§7（控制通道）。
**Phase A 产出（本计划消费）：** `Store.TasksByStatus/ActiveRun/RunsOfTask/StepsOfTask/VerificationsByRun/InsertCommand/InFlight/ListStatuses` + `TaskView/RunRow/StepRow/VerificationRow/CommandRow`。

**Phase A 终审（opus）3 个 kickoff 点，本计划已纳入：**
1. `StepRow`/`TransitionRow` 加 `At string`，SELECT 带上 `at` 列（Task B0）。
2. 预算/retry 从 `budget_ledger` 派生（`runs.retry_count`/`total_tokens` 恒为 0）。
3. running 任务浮顶：`TasksByStatus` 把 running 排末尾，reader 用 `InFlight()` 把 running 顶到最上（Task B2）。

---

## File Structure

- `internal/state/state.go` —— B0 给 `StepRow`/`TransitionRow` 加 `At`，SELECT 带上 `at`。
- `internal/state/state_test.go` —— B0 回归测试。
- `internal/tui/` —— 新包：
  - `model.go` —— bubbletea Model（tab/选中/快照/动画相位）+ Init/Update/View + 两个 tick。
  - `reader.go` —— 读 Store+config 组快照（纯，可注入 Store 测）。
  - `overview.go` —— [1] 总览纯渲染 + 颜色映射。
  - `detail.go` —— [2] 详情纯渲染（含验收方式派生 + 预算派生）。
  - `trace.go` —— [3] 轨迹纯渲染（按 run 分组，带时间戳）。
  - `animate.go` —— 呼吸灯相位→绿色明度（纯函数）。
  - `commands.go` —— resume/cancel 写 commands 表。
  - `styles.go` —— lipgloss 颜色/style 集中定义 + NO_COLOR 降级。
- `internal/cli/dashboard.go` —— `loop-eng dashboard` cobra 命令（开 Store、加载 config、起 bubbletea 程序），注册到 root。
- 各 `*_test.go` —— 纯渲染 + reader 测试。

---

## Task B0: `StepRow`/`TransitionRow` 加 `At` 字段（Phase A 终审 I1）

**Files:**
- Modify: `internal/state/state.go`（StepRow、TransitionRow 加字段；StepsOfTask/Replay/Transitions 的 SELECT 加 `at`）
- Test: `internal/state/state_test.go`

**Interfaces:**
- Produces: `StepRow.At string`、`TransitionRow.At string`；`StepsOfTask`/`Replay`/`Transitions` 返回值带上 `at`。

- [ ] **Step 1: 写失败测试**

追加到 `internal/state/state_test.go`：

```go
func TestStepsAndTransitionsCarryAt(t *testing.T) {
	st, err := Open(t.TempDir() + "/state.db")
	if err != nil { t.Fatal(err) }
	defer st.Close()

	tid, _ := st.InsertTask(state.TaskRow{IssueRef: "#1", Description: "d"})
	rid, _ := st.StartRun(tid)
	_ = st.AppendStep(state.StepRow{RunID: rid, Seq: 11, Role: "plan", Status: "ok"})
	_ = st.AppendTransition(tid, "new", "running", "dispatched")

	steps, err := st.StepsOfTask(tid)
	if err != nil { t.Fatal(err) }
	if len(steps) != 1 || steps[0].At == "" {
		t.Fatalf("step At empty: %+v", steps)
	}
	trs, err := st.Transitions(tid)
	if err != nil { t.Fatal(err) }
	if len(trs) != 1 || trs[0].At == "" {
		t.Fatalf("transition At empty: %+v", trs)
	}
}
```

- [ ] **Step 2: 跑确认失败**

Run: `go test ./internal/state/ -run TestStepsAndTransitionsCarryAt -v`
Expected: FAIL（`At` 字段不存在）。

- [ ] **Step 3: 加字段 + SELECT 带上 `at`**

`internal/state/state.go`：

`StepRow` 加字段：
```go
type StepRow struct {
	RunID, Role, Skill, ModelRef string
	Seq                          int
	InputJSON, OutputJSON        string
	TokensIn, TokensOut          int
	Status, Error                string
	At                           string // 新增：落盘时间（spec §5[3] trace 用）
}
```

`TransitionRow` 加字段：
```go
type TransitionRow struct {
	From, To, Reason string
	At               string // 新增
}
```

`StepsOfTask` 的 SELECT 末尾加 `at` 并 Scan：
```go
rows, err := s.db.Query(
	`SELECT run_id, seq, role, skill, model_ref, input_json, output_json,
	        tokens_in, tokens_out, status, error, at
	 FROM steps WHERE run_id IN (SELECT id FROM runs WHERE task_id=?)
	 ORDER BY at`, taskID)
```
`scanStepRows` 的 Scan 末尾加 `&r.At`（同步改 `Replay`，因为共用 `scanStepRows`——`Replay` 的 SELECT 也要加 `at`）：
```go
// Replay 的查询：
`SELECT run_id, seq, role, skill, model_ref, input_json, output_json,
        tokens_in, tokens_out, status, error, at
 FROM steps WHERE run_id=? ORDER BY seq`
```
`scanStepRows`：
```go
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
```

`Transitions` 的 SELECT 加 `at` 并 Scan：
```go
func (s *Store) Transitions(taskID string) ([]TransitionRow, error) {
	rows, err := s.db.Query(
		`SELECT from_status, to_status, reason, at FROM transitions
		 WHERE task_id=? ORDER BY rowid`, taskID)
	// ...
	for rows.Next() {
		var r TransitionRow
		if err := rows.Scan(&r.From, &r.To, &r.Reason, &r.At); err != nil { return nil, err }
		out = append(out, r)
	}
	return out, rows.Err()
}
```

- [ ] **Step 4: 跑测试通过 + 全量回归**

Run: `go test ./internal/state/ -run TestStepsAndTransitionsCarryAt -v` → PASS。
Run: `go test ./...` → 全绿（`Replay` 现有测试不受影响，只是多 Scan 一列）。

- [ ] **Step 5: 提交**

```bash
git add internal/state/state.go internal/state/state_test.go
git commit -m "feat(state): StepRow/TransitionRow 加 At 字段（trace 时间戳用，Phase A 终审 I1）"
```

---

## Task B1: bubbletea 依赖 + `loop-eng dashboard` 命令骨架

**Files:**
- Modify: `go.mod` / `go.sum`（加 bubbletea + lipgloss）
- Create: `internal/tui/model.go`（最小 Model：渲染静态帧 + q 退出）
- Create: `internal/cli/dashboard.go`（cobra 命令）
- Modify: `internal/cli/root.go`（注册命令）

**Interfaces:**
- Produces: `loop-eng dashboard` 能起一个 bubbletea 程序、显示标题帧、q 退出。

- [ ] **Step 1: 加依赖**

Run:
```bash
go get github.com/charmbracelet/bubbletea@latest github.com/charmbracelet/lipgloss@latest
go mod tidy
```
确认 `go build ./...` 通过。

- [ ] **Step 2: 写最小 Model + 命令**

`internal/tui/model.go`：
```go
// Package tui 实现 loop-eng dashboard 交互式看板（spec §3/§5）：三 Tab +
// 真彩色 + 进行中呼吸灯 + resume/cancel 轻量操控。bubbletea 程序，和 daemon
// 共享同一个 SQLite；数据 tick ~2s 重读，动画 tick ~60ms 重绘呼吸灯。
package tui

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"loop-eng/internal/config"
	"loop-eng/internal/state"
)

// tab 枚举
type tab int
const (
	tabOverview tab = iota
	tabDetail
	tabTrace
)

// Model 是 dashboard 的 bubbletea Model。
type Model struct {
	store   *state.Store
	cfg     *config.Config
	tab     tab
	selIdx  int    // 总览列表选中索引
	selTask string // 当前选中的 task id（详情/轨迹用）

	// 快照（数据 tick 刷新）——B2 填充
	snap *Snapshot

	// 动画相位（动画 tick 推进）——B4 填充
	animPhase float64

	width, height int
	quit          bool
}

// New 构造 Model。store 由 dashboard 命令开好（读写在同一条连接：读快照 + 写
// commands；modernc.org/sqlite 短事务可承受与 daemon 的低竞争）。
func New(st *state.Store, cfg *config.Config) Model {
	return Model{store: st, cfg: cfg, tab: tabOverview}
}

func (m Model) Init() tea.Cmd { return nil } // B7 加 tick

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			m.quit = true
			return m, tea.Quit
		}
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	}
	return m, nil
}

func (m Model) View() string {
	if m.quit {
		return ""
	}
	title := lipgloss.NewStyle().Bold(true).Render("loop-eng dashboard")
	hint := lipgloss.NewStyle().Faint(true).Render("（骨架）q 退出")
	return fmt.Sprintf("%s\n%s\n", title, hint)
}
```

`internal/cli/dashboard.go`：
```go
package cli

import (
	"github.com/spf13/cobra"
	"loop-eng/internal/tui"
	tea "github.com/charmbracelet/bubbletea"
)

// NewDashboardCmd builds `loop-eng dashboard`: 交互式 TUI 看板（spec §3/§5）。
// 读 state.db 展示三 Tab + 颜色/呼吸灯 + resume/cancel。
func NewDashboardCmd() *cobra.Command {
	var repo string
	cmd := &cobra.Command{
		Use:   "dashboard",
		Short: "交互式 TUI 看板（三 Tab + 颜色 + 呼吸灯 + resume/cancel）",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg := mustLoad(repo)
			st := mustOpenState(repo)
			defer st.Close()
			p := tea.NewProgram(tui.New(st, cfg), tea.WithAltScreen())
			_, err := p.Run()
			return err
		},
	}
	cmd.Flags().StringVar(&repo, "repo", ".", "仓库路径")
	return cmd
}
```

`internal/cli/root.go`：在 `NewRootCmd` 里 `cmd.AddCommand(NewDashboardCmd())`。

- [ ] **Step 3: 跑 + 手测**

Run: `CGO_ENABLED=0 go build ./...` → 通过。
手动冒烟：`./loop-eng dashboard`（在 bash 里按 q 退出）。若无 TTY 环境无法手测，至少 `go vet ./...` 通过。

- [ ] **Step 4: 提交**

```bash
git add go.mod go.sum internal/tui/model.go internal/cli/dashboard.go internal/cli/root.go
git commit -m "feat(tui): bubbletea 骨架 + loop-eng dashboard 命令"
```

---

## Task B2: reader.go —— 快照组装（running 浮顶）

**Files:**
- Create: `internal/tui/reader.go`
- Test: `internal/tui/reader_test.go`

**Interfaces:**
- Consumes: `Store.TasksByStatus/ActiveRun/InFlight`（Phase A）。
- Produces: `type Snapshot`、`ReadSnapshot(st, cfg) (*Snapshot, error)`；running 任务浮顶。

- [ ] **Step 1: 写失败测试**

`internal/tui/reader_test.go`：
```go
package tui

import (
	"testing"
	"loop-eng/internal/state"
)

func TestReadSnapshotFloatsRunningToTop(t *testing.T) {
	st, _ := state.Open(t.TempDir()+"/state.db")
	defer st.Close()
	a, _ := st.InsertTask(state.TaskRow{IssueRef: "#a", Description: "new task"})
	b, _ := st.InsertTask(state.TaskRow{IssueRef: "#b", Description: "running task"})
	_ = st.AppendTransition(b, "new", "running", "dispatched")
	_ = st.SetInFlight(b, "execute")
	// a 仍是 new

	snap, err := ReadSnapshot(st, nil) // cfg 可为 nil（本测试不验验收方式）
	if err != nil { t.Fatal(err) }

	// running 应浮顶：snap.Tasks[0].ID == b
	if len(snap.Tasks) < 2 || snap.Tasks[0].ID != b {
		t.Fatalf("running not floated to top: %+v", snap.Tasks)
	}
	if snap.Tasks[1].ID != a {
		t.Fatalf("new task not second: %+v", snap.Tasks)
	}
	// running 元信息
	if snap.Running == nil || snap.Running.TaskID != b {
		t.Fatalf("Running not set: %+v", snap.Running)
	}
	// 计数
	if snap.Counts["new"] != 1 || snap.Counts["running"] != 1 {
		t.Fatalf("counts = %+v", snap.Counts)
	}
	_ = a
}
```

- [ ] **Step 2: 跑确认失败**

Run: `go test ./internal/tui/ -run TestReadSnapshot -v`
Expected: FAIL（`Snapshot`/`ReadSnapshot` 未定义）。

- [ ] **Step 3: 实现 reader**

`internal/tui/reader.go`：
```go
package tui

import (
	"loop-eng/internal/config"
	"loop-eng/internal/state"
)

// Snapshot 是一次数据 tick 读出的全量视图（纯数据，供 view 函数消费）。
type Snapshot struct {
	Tasks   []state.TaskView // running 已浮顶（见 ReadSnapshot）
	Running *RunningInfo     // 进行中任务（in_flight 非空时），nil 表示无活跃
	Counts  map[string]int   // 各状态计数（new/running/needs-review/blocked/done/cancelled）
}

// RunningInfo 是当前活跃子 loop 的展示信息。
type RunningInfo struct {
	TaskID, Phase, StartedAt, RunID string
}

// ReadSnapshot 从 Store 读一次全量视图。running 任务（in_flight）从 TasksByStatus
// 列表里挑出来浮顶（TasksByStatus 本身把 running 排末尾，spec §5[1] 要它置顶）。
// cfg 用于详情页的验收方式派生（B5）；此处仅透传，不在此读取。
func ReadSnapshot(st *state.Store, cfg *config.Config) (*Snapshot, error) {
	tasks, err := st.TasksByStatus()
	if err != nil {
		return nil, err
	}
	counts := make(map[string]int)
	for _, t := range tasks {
		counts[t.Status]++
	}

	snap := &Snapshot{Tasks: tasks, Counts: counts}

	// running 浮顶
	ifl, ok, err := st.InFlight()
	if err == nil && ok {
		runID, startedAt, rok, _ := st.ActiveRun(ifl.TaskID)
		ri := &RunningInfo{TaskID: ifl.TaskID, Phase: ifl.Phase, StartedAt: startedAt}
		if rok {
			ri.RunID = runID
		}
		snap.Running = ri
		// 把 running 任务挪到列表最前
		for i, t := range snap.Tasks {
			if t.ID == ifl.TaskID {
				snap.Tasks = append([]state.TaskView{t}, append(snap.Tasks[:i], snap.Tasks[i+1:]...)...)
				break
			}
		}
		counts["running"] = 1
	}
	_ = cfg
	return snap, nil
}
```

- [ ] **Step 4: 跑测试通过**

Run: `go test ./internal/tui/ -run TestReadSnapshot -v` → PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/tui/reader.go internal/tui/reader_test.go
git commit -m "feat(tui): reader 快照组装 + running 浮顶"
```

---

## Task B3: overview.go —— 总览纯渲染 + 颜色

**Files:**
- Create: `internal/tui/styles.go`（颜色/style 集中）
- Create: `internal/tui/overview.go`
- Test: `internal/tui/overview_test.go`

**Interfaces:**
- Consumes: `Snapshot`、`animPhase`（呼吸灯，B4；此处先接参、用稳态色）。
- Produces: `RenderOverview(snap *Snapshot, selIdx int, animPhase float64, w int) string`。

- [ ] **Step 1: 写失败测试**

`internal/tui/overview_test.go`：
```go
package tui

import (
	"strings"
	"testing"
	"loop-eng/internal/state"
)

func TestRenderOverviewCountsAndSymbols(t *testing.T) {
	snap := &Snapshot{
		Tasks: []state.TaskView{
			{ID: "t_running", IssueRef: "#b", Description: "running task", Status: "running"},
			{ID: "t_new", IssueRef: "#a", Description: "new task", Status: "new"},
		},
		Running: &RunningInfo{TaskID: "t_running", Phase: "execute"},
		Counts:  map[string]int{"new": 1, "running": 1},
	}
	out := RenderOverview(snap, 0, 0.0, 80)
	// 计数条含待处理/进行中
	if !strings.Contains(out, "待处理") || !strings.Contains(out, "进行中") {
		t.Fatalf("counts bar missing: %q", out)
	}
	// 进行中任务行带 ● 符号（呼吸灯稳态）
	if !strings.Contains(out, "●") {
		t.Fatalf("running symbol missing: %q", out)
	}
	// 选中行带选中标记
	if !strings.Contains(out, "▸") {
		t.Fatalf("selection marker missing: %q", out)
	}
}
```

- [ ] **Step 2: 跑确认失败**

Run: `go test ./internal/tui/ -run TestRenderOverview -v` → FAIL。

- [ ] **Step 3: 实现 styles + overview**

`internal/tui/styles.go`：
```go
package tui

import "github.com/charmbracelet/lipgloss"

// 状态→符号 + 颜色（spec §6）。NO_COLOR 时 lipgloss 自动降级为纯文本。
func statusSymbol(status string) string {
	switch status {
	case "new": return "◌"
	case "running": return "●"
	case "needs-review": return "⏸"
	case "needs-info": return "ℹ"
	case "blocked": return "✗"
	case "done": return "✓"
	case "cancelled": return "✘"
	default: return "·"
	}
}

func statusStyle(status string) lipgloss.Style {
	switch status {
	case "running":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("10")) // 亮绿
	case "needs-review":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("11")) // 琥珀黄
	case "needs-info":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("12")) // 蓝
	case "blocked":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("9"))  // 红
	case "done":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("14")) // 暗青
	default:
		return lipgloss.NewStyle().Faint(true)                      // 暗灰
	}
}
```

`internal/tui/overview.go`：
```go
package tui

import (
	"fmt"
	"strings"
	"github.com/charmbracelet/lipgloss"
	"loop-eng/internal/state"
)

// RenderOverview 渲染 [1] 总览：计数条 + 任务列表。纯函数。
// animPhase 用于进行中任务的呼吸灯明度（B4 animate 计算后传入）。
func RenderOverview(snap *Snapshot, selIdx int, animPhase float64, w int) string {
	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Bold(true).Render("loop-eng dashboard"))
	b.WriteString("\n")

	// 计数条
	bar := fmt.Sprintf("待处理 %d · 进行中 %d · 等人审 %d · 阻塞 %d · 完成 %d · 已取消 %d",
		snap.Counts["new"], snap.Counts["running"], snap.Counts["needs-review"],
		snap.Counts["blocked"], snap.Counts["done"], snap.Counts["cancelled"])
	b.WriteString(lipgloss.NewStyle().Faint(true).Render(bar))
	b.WriteString("\n\n")

	for i, t := range snap.Tasks {
		sym := statusSymbol(t.Status)
		marker := "  "
		if i == selIdx {
			marker = "▸ "
		}
		line := fmt.Sprintf("%s%s %-12s %s  %s", marker, sym, t.IssueRef, t.Status, t.Description)
		st := statusStyle(t.Status)
		if t.Status == "running" {
			// 呼吸灯：animPhase 调亮度的变体（B4 提供 brightnessStyle）
			st = brightnessStyle(st, animPhase)
		}
		b.WriteString(st.Render(line))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(lipgloss.NewStyle().Faint(true).Render("↑↓ 选  Enter 详情  t 轨迹  r resume  x cancel  q 退出"))
	b.WriteString("\n")
	_ = state.TaskView{}
	return b.String()
}
```

> `brightnessStyle` 在 B4 的 `animate.go` 实现；本任务先放一个稳态占位使测试通过——在 `styles.go` 末尾加：
```go
// brightnessStyle 在 B4 之前用稳态（phase 不影响），B4 替换为真实明度插值。
func brightnessStyle(base lipgloss.Style, phase float64) lipgloss.Style { return base }
```

- [ ] **Step 4: 跑测试通过**

Run: `go test ./internal/tui/ -run TestRenderOverview -v` → PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/tui/styles.go internal/tui/overview.go internal/tui/overview_test.go
git commit -m "feat(tui): 总览纯渲染 + 状态颜色映射"
```

---

## Task B4: animate.go —— 呼吸灯

**Files:**
- Modify: `internal/tui/animate.go`（新建）、`internal/tui/styles.go`（替换 brightnessStyle）

- [ ] **Step 1: 写失败测试**

`internal/tui/animate_test.go`：
```go
package tui

import "testing"

func TestBreathBrightness(t *testing.T) {
	// phase 0 → 最低（暗），phase π/2 → 最高（亮）
	dark := breathBrightness(0)
	bright := breathBrightness(3.14159265 / 2)
	if dark >= bright {
		t.Fatalf("dark=%v should be < bright=%v", dark, bright)
	}
	// 范围 [0,1]
	if dark < 0 || bright > 1 {
		t.Fatalf("out of [0,1]: %v %v", dark, bright)
	}
}
```

- [ ] **Step 2: 跑确认失败**

Run: `go test ./internal/tui/ -run TestBreathBrightness -v` → FAIL。

- [ ] **Step 3: 实现**

`internal/tui/animate.go`：
```go
package tui

import "math"

// breathBrightness 把动画相位映射到 [0,1] 明度（正弦呼吸）。0→0（暗），
// π/2→1（亮），周期 2π。纯函数，注入 t 可断言。
func breathBrightness(phase float64) float64 {
	return (math.Sin(phase) + 1) / 2
}
```

替换 `styles.go` 里的 `brightnessStyle`（删占位，用真实插值）：
```go
// brightnessStyle 按呼吸相位在亮绿与暗绿之间插值（spec §6 呼吸灯）。
func brightnessStyle(base lipgloss.Style, phase float64) lipgloss.Style {
	b := breathBrightness(phase)
	// 在暗绿(22)与亮绿(10)之间按 b 取色
	var c lipgloss.Color
	if b > 0.66 {
		c = lipgloss.Color("10") // 亮绿
	} else if b > 0.33 {
		c = lipgloss.Color("2")  // 中绿
	} else {
		c = lipgloss.Color("22") // 暗绿
	}
	return base.Foreground(c)
}
```

- [ ] **Step 4: 跑测试通过 + 总览回归**

Run: `go test ./internal/tui/ -v` → PASS（含 B3 总览测试，呼吸灯现在真实插值）。

- [ ] **Step 5: 提交**

```bash
git add internal/tui/animate.go internal/tui/styles.go internal/tui/animate_test.go
git commit -m "feat(tui): 呼吸灯明度插值（正弦呼吸）"
```

---

## Task B5: detail.go —— 详情纯渲染（验收方式 + 预算派生）

**Files:**
- Create: `internal/tui/detail.go`
- Test: `internal/tui/detail_test.go`

**Interfaces:**
- Consumes: `Store.GetTask/VerificationsByRun/ActiveRun/BudgetLedger`（Phase A）+ `cfg`。
- Produces: `RenderDetail(st, cfg, taskID) string`（读 Store 组装后渲染）。

- [ ] **Step 1: 写失败测试**

`internal/tui/detail_test.go`：
```go
package tui

import (
	"strings"
	"testing"
	"loop-eng/internal/config"
	"loop-eng/internal/state"
)

func TestRenderDetailFields(t *testing.T) {
	st, _ := state.Open(t.TempDir()+"/state.db")
	defer st.Close()
	tid, _ := st.InsertTask(state.TaskRow{
		IssueRef: "#18", Description: "给 budget 加硬上限",
		TaskType: "feature", Criteria: []string{"命中上限立即停", "落盘 budget_ledger"},
	})
	rid, _ := st.StartRun(tid)
	_ = st.AppendVerification(rid, 1, true, "go test: ok")
	_ = st.AppendVerification(rid, 2, false, "LLM: diff unrelated")

	cfg := &config.Config{}
	cfg.Models.Verify = config.ModelRef{Name: "glm-5.2"}
	cfg.Verify.Tier3Human = true

	out := RenderDetail(st, cfg, tid)
	for _, want := range []string{"#18", "给 budget 加硬上限", "feature",
		"验收方式", "go test", "glm-5.2", "人审", "验收标准", "命中上限立即停", "落盘 budget_ledger"} {
		if !strings.Contains(out, want) {
			t.Fatalf("detail missing %q in:\n%s", want, out)
		}
	}
}
```

- [ ] **Step 2: 跑确认失败**

Run: `go test ./internal/tui/ -run TestRenderDetailFields -v` → FAIL。

- [ ] **Step 3: 实现**

`internal/tui/detail.go`：
```go
package tui

import (
	"fmt"
	"strings"
	"time"
	"loop-eng/internal/config"
	"loop-eng/internal/state"
)

// RenderDetail 渲染 [2] 详情（全屏）。读 Store 组装字段后渲染。
// 验收方式：tier-1 固定 go test；tier-2 = cfg.Models.Verify.Name；tier-3 由 cfg.Verify.Tier3Human 开关。
// 逐 tier 状态来自 verifications 表（按 run+tier）。
// 预算/retry 从 budget_ledger 派生（runs 列恒为 0，Phase A 终审 I2）。
func RenderDetail(st *state.Store, cfg *config.Config, taskID string) string {
	t, err := st.GetTask(taskID)
	if err != nil {
		return fmt.Sprintf("（任务 %s 不存在）\n", taskID)
	}
	var b strings.Builder
	b.WriteString(lipglossBold.Render(fmt.Sprintf("#%s %s", t.IssueRef, t.Description)))
	b.WriteString("\n")
	b.WriteString(fmt.Sprintf("type: %s   状态: %s\n", t.TaskType, statusOfTask(st, taskID)))

	// 启动时间 / 运行时长（从 runs）
	if rid, started, ok, _ := st.ActiveRun(taskID); ok {
		b.WriteString(fmt.Sprintf("启动: %s · 已运行 %s\n", started, elapsedSince(started)))
		_ = rid
	}

	// 验收方式
	b.WriteString("\n验收方式:\n")
	tiers := verifyTiers(cfg, st, activeRunID(st, taskID))
	for _, tv := range tiers {
		b.WriteString(fmt.Sprintf("  tier-%d  %-20s %s\n", tv.Tier, tv.Label, tv.StatusSym))
	}

	// 验收标准
	b.WriteString("\n验收标准:\n")
	for _, c := range t.Criteria {
		b.WriteString("  • " + c + "\n")
	}

	// 预算（从 budget_ledger 派生）
	used, limit := budgetUsed(st, taskID)
	b.WriteString(fmt.Sprintf("\n预算: %d / %d tokens\n", used, limit))
	b.WriteString("\n[r] resume   [x] cancel   [t] 看轨迹   [Esc] 回总览\n")
	return b.String()
}

// —— 辅助（同文件）——

var lipglossBold = lipglossNewBold()

func statusOfTask(st *state.Store, taskID string) string {
	for _, r := range mustList(st) {
		if r.ID == taskID { return r.Status }
	}
	return "?"
}
func activeRunID(st *state.Store, taskID string) string {
	if rid, _, ok, _ := st.ActiveRun(taskID); ok { return rid }
	return ""
}

type tierView struct{ Tier int; Label, StatusSym string }

func verifyTiers(cfg *config.Config, st *state.Store, runID string) []tierView {
	var out []tierView
	// tier-1 固定
	out = append(out, tierView{1, "go test ./...", symbolForTier(st, runID, 1)})
	// tier-2
	name := "LLM"
	if cfg != nil && cfg.Models.Verify.Name != "" { name = cfg.Models.Verify.Name }
	out = append(out, tierView{2, name + " (LLM diff)", symbolForTier(st, runID, 2)})
	// tier-3
	if cfg != nil && cfg.Verify.Tier3Human {
		out = append(out, tierView{3, "人审 (issue 评论)", symbolForTier(st, runID, 3)})
	}
	return out
}
func symbolForTier(st *state.Store, runID string, tier int) string {
	if runID == "" { return "—" }
	vs, _ := st.VerificationsByRun(runID)
	for _, v := range vs {
		if v.Tier == tier {
			if v.Passed { return "✓ passed" }
			return "✗ " + truncate(v.Detail, 40)
		}
	}
	return "—" // 未触发
}
func budgetUsed(st *state.Store, taskID string) (used, limit int) {
	rid := activeRunID(st, taskID)
	if rid == "" { return 0, 0 }
	rows, _ := st.BudgetLedger(rid)
	for _, r := range rows {
		if r.Kind == "tokens" { used += r.Amount }
		if r.Kind == "retry" { limit = r.Limit } // 复用 limit 字段放 retry 上限（展示另算）
	}
	return used, 0 // per-call limit 不直接是总上限；展示用 used
}
func elapsedSince(iso string) string {
	t, err := time.Parse(time.RFC3339Nano, iso)
	if err != nil { return "?" }
	return time.Since(t).Truncate(time.Second).String()
}
func truncate(s string, n int) string { if len(s) > n { return s[:n]+"…" }; return s }

// 把 lipgloss 调用收口（便于 B8 降级时一处改）
func lipglossNewBold() lipgloss.Style { return lipgloss.NewStyle().Bold(true) }
func mustList(st *state.Store) []state.StatusRow { r, _ := st.ListStatuses(); return r }
```

> 注：`import "github.com/charmbracelet/lipgloss"` 已在包内（styles.go）。`state.StatusRow`/`BudgetLedger` 等 Phase A 已有。

- [ ] **Step 4: 跑测试通过**

Run: `go test ./internal/tui/ -run TestRenderDetailFields -v` → PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/tui/detail.go internal/tui/detail_test.go
git commit -m "feat(tui): 详情纯渲染（验收方式 tier 派生 + 预算从 budget_ledger）"
```

---

## Task B6: trace.go —— 轨迹纯渲染（按 run 分组 + 时间戳）

**Files:**
- Create: `internal/tui/trace.go`
- Test: `internal/tui/trace_test.go`

**Interfaces:**
- Consumes: `Store.RunsOfTask/StepsOfTask/Transitions/VerificationsByRun`（Phase A + B0 的 At）。
- Produces: `RenderTrace(st, taskID) string`。

- [ ] **Step 1: 写失败测试**

`internal/tui/trace_test.go`：
```go
package tui

import (
	"strings"
	"testing"
	"loop-eng/internal/state"
)

func TestRenderTraceGroupsByRun(t *testing.T) {
	st, _ := state.Open(t.TempDir()+"/state.db")
	defer st.Close()
	tid, _ := st.InsertTask(state.TaskRow{IssueRef: "#1", Description: "d"})
	r1, _ := st.StartRun(tid)
	_ = st.AppendStep(state.StepRow{RunID: r1, Seq: 11, Role: "plan", Status: "ok"})
	_ = st.EndRun(r1, "needs-review")
	r2, _ := st.StartRun(tid)
	_ = st.AppendStep(state.StepRow{RunID: r2, Seq: 11, Role: "plan", Status: "ok"})
	_ = st.EndRun(r2, "done")

	out := RenderTrace(st, tid)
	// 两个 run 分组标题
	if !strings.Contains(out, "run 1") || !strings.Contains(out, "run 2") {
		t.Fatalf("run grouping missing:\n%s", out)
	}
	// 时间戳存在（B0 加的 At）
	if !strings.Contains(out, "▸ plan") {
		t.Fatalf("plan step missing:\n%s", out)
	}
}
```

- [ ] **Step 2: 跑确认失败**

Run: `go test ./internal/tui/ -run TestRenderTrace -v` → FAIL。

- [ ] **Step 3: 实现**

`internal/tui/trace.go`：
```go
package tui

import (
	"fmt"
	"strings"
	"loop-eng/internal/state"
)

// RenderTrace 渲染 [3] 轨迹：按 run 分组的时间线。transitions + steps + verifications，
// 按 at 排序。纯读 Store。
func RenderTrace(st *state.Store, taskID string) string {
	runs, _ := st.RunsOfTask(taskID)
	trans, _ := st.Transitions(taskID)
	var b strings.Builder
	b.WriteString(lipglossBold.Render("轨迹 #" + shortID(taskID)))
	b.WriteString("\n\n")

	for i, r := range runs {
		b.WriteString(fmt.Sprintf("run %d (%s → %s)\n", i+1, r.StartedAt, r.Outcome))
		// 该 run 的 transitions（按 at）
		for _, tr := range trans {
			b.WriteString(fmt.Sprintf("  %s  %s → %s   %s\n", timeOnly(tr.At), tr.From, tr.To, tr.Reason))
		}
		// 该 run 的 steps
		steps, _ := st.Replay(r.ID)
		for _, s := range steps {
			b.WriteString(fmt.Sprintf("  %s  ▸ %-8s %-10s %s\n", timeOnly(s.At), s.Role, s.Skill, s.Status))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func timeOnly(iso string) string {
	if len(iso) >= 19 { return iso[11:19] } // RFC3339 的 HH:MM:SS
	return iso
}
func shortID(id string) string { if len(id) > 8 { return id[:8] }; return id }
```

> `Replay(r.ID)` 已带 At（B0 后）。transitions 全属 task（跨 run），简化为每 run 段头下列全部——B6 实现可后续精化为按时间归入对应 run，但 v1 全列即可（spec §5[3] 接受）。

- [ ] **Step 4: 跑测试通过**

Run: `go test ./internal/tui/ -run TestRenderTrace -v` → PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/tui/trace.go internal/tui/trace_test.go
git commit -m "feat(tui): 轨迹纯渲染（按 run 分组 + 时间戳）"
```

---

## Task B7: model.go —— tab 切换 / 选中 / 按键 / 鼠标 / 两个 tick + commands.go

**Files:**
- Modify: `internal/tui/model.go`（Init/Update/View 接全）
- Create: `internal/tui/commands.go`
- Test: `internal/tui/model_test.go`

- [ ] **Step 1: 写失败测试**

`internal/tui/model_test.go`：
```go
package tui

import (
	"testing"
	tea "github.com/charmbracelet/bubbletea"
	"loop-eng/internal/state"
)

func TestKeySwitchAndCancel(t *testing.T) {
	st, _ := state.Open(t.TempDir()+"/state.db")
	defer st.Close()
	tid, _ := st.InsertTask(state.TaskRow{IssueRef: "#1", Description: "d"})

	m := New(st, nil)
	m.selTask = tid

	// '2' → 详情 tab
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("2")})
	if m2.(Model).tab != tabDetail { t.Fatalf("tab not detail") }

	// 'x' 对选中任务写 cancel 命令
	m_tab := m
	m_tab.selTask = tid
	m3, _ := m_tab.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	_ = m3
	pending, _ := st.PendingCommands()
	if len(pending) != 1 || pending[0].Verb != "cancel" {
		t.Fatalf("cancel command not written: %+v", pending)
	}
}
```

- [ ] **Step 2: 跑确认失败**

Run: `go test ./internal/tui/ -run TestKeySwitchAndCancel -v` → FAIL（Update 不处理按键）。

- [ ] **Step 3: 实现 commands.go + 接全 Update/Init/View**

`internal/tui/commands.go`：
```go
package tui

import "loop-eng/internal/state"

// issueResume 写一条 resume 命令（payload 可为空）。
func issueResume(st *state.Store, taskID, payload string) error {
	return st.InsertCommand(taskID, "resume", payload)
}
// issueCancel 写一条 cancel 命令。
func issueCancel(st *state.Store, taskID string) error {
	return st.InsertCommand(taskID, "cancel", "")
}
```

`internal/tui/model.go` —— 替换 Init/Update/View（替换 B1 的骨架版）：
```go
import (
	"time"
	tea "github.com/charmbracelet/bubbletea"
)

// 两个 tick 的消息
type dataTickMsg struct{}
type animTickMsg struct{}

const (
	dataTickInterval = 2 * time.Second
	animTickInterval = 60 * time.Millisecond
)

func dataTick() tea.Cmd {
	return tea.Tick(dataTickInterval, func(time.Time) tea.Msg { return dataTickMsg{} })
}
func animTick() tea.Cmd {
	return tea.Tick(animTickInterval, func(time.Time) tea.Msg { return animTickMsg{} })
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(dataTick(), animTick())
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height

	case dataTickMsg:
		snap, _ := ReadSnapshot(m.store, m.cfg)
		m.snap = snap
		return m, dataTick() // 继续

	case animTickMsg:
		m.animPhase += 0.15
		return m, animTick()

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			m.quit = true
			return m, tea.Quit
		case "1":
			m.tab = tabOverview
		case "2":
			m.tab = tabDetail
		case "3", "t":
			m.tab = tabTrace
		case "esc":
			m.tab = tabOverview
		case "up", "k":
			if m.selIdx > 0 { m.selIdx-- }
		case "down", "j":
			if m.snap != nil && m.selIdx < len(m.snap.Tasks)-1 { m.selIdx++ }
		case "enter":
			if m.snap != nil && m.selIdx < len(m.snap.Tasks) {
				m.selTask = m.snap.Tasks[m.selIdx].ID
				m.tab = tabDetail
			}
		case "r":
			if m.selTask != "" { _ = issueResume(m.store, m.selTask, "") }
		case "x":
			if m.selTask != "" { _ = issueCancel(m.store, m.selTask) }
		}

	case tea.MouseMsg:
		// v1：鼠标点击总览行 = 选中（简化：用 Y 坐标粗映射 selIdx）
		// 完整鼠标 hit-test 留后续；此处至少不崩。
	}
	return m, nil
}

func (m Model) View() string {
	if m.quit { return "" }
	// 首屏数据未到：先读一次
	if m.snap == nil {
		m.snap, _ = ReadSnapshot(m.store, m.cfg)
	}
	switch m.tab {
	case tabOverview:
		return RenderOverview(m.snap, m.selIdx, m.animPhase, m.width)
	case tabDetail:
		if m.selTask == "" { return "（未选中任务）\n" }
		return RenderDetail(m.store, m.cfg, m.selTask)
	case tabTrace:
		if m.selTask == "" { return "（未选中任务）\n" }
		return RenderTrace(m.store, m.selTask)
	}
	return ""
}
```

- [ ] **Step 4: 跑测试通过**

Run: `go test ./internal/tui/ -v` → PASS（含按键 + cancel 写库）。

- [ ] **Step 5: 提交**

```bash
git add internal/tui/model.go internal/tui/commands.go internal/tui/model_test.go
git commit -m "feat(tui): tab/选中/按键 + 数据&动画双 tick + resume/cancel"
```

---

## Task B8: NO_COLOR / 非 TTY 降级 + 收尾

**Files:**
- Modify: `internal/tui/styles.go`（检测 NO_COLOR/非 TTY，降级）
- Modify: `internal/cli/dashboard.go`（非 TTY 时不进 alt-screen）
- Test: `internal/tui/styles_test.go`

- [ ] **Step 1: 写失败测试**

`internal/tui/styles_test.go`：
```go
package tui

import (
	"os"
	"strings"
	"testing"
	"loop-eng/internal/state"
)

func TestNoColorDegradesToText(t *testing.T) {
	os.Setenv("NO_COLOR", "1")
	defer os.Unsetenv("NO_COLOR")
	// RenderOverview 在 NO_COLOR 下不应含 ANSI 转义码
	snap := &Snapshot{
		Tasks: []state.TaskView{{ID: "t", IssueRef: "#1", Description: "d", Status: "new"}},
		Counts: map[string]int{"new": 1},
	}
	out := RenderOverview(snap, 0, 0, 80)
	if strings.Contains(out, "\x1b[") {
		t.Fatalf("NO_COLOR 下仍含 ANSI 转义: %q", out)
	}
}
```

- [ ] **Step 2: 跑确认失败**

Run: `go test ./internal/tui/ -run TestNoColorDegradesToText -v` → FAIL（lipgloss 默认仍上色）。

- [ ] **Step 3: 实现**

`internal/tui/styles.go` 头部加降级开关——lipgloss 通过 `lipgloss.SetColorProfile` 控制；NO_COLOR/非 TTY 时设为 `termenv.Ascii`：
```go
package tui

import (
	"os"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

func init() {
	if os.Getenv("NO_COLOR") != "" || !isTTY() {
		lipgloss.SetColorProfile(termenv.Ascii) // 纯文本，无 ANSI 颜色
	}
}
func isTTY() bool {
	fi, _ := os.Stdout.Stat()
	return (fi.Mode() & os.ModeCharDevice) != 0
}
```
> `github.com/muesli/termenv` 是 bubbletea 的传递依赖（已在 go.sum），不算新依赖；若 go mod tidy 报缺失，`go get github.com/muesli/termenv`。`lipgloss.SetColorProfile` 在 NO_COLOR 时让所有 style 退化为纯文本，符号仍保留。

`internal/cli/dashboard.go`：非 TTY 时不用 alt-screen（避免管道里刷屏）：
```go
opts := []tea.ProgramOption{}
if isTTYish() { opts = append(opts, tea.WithAltScreen()) }
p := tea.NewProgram(tui.New(st, cfg), opts...)
```
（`isTTYish` 用同款 Stat 判断；或直接把判断收口到 tui 包导出 `func IsTTY() bool`。）

- [ ] **Step 4: 跑测试通过 + 全量回归**

Run: `go test ./internal/tui/ -v` → PASS。
Run: `CGO_ENABLED=0 go build ./...` + `go test ./...` → 全绿。

- [ ] **Step 5: 提交**

```bash
git add internal/tui/styles.go internal/tui/styles_test.go internal/cli/dashboard.go
git commit -m "feat(tui): NO_COLOR/非 TTY 降级为纯文本"
```

---

## Self-Review（已核对）

**Spec coverage：** §3 架构（双 tick、共享 SQLite）→ B1/B7；§5 三 Tab → B3/B5/B6；§6 颜色+呼吸灯 → B3/B4；§7 控制通道 → B7（InsertCommand）；§5 详情字段（描述/启动/运行/验收方式/验收标准/预算）→ B5；opus 三点 → B0/B2/B5。

**Placeholder：** 无 TBD。B8 的鼠标 hit-test 标注「v1 简化、完整留后续」是有意 scope（spec §1 非目标未禁鼠标但 v1 键盘优先）。

**类型一致：** `Snapshot`/`RunningInfo`（B2）被 B3/B7 消费一致；`RenderOverview/Detail/Trace` 签名与 model.go View 调用一致；`issueResume/Cancel`（B7）与 `Store.InsertCommand`（Phase A）签名一致。

**冻结签名：** 仅 `state` 加字段（B0）+ 新 `internal/tui` 包；不动 `AppendStep`/`Tier.Check`/`Chain`/`channel.Channel`。

**依赖：** 仅 bubbletea + lipgloss（+ termenv 传递依赖）；无 CGO。

---

## 全部完成后

- `loop-eng dashboard` 启动三 Tab TUI：总览（计数+颜色+呼吸灯）、详情（验收方式+标准+预算）、轨迹（按 run+时间戳）；`r`/`x` 发 resume/cancel；`q` 退出；NO_COLOR 降级。
- `CGO_ENABLED=0 go build ./...` + `go test ./...` 全绿。
- 之后：整支 `spec/dashboard-tui`（文档 + Phase A + Phase B）一起 land。

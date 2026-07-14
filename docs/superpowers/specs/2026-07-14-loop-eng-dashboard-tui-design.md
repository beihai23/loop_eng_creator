# loop-eng 看板 TUI（子项目 2）—— 设计规格

> 状态：初稿（2026-07-14）。本文档独立设计**子项目 2（看板 TUI）**，依赖子项目 1 已落地的 SQLite 数据层（spec §8.7）。读完应可直接进入实现计划。

## 0. 背景

子项目 1（核心循环）已落地：常驻 daemon、工单通道、完整三层验证、park/resume、预算、worktree 隔离、可回放 SQLite trace、以及一条可观测性数据层（spec §8.7，含 `in_flight` 表 + `loop-eng status --watch`）。

当前的可观测出口是 `loop-eng status --watch`：纯文本、无颜色、无交互的 2s 轮询视图。本规格把它升级成**一个真正可交互的 TUI 看板**——填上 spec/README 早已预留的 `loop-eng dashboard` 命令位。

形态决策：**TUI 优先，web 后续迭代**（与设计对话一致）。技术栈：**bubbletea + lipgloss**（与现有 cobra / modernc.org/sqlite 一样属第三方纯 Go 库，不引入 CGO）。

定位决策：**观测 + 轻量操控**——既看，也能从 TUI 对任务发 `resume` / `cancel`。不做任务创建、不做验收标准编辑（主路径仍是「建 issue → daemon 捞」）。

## 1. 目标 / 非目标

**目标：**
- 一个 `loop-eng dashboard` TUI：三 Tab（总览 / 详情 / 轨迹），真彩色，鼠标 + 键盘双操。
- 待处理任务计数、按状态着色、进行中任务呼吸灯。
- 点选/选中任务 → 全屏详情（描述、启动时间、运行时长、验收方式、验收标准、预算、retry）。
- 全屏轨迹时间线（= 可视化的 `loop-eng replay`）。
- 轻量操控：`resume`（带反馈文本，重入 FIFO）、`cancel`（终态）。
- 顺带补全 `runs` 表（修一个潜伏 bug，详见 §4.1）。

**非目标（YAGNI 红线）：**
- ❌ web 看板（spec 已列「后续」）。
- ❌ 从 TUI 建任务 / 改验收标准 / 手动派活。
- ❌ 并发执行（单活跃不动，spec §12）。
- ❌ 硬杀进程（cancel 协作式、phase 边界生效，见 §7）。
- ❌ 替换 `loop-eng status` / `--watch`（那俩留给脚本/非交互；dashboard 是交互兄弟）。

## 2. 设计原则（对齐核心 spec）

1. **一切走 SQLite。** 控制指令也走表（新 `commands` 表），不另开 unix socket / 信号文件。和核心 spec 原则 1（「上下文是缓存，磁盘才是真相源」）一致。
2. **只读打开 DB。** TUI 以 `mode=ro` 连同一个 db 文件，绝不与 daemon 抢写。唯一例外是往 `commands` 表 `INSERT`（append-only，与 daemon 的写无竞态）。
3. **不碰冻结签名。** `channel.Channel` / `model.Client.Call` / `verify.Tier.Check` / `AppendStep` 签名不动。`AppendStep` 已接收 `StepRow`（含 `RunID` 字段），改的是传入值，不是签名。
4. **协作式取消。** 不硬杀 `RunTask`；cancel 在 SubLoop 的 phase 边界生效。
5. **纯函数渲染。** 沿用 `status.go: renderWatchView` 的模式——view 函数只吃快照结构体、吐渲染结果，不带时间/终端/DB，可单测。

## 3. 架构总览

```
  GitHub issue ──▶ daemon tick ──▶ SQLite ─────────────┐
    (建issue)       (4+N 步)       tasks / task_status  │
                                      runs / in_flight   │ read (展示)
                                      steps / verify     │
                                      budget_ledger      │
                                      commands ◀─────────┤
                          ▲                              ▼
                          │ drain (step 3.5)       ┌──────────┐
                      commands ◀──────────────── │   TUI    │
                    (resume/cancel)               │ bubbletea │
                                                   │ 3 tabs   │
                                                   │ 颜色/呼吸 │
                                                   └──────────┘
```

**两个独立的时间域**（bubbletea 的 message/cmd 模型天然支持）：

| tick | 周期 | 职责 |
|---|---|---|
| 数据 tick | ~2s | 重读 `state.Store`，刷新列表/详情/轨迹快照 |
| 动画 tick | ~60ms | 只重绘进行中任务的呼吸灯，**不读 DB** |

单活跃（spec §12）⇒ 全局最多一个进行中任务 ⇒ 最多一盏呼吸灯，动画 tick 近乎零负担。

**只读打开 DB：** `sql.Open("sqlite", "file:"+path+"?mode=ro")`（modernc.org/sqlite 支持查询参数）。命令退出即关。

## 4. 数据层变更（实现 TUI 前的前置项）

### 4.1 补全 `runs`（修空壳 + 修 `run_id=taskID` 潜伏 bug）

**现状问题：**
- `runs` 表（`id, task_id, started_at, ended_at, outcome, total_tokens, retry_count`）只有 schema，**全仓库无任何 `INSERT INTO runs`**——永远是空壳。
- SubLoop 把 `taskID` 当 `run_id` 用（`AppendStep{RunID: taskID}`、`AppendBudget(taskID, …)`），而 `attempt` 每次 `Run` 都从 1 重计（`for attempt := 1; …`），`seq = attempt*10+N` 随之每次重置。
- park/resume 是真实路径 ⇒ 一个任务会被 dispatch 多次 = 多次 `SubLoop.Run`。第二次 run 的 steps 会产出与第一次**相同的 seq**（都从 11,12,13 起），且 `run_id` 都是 `taskID`。`Replay(runID)` 是 `WHERE run_id=? ORDER BY seq` ⇒ 两次 run 的 step **交错混排**，replay 一旦任务被 resume 过就糊掉。

**修法（补全，不删表）：**
- 每次 `SubLoop.Run` 入口调 `Store.StartRun(taskID) → runID`，新建一行 run（新 id、`started_at=now`、`ended_at=NULL`）。
- `AppendStep` / `AppendBudget` 改传**真 `runID`**（在 Run 入口取得，attempt 循环内可见），不再传 `taskID`。
- 每个 `return` 终态路径（done / blocked / needs-review / cancelled / error）调 `Store.EndRun(runID, outcome, retryCount)`，写 `ended_at / outcome / retry_count`。
- `total_tokens`：best-effort。若 `budget.Enforcer` 已聚合本轮花费则写入，否则置 0、由 TUI 从 `steps` 求和展示（不影响正确性）。

**结果：** `Replay(runID)` 恢复语义、TUI 轨迹按 run 干净分组、潜伏 bug 消失。`runs.id` 形如 `run_<hex>`（`newID("run")` 生成），`replay --run <run_id>` 仍可用（且现在真的按单次 run 回放）。

### 4.2 新表 `commands`（控制通道）

```sql
CREATE TABLE IF NOT EXISTS commands(
    id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL,
    verb TEXT NOT NULL,        -- resume | cancel
    payload TEXT,              -- resume 附带的反馈文本（可空）
    created_at TEXT NOT NULL,
    applied_at TEXT);          -- NULL = 待处理
CREATE INDEX IF NOT EXISTS idx_commands_pending ON commands(applied_at);
```

append-only。TUI `INSERT`（`applied_at=NULL`）；daemon `drainCommands` 取 `applied_at IS NULL` 行、应用、回写 `applied_at`。

### 4.3 新终态 `cancelled`

- `cancel` 引入终态 `cancelled`。
- `NextReadyTask` 只捞 `new`（不变）⇒ `cancelled` 自然不会被再 dispatch。
- `reconcile` 把 `cancelled` 与 `done`/`blocked` 同列为终态（不做 channel 反转处理，除非人在 issue 上 reopen——届时走现有 done-reopen 逻辑，可后续补，v1 不强求）。
- TUI 颜色映射新增 `cancelled`（暗灰 ✘，见 §6）。

### 4.4 新增 `state.Store` 方法

**写侧（TUI + daemon + SubLoop）：**
- `InsertCommand(taskID, verb, payload string) error` —— TUI 发指令。
- `StartRun(taskID string) (runID string, err error)` —— SubLoop 入口。
- `EndRun(runID, outcome string, retryCount int) error` —— SubLoop 终态。
- `AppendVerification(runID string, tier int, passed bool, detail string) error` —— SubLoop 逐 tier 落盘（§4.6）。
- `MarkCommandApplied(cmdID string) error` —— daemon drain 回写。

**读侧（daemon drain + SubLoop 协作 cancel）：**
- `PendingCommands() ([]CommandRow, error)` —— daemon drain 用（`applied_at IS NULL ORDER BY rowid`）。
- `CancelRequested(taskID string) (bool, error)` —— SubLoop phase 前自查（`verb='cancel' AND applied_at IS NULL LIMIT 1`）。

**读侧（TUI reader）：**
- `TasksByStatus() ([]TaskView, error)` —— 一次 join `tasks`+`task_status`，带回 `id/issue_ref/description/task_type/status/created_at`，避免 N+1。按「进行中→new→needs-review→blocked→done/cancelled」排序。
- `ActiveRun(taskID string) (runID, startedAt string, ok bool)` —— 进行中任务的当前 run + 启动时间（`ended_at IS NULL`）。
- `RunsOfTask(taskID string) ([]RunRow, error)` —— 轨迹按 run 分组用。
- `StepsOfTask(taskID string) ([]StepRow, error)` —— 轨迹跨 run 视图用（已有 `Replay(runID)` / `BudgetLedger(runID)` 按 run 取）。
- `VerificationsByRun(runID string) ([]VerificationRow, error)` —— 详情页逐 tier 状态（§4.6）。

### 4.5 SubLoop 改动（最小）

- Run 入口 `runID := Store.StartRun(taskID)`；所有 `AppendStep{RunID: …}` / `AppendBudget(…)` 用 `runID`。
- 每个 phase（plan / execute / verify）**之前**加一个自查：
  ```go
  if ok, _ := sl.Store.CancelRequested(taskID); ok {
      _ = sl.Store.ClearInFlight()
      return sl.report(ctx, taskID, task, "cancelled", "cancelled by TUI"), nil
  }
  ```
- 每条终态 `return` 前调 `Store.EndRun(runID, outcome, attempt)`。
- 不改任何冻结签名；`report()` 内部已有的 `AppendTransition` 自然覆盖 `running→cancelled`。

### 4.6 补全 `verifications`（同 runs，支撑详情页逐 tier 视图）

**现状问题（自审发现，与 runs 同类的空壳）：** `verifications` 表（`id, step_id, tier, passed, detail, at`）只有 schema、**零写入**。`verify.Chain` 逐 tier 求值并短路于首个失败，但只返回聚合 `VerifyResult{Passed,Detail,NeedsHuman}`，**不落盘逐 tier 结果**；验证结果目前只记在 `steps`（一行 `role=verify`，聚合）。⇒ 详情页「验收方式」想要的逐 tier 状态（tier-1 ✓ / tier-2 ⏳ / tier-3 —）无数据支撑。

**修法（补全，与 §4.1 同决策）：**
- `verify.VerifyResult` 增加字段 `Tiers []TierOutcome`（每个：`Tier int / Passed bool / NeedsHuman bool / Detail string`），由 `Chain` 求值时按序填入——短路于首个 `!Passed`，未到达的 tier 不进切片（= 详情页「未触发」）。**`Chain` 签名不变、`Tier.Check` 冻结签名不动**（仅 struct 加字段，向后兼容）。
- `verifications` 表经 best-effort `ALTER TABLE verifications ADD COLUMN run_id TEXT` 挂到 run（照搬 `state.go` 里 `last_comment_at` 的迁移手法）；`step_id` 保留可空（legacy，不再作关联键）。
- SubLoop 在 verify 步骤后遍历 `res.Tiers`，逐条 `Store.AppendVerification(runID, tier, passed, detail)`。
- 详情页逐 tier 行：tier 标签来自 config（tier-1 固定 `go test` / tier-2 `Models.Verify` / tier-3 `Verify.Tier3Human`），实时状态来自 `verifications`（按 run + tier）；verify 进行中且尚未落盘时显示 ⏳。

**新增 Store 方法：**
- 写：`AppendVerification(runID string, tier int, passed bool, detail string) error`
- 读：`VerificationsByRun(runID string) ([]VerificationRow, error)`

## 5. 三个 Tab

### [1] 总览（默认页）
```
┌ loop-eng —— 待处理 3 · 进行中 1 ● · 等人审 1 · 阻塞 2 · 完成 5 ────┐
│                                                                     │
│  ▸ #18 给 budget 加硬上限    ●  verify     12m03s   ← 呼吸灯       │
│    #19 修 retry 退避          ◌  new                                │
│    #20 接线日志               ◌  new                                │
│    #21 docs: TUI 说明         ⏸  needs-review                       │
│    #22 gh TLS 重试            ✗  blocked                            │
│    …已完成 5 条（折叠）        ✓                                     │
│                                                                     │
│  ↑↓ 选  Enter 详情  t 轨迹  r resume  x cancel  q 退出             │
└─────────────────────────────────────────────────────────────────────┘
```
- 顶部计数条：`待处理=count(status=new)`、`进行中=ActiveRun 非空时 1`、各状态计数实时随数据 tick 更新。
- 排序：进行中置顶 → new → needs-review → blocked → done/cancelled（折叠/暗色）。
- 鼠标点击 = 选中；双击 = 进详情。

### [2] 详情（选中任务回车进入，全屏）
```
┌ #18 给 budget 加硬上限 ───────────────────────────────────────────┐
│ issue: beihai23/loop-eng#18   type: feature   状态: 进行中 ●        │
│ phase: verify (retry 1/3)          启动: 14:02:11 · 已运行 12m03s   │
│ ───────────────────────────────────────────────────────────────── │
│ 验收方式:                                                          │
│   tier-1  go test ./...        ✓ passed                           │
│   tier-2  glm-5.2 (LLM diff)   ⏳ running                          │
│   tier-3  人审 (issue 评论)    — 未触发                            │
│ 验收标准:                                                          │
│   • 命中上限立即停                                                 │
│   • 落盘一行 budget_ledger                                         │
│   • 不改 AppendStep 签名                                           │
│ 预算: 38.2k / 100k tokens   retry: 1                               │
│ ───────────────────────────────────────────────────────────────── │
│ [r] resume   [x] cancel   [t] 看轨迹   [Esc] 回总览               │
└─────────────────────────────────────────────────────────────────────┘
```

**字段 → 数据源映射：**

| 面板字段 | 来源 |
|---|---|
| 基本描述 | `tasks.description` |
| issue / type | `tasks.issue_ref` / `tasks.task_type` |
| 状态 / phase | `task_status.status` / `in_flight.phase` |
| 启动时间 | `runs.started_at`（当前活跃 run，§4.1 补全后可用） |
| 运行时长 | 进行中：`now − started_at`；已完成：`ended_at − started_at` |
| 验收方式 | tier 标签来自 `config`（tier-1 固定 `go test ./...` / tier-2 = `config.Models.Verify` / tier-3 由 `config.Verify.Tier3Human` 开关）；逐 tier 实时状态（✓/⏳/✗/—）来自 `verifications` 表（§4.6 补全后按 run+tier 取） |
| 验收标准 | `tasks.acceptance_criteria_json` → `[]string` |
| 预算 | `budget_ledger` 求和 vs 配置上限 |
| retry | `budget_ledger` 中 `kind='retry'` 的 amount，或 `runs.retry_count` |

### [3] 轨迹（全屏时间线）
```
┌ #18 轨迹 ───────────────────────────────────────────────────────┐
│ run 2 (14:02 → 进行中)                                            │
│ 14:02:11  new → running     dispatched                           │
│ 14:02:12  ▸ plan            glm-5.2   4.1k tok   ✓               │
│ 14:05:33  ▸ execute         claude-p  31k tok   ✓                │
│ 14:14:01  ▸ verify tier-1   go test              ✓ passed        │
│ 14:14:10  ▸ verify tier-2   glm-5.2   2.2k tok   ⏳ …             │
│ 14:08:22  retry 1/3         budget brake: per-call tokens        │
│ ─────────────────────────────────────────────────────────────── │
│ run 1 (07-12 09:11 → needs-review，已 park)                       │
│ 09:11:02  …                                                      │
└───────────────────────────────────────────────────────────────────┘
```
- 按 `RunsOfTask` 分组；组内合并 `transitions` + `steps`（含 `at`）+ `verifications` + `budget_ledger`，按时间排序。
- `seq = attempt*10+N` 在单 run 内把同 attempt 的 plan/execute/verify 归为一组。

## 6. 颜色 / 状态 / 呼吸灯

| 状态 | 符号 | 颜色 | 动效 |
|---|---|---|---|
| `new` 待处理 | ◌ | 暗灰 | — |
| `running` 进行中 | ● | **亮绿** | **呼吸灯** |
| `needs-review` 等人审 | ⏸ | 琥珀黄 | — |
| `needs-info` 待补信息 | ℹ | 蓝 | — |
| `blocked` 阻塞 | ✗ | 红 | — |
| `done` 完成 | ✓ | 暗青 | — |
| `cancelled` 已取消 | ✘ | 暗灰 | — |

**呼吸灯**（仅进行中那一盏）：动画 tick 每 ~60ms 发一个 `animMsg{t}`；view 里 `brightness = (sin(t/600ms)+1)/2` 在两个绿色 lipgloss style 间插值。单活跃 ⇒ 全局一盏，无性能顾虑。

**降级：** 检测 `NO_COLOR` / 非 TTY（`!isatty`）时，关闭颜色与动画，退化为「符号 + 纯文本」，与现有 `status` 行为一致（脚本可读、可重定向）。

## 7. 控制通道（resume / cancel）

**daemon tick 新增第 3.5 步 `drainCommands`**（插在 `pollSignals` 与 `dispatch` 之间）：
```go
// ---- step 3.5: drain TUI commands ----
if err := e.drainCommands(ctx); err != nil {
    e.logf("[daemon] drain commands error: %v", err)
}
```

**两动词语义（刻意只两个，YAGNI）：**

| 动词 | 适用状态 | 效果 |
|---|---|---|
| `resume` | `needs-review` / `blocked` | `X → new` + `SetResumeFeedback(payload)`，重入 FIFO。**与 pollSignals 的人审回复同路**——TUI 只是「替你把反馈贴进来」。UI 上 parked 显「resume」、blocked 显「retry」，底层都是 `resume`。 |
| `cancel` | 任何状态 | → 终态 `cancelled`。 |

**取消「正在跑」的任务（协作式）：**
- **非 running**（new/parked/blocked）：`drainCommands` 直接 `X → cancelled`、回写 `applied_at`，立竿见影。
- **running**：cancel 命令躺在 `commands` 表；daemon 正阻塞在 `RunTask`，drain 跑不到。**SubLoop 在每个 phase 边界自查 `CancelRequested`**，看到就提前 return `cancelled`（§4.5）。daemon 随后的 drain 把该命令标记 `applied_at`（幂等：任务已终态则只回写、不重复 transition）。

**明示的代价：** cancel 不硬杀，**在下一个 phase 边界生效**——若 `execute`（claude-p）正跑 3 分钟，需等它这一跑结束。这是协作式的本质，符合「不改 `RunTaskFunc` 契约 + 不做 worktree 强清理」的 YAGNI 取舍。

## 8. 组件结构

新包 `internal/tui/`，严格沿用 `renderWatchView` 的纯函数渲染模式：

```
internal/tui/
  model.go       bubbletea Model（当前 tab / 选中任务 / 动画相位 / 最新快照）
  overview.go    [1] 纯渲染：snapshot → lipgloss 块
  detail.go      [2] 纯渲染
  trace.go       [3] 纯渲染
  reader.go      读 state.Store + config → 组 snapshot（TasksByStatus/ActiveRun/RunsOfTask/…）
  commands.go    写 commands 表（resume/cancel）
  animate.go     呼吸灯相位 → 绿色明度（纯函数，注入 t 可断言）
internal/cli/
  dashboard.go   cobra 命令（只读开 DB、起 bubbletea 程序、注册到 root）
改动：
  internal/state/state.go       +commands 表 schema +verifications 加 run_id 列 +cancelled 终态 +§4.4 全部方法 +runs/verifications 补全
  internal/verify/verify.go     VerifyResult +Tiers 字段；chain.go 求值时填充逐 tier 结果
  internal/daemon/engine.go     +drainCommands 步骤（step 3.5）
  internal/loop/subloop.go      +StartRun/EndRun 透传 runID +verify 后逐 tier AppendVerification +phase 前 CancelRequested 自查
  internal/cli/replay.go        （语义随 runs 补全自动变正确，无需改）
```

## 9. 测试策略（全部不带真终端 / 真时间）

- **纯 view 函数**（`overview/detail/trace`）：注入 snapshot 结构体，断言渲染字符串/style。照搬 `status_test.go` 对 `renderWatchView` 的做法。
- **reader**：临时 SQLite 灌 fixture（多 run、跨 resume），断言 `ActiveRun`/`RunsOfTask`/`TasksByStatus` 正确；断言从 `config` + `verifications` 派生「验收方式」正确。
- **commands 写 + drain**：`InsertCommand` 正确性；`drainCommands` 把 `resume`/`cancel` 翻成正确 transition 并回写 `applied_at`（注入 fake，照搬现有 daemon 测试）；幂等性（终态任务再 drain 不重复 transition）。
- **runs 补全**：`StartRun`→`EndRun` 往返；`Replay(runID)` 在「同任务两次 run」fixture 下不再交错（**回归测试，钉死 §4.1 的 bug**）。
- **verifications 补全**：`Chain` 返回的 `Tiers` 与落盘的 `verifications` 行一致（tier/passed/detail）；短路于首个失败时，未到达的 tier 不落盘（=「未触发」）；详情页 reader 按 run+tier 还原逐 tier 状态正确。
- **协作 cancel**：SubLoop 预置一条 pending cancel，断言下个 phase 边界提前 return `cancelled` + `EndRun` 已写。
- **animate**：给定注入 `t`，断言相位→预期明度（确定性）。
- **构建约束**：`CGO_ENABLED=0 go build ./...` 成功；`go test ./...` 全绿（含现有 M1/M2/e2e）。

## 10. 验收标准

1. `loop-eng dashboard` 启动一个 bubbletea TUI，三 Tab 可切换（`1/2/3` 或鼠标），`q` 退出。
2. 总览页顶部计数条正确反映各状态任务数（用 fixture 验证）；进行中任务显示绿色呼吸灯。
3. 选中任务回车 → 详情页展示：描述 / issue / type / 状态+phase / **启动时间** / **运行时长** / **验收方式（tier-1/2/3 三行）** / **验收标准** / 预算 / retry，字段全部有据可查。
4. 轨迹页按 run 分组展示时间线；**同任务经一次 resume 后产生两 run，两组 step 不交错**（回归 §4.1）。
5. `r`（resume）对 parked/blocked 任务：写 `commands(resume)` → daemon 下个 tick drain → 任务回 `new` 并带反馈；详情页可见反馈进入下一轮。
6. `x`（cancel）对非 running 任务：drain 后 → `cancelled`；对 running 任务：下个 phase 边界 → SubLoop 自查 cancel → `cancelled`。
7. `runs` 与 `verifications` 表不再为空：每个 dispatch 一行 run（`started_at/ended_at/outcome/retry_count` 齐全），每次 verify 逐 tier 一行 verifications（短路于首个失败，未到 tier 不落盘）；`replay --run <run_id>` 按单次 run 正确回放；详情页 tier-1/2/3 状态全部有据可查。
8. `NO_COLOR` / 非 TTY 下退化为符号+纯文本，无颜色无动画，可重定向。
9. 不引入 CGO；不碰冻结签名（`channel.Channel` / `model.Client.Call` / `verify.Tier.Check` / `AppendStep`）。
10. `CGO_ENABLED=0 go build ./...` 成功；`go test ./...` 全绿。

## 11. 与核心 spec 的关系 / 后续

- 本规格 = 核心 spec（2026-07-02）「子项目 2」。数据层依赖 §8.7（已落地）。
- §4.1 补全 `runs` 既是本规格的前置项，也顺手修了核心 spec 遗留的一个潜伏 bug（属合理范围内的数据层夯实，非无关重构）。
- **后续（不在本规格）：** web 看板；硬杀式 cancel（需 `RunTaskFunc` 加 ctx 取消 + worktree 清理）；多仓库 worktree 扇出；TUI 内建任务/改标准。

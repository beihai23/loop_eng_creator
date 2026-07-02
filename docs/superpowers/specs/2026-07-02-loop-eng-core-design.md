# loop-eng 核心（子项目 1）—— 设计规格

- **日期：** 2026-07-02
- **状态：** 草稿 v2（并入 daemon + 工单通道），待用户评审
- **范围：** `loop-eng` v1 路线图的子项目 1（核心地基）
- **实现语言：** Go

本规格**只设计子项目 1**。看板 TUI（子项目 2）后续单独出规格。

---

## 1. 背景与目标

`loop-eng` 是一个个人的 loop engineering 工具，落地两篇源文章里的反馈控制环方法论：一个**控制器是 LLM** 的控制环，以及由此引出的三项适配——**独立验证、准确的现状表征、状态落盘**。

`loop-eng` 是**常驻 CLI + 运行时**（「B」形态）：工具拥有 loop 的编排代码；用户拥有易变的部分（skill、验证脚本、配置）。用户装一次工具，每个项目配一次，然后拿它去跑开发任务。

**关键架构事实：主循环是一个常驻 daemon。** 它是串级控制里的慢环——管全局状态、派子 loop、收结果、并在**不阻塞**的情况下等人（人审、求助）。因为 daemon 同时要照管多个子 loop，任何「等人」都不能卡住它：tier-3 人审必须**异步**，通过工单系统的评论进行——发 review-request、把任务 park 起来、释放子 loop 槽位、daemon 去干别的；轮询到人的回复后再恢复该任务。

**v1 工单系统 = GitHub Issue。** issue 就是任务，issue 的评论就是战报 + 人审通道。工单通道做成**可插拔接口**，Jira/Linear 后续走同一接口。

**子项目 1 目标（地基）：** 一个常驻的 loop 引擎，能轮询 GitHub Issue 捞任务、并发跑多个子循环（计划 → 执行 → 验证 → 写回，完整三层验证）、tier-3 异步人审经 issue 评论、强制预算、在 worktree 里隔离执行、落盘一份可回放的 trace、并路由结果。看板 TUI 是子项目 2。

---

## 2. v1 路线图（背景）

| # | 子项目 | 本规格？ |
|---|---|---|
| 1 | 核心循环 + 完整三层验证 + 可回放状态 + 预算 + worktree 隔离 + **常驻 daemon + 工单通道（战报 + 异步人审）+ 并发子循环** + 可观测性数据层 | ✅ |
| 2 | 看板 TUI（读 #1 的 SQLite） | 后续 |

建这个顺序的理由：验证是生死线，必须先单独建稳；而 **tier-3 要异步非阻塞，就非得靠常驻 daemon + 工单通道**——所以它们是地基的一部分，不是后加的层。可观测性的**数据层**归 #1（从第一天起每次状态变迁都是一行）；它的**展示层**（TUI）是 #2。

> **关于范围：** 地基不小，但**建的时候可以在实现计划里分里程碑**（如 M1 核心子循环 + tier1/2 + 工单接口 stub → M2 真工单通道 → M3 daemon + 并发 + 异步 tier-3）。**spec 覆盖完整地基，交付的 v1 地基一样不少**——分里程碑是建序，不是降标准。

---

## 3. 不可妥协的原则（来自文章）

这些是硬规则。任何违反其中一条的设计选择都是错的。

1. **上下文是缓存，不是真相源。** 任何必须跨轮存活的东西，在当轮结束前就落盘到 SQLite/磁盘。没有任何跨轮的东西只活在 LLM 上下文里。*「LLM 的记忆只认落盘那份。上下文里的，按零算。」*
2. **验证独立于执行。** 独立性来自**不共享上下文/信息**，不只是「换个模型」。验证端只读落盘的 diff + 验收标准，永远不读执行的推理过程。
3. **自报是信号，不是结论。** 执行端说「我做完了、标准满足了」只是触发验证。结论永远由独立的验证层下。
4. **每一轮主动喂准现状。** 控制器只看得见我们喂给它的文字。子循环每轮开始时**重新读**落盘状态，绝不信任过期的叙事。
5. **三道预算刹车从第一天起就有**——每调用 token 上限、每任务 token 上限、最大重试次数。没有这三样的 loop 就是烧钱。
6. **skill 是非确定性代码。** 无状态、职责固定、输入输出契约固定。要版本化、要有回归集。
7. **主循环 vs 子循环（串级控制）。** 主循环（daemon）是常驻的慢环，管全局状态与派发，**永远不阻塞在人上**；子循环为一个任务而生、跑完把结果交回、然后销毁。子循环的结果被主循环**摄取**。

---

## 4. 范围

**在范围内（子项目 1 地基）：**
- 常驻 daemon 引擎：轮询 GitHub Issue 捞任务 + 捞人审回复；并发派子循环；收回结果、路由、把战报写成评论
- 主循环（daemon tick）：捞取 → 门禁 → 派发 → 路由
- 子循环编排：计划 → 执行 → 验证 → 写回，带重试；tier-3 时 park 并释放槽位
- 完整三层验证链（确定性 → LLM 新鲜上下文 → **异步**人审）
- 4 个 skill（含初稿 prompt）：`triage`、`plan`、`verify`、`help`（执行直接用 `claude`，不造 skill）
- 两条模型集成路径：直连 API（triage、plan）和 `claude -p`（execute、verify-tier2）
- 可插拔工单通道接口 + v1 实现 `githubChannel`（轮询）
- 可回放的 SQLite trace（可观测性数据层）+ 任务生命周期/队列状态
- 预算强制（三道刹车）+ 只追加的账本
- 执行的 worktree 隔离
- skill 回归测试框架
- 崩溃/重启可恢复

**不在范围内（后续）：**
- 看板 TUI（子项目 2）
- web 看板、多 loop 协调、多仓库 worktree 扇出、Jira/Linear 通道、webhook 接入、skill 通过 git 自动版本化

---

## 5. 所有权分界线（「B」契约）

| 工具拥有（稳定内核；只在升级时动） | 用户拥有（易变；用户来调） |
|---|---|
| daemon 引擎、主循环派发、子循环骨架、状态读写、预算强制、验证串联、模型客户端、工单通道客户端 | skill 的 prompt 文件（`.loop/skills/*.md`）、验证脚本（在 config 里声明）、配置参数、验收标准、issue 内容本身 |

内置 skill 默认值通过 `go:embed` 打进二进制；`.loop/skills/` 里任何同名文件覆盖内置默认。所以「把 loop engineering 当代码调」整件事都发生在分界线用户那一侧，且能在工具升级后存活。

---

## 6. 架构

### 6.1 组件图

```
cmd/loop-eng/main.go            入口
internal/
  cli/        cobra 命令：root, init, daemon, status, replay, task, skill, config
  config/     加载 + 校验 .loop/config.yaml
  daemon/     常驻引擎：tick 循环、并发槽管理、park/resume、reap
  loop/
    mainloop.go    daemon tick 里的一次调度逻辑（捞取→门禁→派发→路由）
    subloop.go     计划 → 执行 → 验证 → 写回 （单任务，带重试；tier-3 时 park）
  channel/    工单通道接口 + github/github.go（go-github，轮询）
  skill/      skill 注册表、模板渲染、I/O 序列化/反序列化、回归
  model/      两个客户端：apiClient（anthropic SDK）+ claudeClient（os/exec 调 `claude -p`）
  verify/     chain.go, deterministic.go, llm.go, human.go（human.go 异步 park）
  state/      SQLite 存储（modernc.org/sqlite）、schema、回放读取器
  budget/     每调用 / 每任务 / 重试 强制 + 账本
  isolation/  worktree 创建 / 提交 / 丢弃
  embed/skills/*.md   内置 skill 默认值（go:embed）
```

每个包只有一个清晰职责，通过类型化接口通信，可独立单测。

### 6.2 关键接口（职责边界）

- `channel.Channel` ——
  `ListNewTasks(ctx) ([]Task, error)`
  `ListReplies(ctx, taskRefs []string) (map[string][]Reply, error)`
  `PostComment(ctx, taskRef, body) error`
  `UpdateStatus(ctx, taskRef, status) error`
  `CreateTask(ctx, desc, criteria) (taskRef, error)` （仅 `task new` 助手用）
  v1 实现：`githubChannel`（按标签 `loop:task` 过滤 issue；轮询）。
- `model.Client` —— `Call(ctx, prompt, schema) (output, usage, err)`。两个实现（`apiClient`、`claudeClient`）；循环只依赖接口，测试时注入 stub。
- `skill.Skill` —— `{Name, Version, Render(input) (prompt string), Parse(output) (typed, err)}`。无状态。
- `verify.Tier` —— `Check(ctx, diff, criteria, priorFailure) (result, err)`。tier-3 的实现不阻塞：返回 `needs-human`，由 daemon park。
- `state.Store` —— 只追加写入器 + 回放读取器 + 任务生命周期读写。trace 行**禁止原地改写**。
- `budget.Enforcer` —— `BeforeCall(usageEstimate) error`、`AfterCall(usage)`、`ShouldRetry(attempt) bool`。
- `daemon.Engine` —— `Run(ctx)` 常驻；管理并发槽与 parked 任务集合。

---

## 7. 数据流 —— daemon 的运行模型

### 7.1 daemon tick（每个 `poll_interval` 跑一次）

```
loop-eng daemon  （常驻）
  每次 tick：
    1. 同步工单：channel.ListNewTasks() → 把新任务-issue 摄入（status=new）
    2. 查 parked 任务的人审回复：channel.ListReplies(parked 任务)
       └─ 每条回复 → 把该任务标为「待恢复」，带上人的反馈
    3. 对每个可调度的任务（new 或 待恢复），只要还有空闲并发槽：
       └─ 派一个子循环（status=running，占一个槽）
    4. 收割已结束的子循环 → 路由结果、把战报写成评论、更新 status、释放槽
```

### 7.2 子循环（每个任务一份，并发跑）

```
第 n 轮：
  a. 计划   ── plan skill（直连 API）：读落盘状态 + 任务 + 标准 → 执行计划（不写代码）
  b. 执行   ── `claude -p`，在 worktree 里：计划 + 任务 + 标准 + 仓库路径 → diff + 自报（信号）
  c. 验证   ── 三层链，按序：
               tier 1  确定性脚本（config）              ─ 不过 → 反馈
               tier 2  `claude -p` 新鲜会话：只给 diff + 标准 ─ 不过 → 反馈
               tier 3  人审（异步）                       ─ 见下
             tier 1/2 全过 → 写回 → done
  d. 写回   ── 落盘状态（SQLite trace）+ 战报（issue 评论）+ 把结果摄取回主循环

  tier 1/2 不过：失败作为下一轮 计划 的输入；重试（≤ max_retries）
  连续 3 次失败 或 预算耗尽：→ blocked → help skill → 写 help_request 评论 → 结束

  tier 3（人审）：
     → channel.PostComment(review-request：diff 摘要 + 验收标准 + 问什么)
     → status = needs-review
     → park：释放子循环槽，子循环结束（不阻塞 daemon）
     [等人在 issue 上回复]
     daemon 下一次 tick 的第 2 步发现回复 → 带反馈恢复该任务（回到第 a 步）
     人 accept → 写回 → done；人 reject/给反馈 → 带反馈重试
```

**parked 任务不占并发槽**——这是「daemon 等人时不退出、还能照管别的子 loop」的实现关键。每个编号步骤、每次状态变迁，都在继续往下之前**先**写一行到 SQLite，所以 daemon 重启后可从落盘状态恢复。

---

## 8. 组件细节

### 8.1 CLI 命令面（v1）

| 命令 | 用途 |
|---|---|
| `loop-eng init` | 在当前仓库生成 `.loop/`：写 `config.yaml`、拷贝默认 skill、建空 `state.db`、设 worktree 基目录、引导 GitHub 认证。**「搭建」这一步。** |
| `loop-eng daemon` | 启动常驻 loop 引擎（轮询 issue、派子 loop、路由、写战报）。**「实践」这一步。** |
| `loop-eng task new "<desc>"` | （可选助手）用 GitHub API 建一个格式正确的 issue（设 `loop:task` 标签、解析验收标准）。不是必需——你也可以直接在 GitHub 建 issue。 |
| `loop-eng status [<issue#>]` | 从 SQLite 打印任务/run/队列状态。 |
| `loop-eng replay <run-id>` | 按 trace 逐步回放某次 run。 |
| `loop-eng skill (edit\|test) [<name>]` | 编辑 skill 覆盖 / 跑回归 fixture。 |
| `loop-eng config (get\|set\|edit)` | 查看/编辑配置。 |

`loop-eng dashboard` 预留给子项目 2。主路径是「你建 issue → daemon 捞」，不是 CLI 派活。

### 8.2 配置（`.loop/config.yaml`）

```yaml
channel:
  provider: github
  repo: owner/name
  task_label: "loop:task"        # 带这个标签的 issue 才被当作任务
  auth_token_env: LOOP_ENG_GITHUB_TOKEN
daemon:
  poll_interval: 60s
  concurrency: 2                 # 并发子循环槽；parked 任务不占槽
models:
  triage: { provider: anthropic, name: claude-haiku-4-5 }   # 小/便宜/快
  plan:   { provider: anthropic, name: claude-sonnet-5 }
  execute: { via: claude-p, binary: claude }
  verify:  { via: claude-p, binary: claude }                 # 每次调用开新会话
budget:
  per_call_tokens: 20000
  per_task_tokens: 200000
  max_retries: 3
verify:
  deterministic:
    - { label: tests, cmd: ["pytest", "-q"] }
    - { label: types, cmd: ["mypy", "src/"] }
    - { label: lint, cmd: ["ruff", "check", "."] }
  tier3_human: true              # 关掉则跳过 tier-3（仅限你想纯自动跑时）
isolation:
  worktree: true
gate:
  rules:
    - { when: "task_type == deploy", requires: human_go }
skills:
  dir: .loop/skills              # 覆盖内置默认
```

配置加载时校验；预算三刹车的值缺失/非法、channel/daemon 必填项缺失，都是硬错误（不给静默默认值）。

### 8.3 主循环（daemon 引擎）

daemon 是常驻进程，按 `poll_interval` 跑 tick（见 §7.1）。每次 tick 的调度逻辑：

1. **捞取**：`channel.ListNewTasks()` → 摄入新任务（status=`new`）。
2. **查回复**：`channel.ListReplies(parked)` → 给有回复的 parked 任务标记「待恢复」+ 附上人的反馈。
3. **派发**：对每个可调度任务（`new` 或 待恢复），有空闲槽就派子循环（status=`running`，占槽）。
4. **收割 + 路由**：已结束的子循环 → 记录结果、写战报评论、更新 status、释放槽。
5. **门禁**：派发前对每个新任务套用 `gate.rules`（机械强制）；不通过 → `needs-human-decision`，写评论，不派。

daemon 任何阶段都不阻塞在人上（原则 7）。

### 8.4 子循环编排

计划→执行→验证→写回 的骨架，按任务跑，重试有上限。每轮开始时从落盘存储**重新读**状态（原则 4）。任务结束后子循环销毁，只留下它落盘的结果（原则 7）。tier-3 时 park 并释放槽，daemon 之后恢复。

### 8.5 skill（用户拥有，覆盖 `go:embed` 默认；v1 内置初稿）

无状态、I/O 契约固定、版本化。JSON 进出。子项目 1 **内置四个 skill 的初稿 prompt**，用户随后在 `.loop/skills/` 覆盖、用 seed 任务调。

- **triage** —— 入：`{task_description, acceptance_criteria, task_type}` → 出：`{startable, missing_info[], loop_doable, suggested_type, difficulty, needs_human_decision, reason}`
- **plan** —— 入：`{task, acceptance_criteria, battle_report(前几轮), repo_state_summary}` → 出：`{plan:[{step, files, expected}], risks[]}`（不写代码）
- **verify**（tier-2）—— 入：`{diff, acceptance_criteria, prior_failure_signal?}` → 出：`{passed, reason, failing_criteria[]}`（新鲜上下文；执行推理**绝不**在输入里）
- **help** —— 入：`{task, blocked_state, attempts_summary, last_error}` → 出：`{help_request:{stuck_at, tried[], need_from_human}}`

**执行不是 skill** —— 它直接调 `claude -p`（要用工具 / 改文件）。skill 的 I/O 是强类型 Go struct；强类型**就是**「固定 I/O 契约」的强制手段。

### 8.6 验证 —— 命门（完整三层，tier-3 异步）

对独立性的结构性强制（原则 2）：`verify` 包是一个**与执行分离的组件**，与执行**不共享**任何内存上下文。它只从落盘存储读 `(diff, acceptance_criteria)`。

- **tier 1 —— 确定性脚本。** 对 worktree 跑每条配置好的 `verify.deterministic` 命令；解析退出码 + 输出。最独立（不碰 LLM）。默认第一道筛。脚本报错（而非失败）按「失败带详情」处理，绝不按通过。
- **tier 2 —— LLM 新鲜上下文。** `claude -p` 开全新会话，只给 **diff + 验收标准**（重试时再加**上一轮的验证失败详情**——绝不是 execute/plan 的输出）。绝不给执行对话。处理脚本覆盖不到的语义标准。
- **tier 3 —— 异步人审。** 子循环不阻塞：发 review-request 评论（diff 摘要 + 验收标准 + 要人判断的点）→ 置 `needs-review` → park → 释放槽 → 子循环结束。daemon 轮询到人在该 issue 上的回复后，带反馈恢复任务。人 accept → 写回 → done；reject/反馈 → 带反馈重试。用于业务正确性、审美、外部依赖正确性这类判断。

**顺序：** tier 1 → 2 → 3。tier 1 不过就短路（不浪费 tier 2/3）。tier 1/2 任何一层不过 → 失败成为下一轮 计划 的反馈，预算内重试。**执行端的自报只触发这条链，绝不是结论**（原则 3，结构性强制：execute 的输出不是任何一层「通过」判定的输入）。

### 8.7 状态 / 落盘（可观测性数据层 + 任务生命周期）

SQLite，走 `modernc.org/sqlite`（纯 Go → 二进制全静态）。schema **只追加、为回放设计**，不只记当前状态。

表（草图）：

- `tasks(id, issue_ref, description, acceptance_criteria_json, task_type, source, created_at, updated_at)`
- `task_status(id, task_id, status, slot_id NULL, parked_detail NULL, updated_at)` —— 当前生命周期态；status ∈ `new|running|needs-review|blocked|needs-info|needs-human-decision|done|error`。**只有 `running` 占槽**。
- `runs(id, task_id, started_at, ended_at, outcome, total_tokens, retry_count)`
- `steps(id, run_id, seq, role[triage|plan|execute|verify], skill, model_ref, input_hash, input_json, output_json, tokens_in, tokens_out, status[ok|fail|blocked], error, at)`
- `verifications(id, step_id, tier[1|2|3], passed, detail, at)`
- `transitions(id, task_id, from_status, to_status, reason, at)` —— 每次状态变更一行
- `budget_ledger(id, run_id, scope[call|task], kind[tokens|retry], amount, limit, at)`
- `reports(id, task_id, round, channel_ref, at)` —— 指向 issue 评论（战报）

索引在 `(run_id, seq)`、`(task_id, at)`、`(status)`。trace 行（`steps`、`transitions`、`verifications`、`budget_ledger`）**只追加**；回放 = `SELECT … WHERE run_id=? ORDER BY seq`。战报 = issue 评论（人能直接读、直接回，不用切工具）。任务生命周期态让 daemon 重启后能重建「哪些在跑、哪些 parked、哪些待恢复」。

### 8.8 预算

三道刹车，在每次模型调用和每次重试之前强制（原则 5）：每调用 token 上限、每任务 token 上限、最大重试（默认 3）。每次检查追加到 `budget_ledger`。碰到任何一道 → 中止 → `blocked` → `help` skill → 写 help_request 评论 → park/结束。上限可配；**这三道刹车的存在本身不可配**。

### 8.9 worktree 隔离

每个子循环在一个全新 git worktree 里执行，路径 `.loop/worktrees/<run-id>/`。验证通过时，worktree 的 diff 就是产物（后续：提为 PR）。失败/中止时，worktree 分支丢弃。这是回滚原语（文章 2）：「跑飞了丢这个分支」。**parked 任务（等人）期间其 worktree 保留**，恢复时续用。

### 8.10 模型集成（两条路径）

- **直连 API**（`apiClient`，anthropic Go SDK）：`triage`、`plan`。便宜、快、结构化 JSON 输出。triage 用小模型。让重量级的 `claude` 启动不拖累轻量判断。
- **`claude -p`**（`claudeClient`，`os/exec`）：`execute`、`verify`-tier2。execute 要用工具/改文件；verify-tier2 按文章要求需要全新 Claude Code 会话。`claudeClient` 每次都开**新**会话（不共享对话）——这是验证独立性强制的一部分。

循环只依赖 `model.Client`；两个实现可换、可 stub。

### 8.11 工单通道（可插拔）

`channel.Channel` 接口（见 §6.2）。v1 实现 `githubChannel`：用 go-github，按 `task_label` 过滤 issue；轮询拿新任务和 parked 任务的回复；把战报/求助/人审请求写成评论；用 label 更新 status（如 `loop:needs-review`）。Jira/Linear 后续实现同一接口。

---

## 9. 任务与验收标准格式

任务 = 一个 GitHub Issue（带 `loop:task` 标签）。issue 正文里写明描述、类型、验收标准（约定一个简单格式，`task new` 助手会帮你生成）：

```markdown
## 任务
修复登录按钮的 500 错误

type: bugfix

## 验收标准
- [ ] 用合法凭据点登录返回 200        # → 映射到一条测试（tier 1）
- [ ] 错误日志里没有新错误            # → tier 1（日志检查）或 tier 2
- [ ] 修复符合产品意图（不改 UX）     # → tier 3（人审）
```

分诊（triage skill）判断标准是否充分、哪些可脚本化。验证 tier-1 把可脚本化的标准映射到配置的脚本；tier 2/3 处理其余。没有可检查的验收标准时，分诊在 issue 评论里说清缺什么、置 `needs-info`。

---

## 10. 错误处理与可恢复性

- **分诊 `startable=false`** → 置 `needs-info`，把 `missing_info` 写成 issue 评论，park。
- **门禁拦截** → 置 `needs-human-decision`，写原因评论，park。
- **验证不过（tier 1/2）** 或 **人审 reject** → 失败/反馈成为下一轮 计划 的输入；预算内重试。
- **连续 3 次失败 或 预算耗尽** → 置 `blocked`；`help` skill 生成 `help_request`；写评论；park。
- **tier-3 人审** → 置 `needs-review`，发 review-request 评论，park（释放槽，不阻塞 daemon）。
- **daemon 崩溃/重启** → 从 SQLite 的 `task_status` + `transitions` 重建：哪些在跑（可续跑）、哪些 parked（继续等回复）、哪些待恢复。每个 worktree 按 run-id 找回；parked 任务的 worktree 保留。
- **工单接口暂时不可用**（限流/网络）→ 本次 tick 跳过该步、下次 tick 重试；不丢已落盘状态。
- **确定性脚本报错**（区别于失败）→ 按「失败带详情」处理，绝不静默通过。

---

## 11. 测试策略

- **单测**（确定性，不联网）：预算强制、门禁规则、验证链顺序与短路、skill I/O 序列化/反序列化、配置校验、状态追加 + 回放读、任务生命周期/park-resume、worktree 创建/丢弃、daemon tick 调度（用假 channel + 假 model）。
- **skill 回归**（原则 6）：每个 skill 附带一组 fixture（输入 → 预期输出**形状**）。`loop-eng skill test` 跑它们。两种模式：live-model（本地）和 recorded-fixture（CI，确定性）。skill 版本化；回归失败就拦住这次 skill 改动。
- **集成**：注入假 `channel`（预设的新任务 + 人审回复）+ stub `model.Client`（预设 plan/verify 输出）+ 假 `claude`（固定 diff）→ 确定性地驱动多次 daemon tick，断言 park/resume/并发/路由。
- **端到端验收**（v1「最小可跑通」目标）：建一个 hello-world issue（*「创建文件 greet.txt，内容 'hello'」*，标准 *「文件存在且内容为 'hello'」*），跑 daemon，断言 分诊→计划→执行→验证(tier1)→写评论→done，且战报出现在 issue 评论里。

---

## 12. 关键决策（带理由）

- **主循环 = 常驻 daemon，tier-3 异步经工单评论** —— 串级控制的正确形态：主循环是常驻慢环、永远不阻塞在人上、能并发照管多个子 loop；tier-3 必须 park+释放槽+轮询恢复，不能阻塞终端（否则别的子 loop 全停）。
- **工单 = 任务（issue 即任务，评论即战报）** —— 和代码同源、id 即工单号、人能直接读/回；任务派发走工单（你建 issue，daemon 捞），不靠 CLI 建任务。
- **v1 工单 = GitHub Issue，通道可插拔** —— 最常见、和仓库同源；Jira/Linear 后续走同一 `channel.Channel` 接口。
- **选 Go 而非 Python** —— 单一静态二进制（部署）、goroutine 原生的 daemon/并发、强类型的 skill I/O 契约。常被提起的「Python LLM 生态」反对理由在这里消解：这个工具只做薄 HTTP 调用 + shell out 到 `claude`。
- **`modernc.org/sqlite`（纯 Go）** 而非 CGO 驱动 —— 保持二进制全静态、可交叉编译。
- **两条模型路径**（triage/plan 走 API，execute/verify 走 `claude -p`）——忠于文章；让轻量判断保持便宜，又给 execute/verify 它们需要的工具使用/新鲜会话。
- **验证独立由结构强制**（独立包、不共享上下文、新会话）——命门是一个架构属性，不是一句约定。
- **skill 作为用户拥有的 markdown，覆盖 `go:embed` 默认；v1 内置初稿** —— B 的所有权线；调优能在升级后存活；初稿让 v1 开箱可跑，再用 seed 任务调。
- **只追加、可回放的 trace** —— 靠回放调试，不靠复现（文章 2）。
- **自报是信号不是结论** —— 硬规则，由「execute 的输出不进任何一层的通过判定」来强制。

---

## 13. 开放问题（实现前/早期要定）

1. **`claude -p` 调用细节** —— execute 和新鲜 verify 各自的 flag / 输入格式。实现时按所装的 Claude Code 版本确认。
2. **冷启动 seed 任务集** —— 文章 2 要求用真实任务去调分诊阈值 / 验收标准范式 / skill 初稿。为子项目 1 的校准定这批 seed 任务（需要你给）。
3. **issue 正文格式约定** —— 任务/验收标准的解析格式（§9 的草图）要不要做成更严格的 schema，还是容忍自然语言 + 让 triage skill 解析？
4. **并发默认值** —— `daemon.concurrency` 默认 2 是否合适，还是个人工具默认 1（parked 不占槽，所以 1 也能实现「等人时干别的」）？

---

## 14. 术语对照（文章术语 → 组件）

| 文章术语 | 本设计里对应 |
|---|---|
| 反馈控制环 | 整个 daemon + 子循环流程 |
| 主循环（常驻慢环） | `internal/daemon`（常驻引擎，不阻塞在人上） |
| 子循环（串级快环） | `internal/loop/subloop.go` |
| 控制器 = LLM | 模型客户端（triage/plan 走 API，execute/verify 走 `claude -p`） |
| 设定值 / 期望 | 验收标准 |
| 观测值 / 现状 | 落盘的 SQLite 状态，每轮重读 |
| 失忆将军 | 无状态的 skill + 上下文即缓存原则 |
| 战报 | issue 评论 + `steps`/`transitions` 行 |
| 工单（id/状态/评论流） | GitHub Issue（`internal/channel`） |
| 验证独立 | `internal/verify`、新会话、不共享上下文 |
| 落盘 | SQLite 只追加 trace + `task_status` 生命周期 |
| 预算刹车 | `internal/budget`（三道刹车） |
| 回滚 | worktree 隔离（`internal/isolation`） |
| skill（非确定性代码） | `internal/skill` + 回归框架 |

# loop-eng 核心（子项目 1）—— 设计规格

- **日期：** 2026-07-02
- **状态：** 草稿，待用户评审
- **范围：** `loop-eng` v1 路线图的子项目 1（核心 + 验证地基）
- **实现语言：** Go

本规格**只设计子项目 1**。子项目 2–4 列出作为背景，各自后续单独出规格。

---

## 1. 背景与目标

`loop-eng` 是一个个人的 loop engineering 工具，落地两篇源文章里的反馈控制环方法论：一个**控制器是 LLM** 的控制环，以及由此引出的三项适配——**独立验证、准确的现状表征、状态落盘**。

`loop-eng` 是**常驻 CLI + 运行时**（「B」形态）：工具拥有 loop 的编排代码；用户拥有易变的部分（skill、验证脚本、配置）。用户装一次工具，每个项目配一次，然后拿它去跑开发任务。

**v1 包含全部：** 核心循环、完整三层验证、可观测性数据层、TUI 看板、GitHub Issue 接入、常驻 daemon。v1 按四个有序的子项目来建；本规格覆盖第一个。

**子项目 1 目标：** 一个本地、按需的 loop，能接收单个任务、分诊、跑子循环（计划 → 执行 → 验证 → 写回，带完整三层验证）、强制预算、在 worktree 里隔离执行、落盘一份可回放的 trace、并路由结果（done / blocked / needs-review / needs-info）。这是其余一切的根基；验证是它的命门。

---

## 2. v1 路线图（背景）

| # | 子项目 | 本规格？ |
|---|---|---|
| 1 | 核心循环 + 完整三层验证 + 可回放状态 + 预算 + worktree 隔离 + 可观测性**数据层** | ✅ |
| 2 | 看板 TUI（读 #1 的 SQLite） | 后续 |
| 3 | GitHub Issue 接入（任务源 + 战报；替换本地战报） | 后续 |
| 4 | daemon（常驻轮询 #3 的 Issue 源） | 后续 |

建这个顺序的理由：验证是生死线，必须先单独建稳，看板和 daemon 堆上去才有意义。可观测性的**数据层**归 #1（从第一天起每次状态变迁都是一行）；它的**展示层**（TUI）是 #2。

---

## 3. 不可妥协的原则（来自文章）

这些是硬规则。任何违反其中一条的设计选择都是错的。

1. **上下文是缓存，不是真相源。** 任何必须跨轮存活的东西，在当轮结束前就落盘到 SQLite/磁盘。没有任何跨轮的东西只活在 LLM 上下文里。*「LLM 的记忆只认落盘那份。上下文里的，按零算。」*
2. **验证独立于执行。** 独立性来自**不共享上下文/信息**，不只是「换个模型」。验证端只读落盘的 diff + 验收标准，永远不读执行的推理过程。
3. **自报是信号，不是结论。** 执行端说「我做完了、标准满足了」只是触发验证。结论永远由独立的验证层下。
4. **每一轮主动喂准现状。** 控制器只看得见我们喂给它的文字。子循环每轮开始时**重新读**落盘状态，绝不信任过期的叙事。
5. **三道预算刹车从第一天起就有**——每调用 token 上限、每任务 token 上限、最大重试次数。没有这三样的 loop 就是烧钱。
6. **skill 是非确定性代码。** 无状态、职责固定、输入输出契约固定。要版本化、要有回归集。
7. **主循环 vs 子循环（串级控制）。** 主循环管全局状态和派发；子循环为一个任务而生、跑完把结果交回、然后销毁。子循环的结果被主循环**摄取**。

---

## 4. 范围

**在范围内（子项目 1）：**
- CLI：`init`、`run`、`status`、`replay`、`skill`（edit/test）、`config`
- 主循环：分诊 → 硬门禁 → 派子循环 → 路由结果
- 子循环编排：计划 → 执行 → 验证 → 写回，带重试
- 完整三层验证链（确定性 → LLM 新鲜上下文 → 人审）
- 4 个 skill：`triage`、`plan`、`verify`、`help`（执行直接用 `claude`，不造 skill）
- 两条模型集成路径：直连 API（triage、plan）和 `claude -p`（execute、verify-tier2）
- 可回放的 SQLite trace（可观测性数据层）
- 预算强制（三道刹车）+ 只追加的账本
- 执行的 worktree 隔离
- skill 回归测试框架
- 崩溃后可恢复

**不在范围内（后续子项目 / post-v1）：**
- 看板 TUI（#2）、GitHub Issue 接入（#3）、daemon/轮询（#4）
- web 看板（post-v1）、多 loop 协调（post-v1）
- 多仓库 worktree 扇出、skill 通过 git 自动版本化（未来）

---

## 5. 所有权分界线（「B」契约）

| 工具拥有（稳定内核；只在升级时动） | 用户拥有（易变；用户来调） |
|---|---|
| 主循环派发、子循环骨架、状态读写、预算强制、验证串联、模型客户端 | skill 的 prompt 文件（`.loop/skills/*.md`）、验证脚本（在 config 里声明）、配置参数、验收标准 |

内置的 skill 默认值通过 `go:embed` 打进二进制；`.loop/skills/` 里任何同名文件覆盖内置默认。所以「把 loop engineering 当代码调」整件事都发生在分界线用户那一侧，并且能在工具升级后存活。

---

## 6. 架构

### 6.1 组件图

```
cmd/loop-eng/main.go            入口
internal/
  cli/        cobra 命令：root, init, run, status, replay, skill, config
  config/     加载 + 校验 .loop/config.yaml
  loop/
    mainloop.go    分诊 → 门禁 → 派发 → 路由  （单个任务）
    subloop.go     计划 → 执行 → 验证 → 写回 （单个任务，带重试）
  skill/      skill 注册表、模板渲染、I/O 序列化/反序列化、回归
  model/      两个客户端：apiClient（anthropic SDK）+ claudeClient（os/exec 调 `claude -p`）
  verify/     chain.go, deterministic.go, llm.go, human.go
  state/      SQLite 存储（modernc.org/sqlite）、schema、回放读取器
  budget/     每调用 / 每任务 / 重试 强制 + 账本
  isolation/  worktree 创建 / 提交 / 丢弃
  embed/skills/*.md   内置 skill 默认值（go:embed）
```

每个包只有一个清晰职责，通过类型化接口通信，可独立单测。

### 6.2 关键接口（职责边界）

- `model.Client` —— `Call(ctx, prompt, schema) (output, usage, err)`。两个实现（`apiClient`、`claudeClient`）；循环只依赖接口，测试时注入 stub。
- `skill.Skill` —— `{Name, Version, Render(input) (prompt string), Parse(output) (typed, err)}`。无状态。
- `verify.Tier` —— `Check(ctx, diff, criteria, priorFailure) (result{Passed, Detail, FailingCriteria}, err)`。三个实现。
- `state.Store` —— 只追加写入器 + 回放读取器。trace 行**禁止原地改写**。
- `budget.Enforcer` —— `BeforeCall(usageEstimate) error`、`AfterCall(usage)`、`ShouldRetry(attempt) bool`。

---

## 7. 数据流 —— 一个任务的旅程

```
loop-eng run "<task>"  （或 --task-file task.md）
  │
  ▼
[主循环]  （只在这个任务期间存活）
  1. 加载任务（描述 + 验收标准 + 类型）
  2. 分诊  ── triage skill（小模型，直连 API）
  3. 硬门禁 ── config 规则（机械的，例如 deploy 类型需要 human-go）
  4. 若不可开始   → needs-info  （写入 missing_info）  → 结束
     若门禁拦截  → needs-human-decision             → 结束
  5. 带一份新预算派发 子循环
  │
  ▼
[子循环]  （计划 → 执行 → 验证 → 写回，在预算内重试）
  第 n 轮：
    a. 计划    ── plan skill（直连 API）：读落盘状态 + 任务 + 标准 → 执行计划（不写代码）
    b. 执行    ── `claude -p`，在 worktree 里：计划 + 任务 + 标准 + 仓库路径 → diff + 自报（信号）
    c. 验证    ── 三层链，按序：
                    tier 1  确定性脚本（config）            ─ 不过 → 反馈
                    tier 2  `claude -p` 新鲜会话：只给 diff + 标准 ─ 不过 → 反馈
                    tier 3  人审（needs-review）             ─ 打回 → 反馈
                  全过 → 写回
    d. 写回    ── 落盘状态（SQLite trace）+ 战报（.md）+ 把结果摄取回主循环
    验证不过（tier1/2）或人审打回时：
       把失败作为下一轮 计划 的输入；重试（≤ max_retries）
    连续 3 次失败 或 预算耗尽：
       → blocked → help skill → 写 help_request → 结束
  │
  ▼
[路由]  done | blocked | needs-review | needs-info | error
```

每个编号步骤、每次状态变迁，都在继续往下之前**先**写一行到 SQLite，所以这个 run 从任何点都可恢复、可回放。

---

## 8. 组件细节

### 8.1 CLI 命令面（v1）

| 命令 | 用途 |
|---|---|
| `loop-eng init` | 在当前目录生成 `.loop/`：写 `config.yaml`、拷贝默认 skill、建空 `state.db`、设置 worktree 基目录。**「搭建」这一步。** |
| `loop-eng run "<task>" \| --task-file <path>` | 对单个任务跑完整 loop。`--resume <task-id>` 从最近检查点恢复。 |
| `loop-eng status [<task-id>]` | 从 SQLite 打印任务/run 状态。 |
| `loop-eng replay <run-id>` | 按 trace 逐步回放某次 run。 |
| `loop-eng skill edit <name>` | 打开项目里某 skill 的覆盖文件（若不存在，从内置默认拷一份）。 |
| `loop-eng skill test [<name>]` | 跑 skill 回归 fixture。 |
| `loop-eng config (get\|set\|edit)` | 查看/编辑配置。 |

`loop-eng dashboard` 预留给子项目 2。

### 8.2 配置（`.loop/config.yaml`）

```yaml
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
  tier3_human: true
isolation:
  worktree: true
gate:
  rules:
    - { when: "task_type == deploy", requires: human_go }
skills:
  dir: .loop/skills     # 覆盖内置默认
```

配置加载时校验；预算三刹车的值缺失/非法是硬错误（不给静默默认值）。

### 8.3 主循环

1. 加载任务。
2. 通过 `triage` skill **分诊**（小模型，直连 API）。输出是类型化判断（见 8.5）。
3. **硬门禁**：机械地套用 config 的 `gate.rules`。规则**不做理解**，只做强制（例如 deploy 类型任务需要 human-go 标记）。模型的理解在分诊里已完成；规则做硬门禁。
4. 路由：`needs-info`（startable=false）、`needs-human-decision`（门禁拦截），或派子循环。
5. 子循环返回时，记录结果 + 摄取结果。

### 8.4 子循环编排

计划→执行→验证→写回 的骨架，按任务跑，重试有上限。每轮开始时从落盘存储**重新读**状态（原则 4）。任务结束后子循环销毁，只留下它落盘的结果（原则 7）。

### 8.5 skill（用户拥有，覆盖 `go:embed` 默认）

无状态、I/O 契约固定、版本化。JSON 进出。

- **triage** —— 入：`{task_description, acceptance_criteria, task_type}` → 出：`{startable, missing_info[], loop_doable, suggested_type, difficulty, needs_human_decision, reason}`
- **plan** —— 入：`{task, acceptance_criteria, battle_report(前几轮), repo_state_summary}` → 出：`{plan:[{step, files, expected}], risks[]}`（不写代码）
- **verify**（tier-2）—— 入：`{diff, acceptance_criteria, prior_failure_signal?}` → 出：`{passed, reason, failing_criteria[]}`（新鲜上下文；执行推理**绝不**在输入里）
- **help** —— 入：`{task, blocked_state, attempts_summary, last_error}` → 出：`{help_request:{stuck_at, tried[], need_from_human}}`

**执行不是 skill** —— 它直接调 `claude -p`（要用工具 / 改文件）。

skill 的 I/O 是强类型 Go struct；`encoding/json` 通过 `skill.Skill.Parse` 往返。强类型**就是**「固定 I/O 契约」的强制手段。

### 8.6 验证 —— 命门（完整三层，不分阶段）

对独立性的结构性强制（原则 2）：`verify` 包是一个**与执行分离的组件**，与执行**不共享**任何内存上下文。它只从落盘存储读 `(diff, acceptance_criteria)`。

- **tier 1 —— 确定性脚本。** 对 worktree 跑每一条配置好的 `verify.deterministic` 命令；解析退出码 + 输出。最独立（不碰 LLM）。默认的第一道筛。脚本报错（而非失败）按「失败带详情」处理，绝不按通过。
- **tier 2 —— LLM 新鲜上下文。** `claude -p` 开一个全新会话，只给 **diff + 验收标准**（重试时再加**上一轮的验证失败详情**——绝不是 execute/plan 的输出）。绝不给执行对话，绝不给 plan/execute 输出。处理脚本覆盖不到的语义标准。
- **tier 3 —— 人审。** 任务 → `needs-review`；打印 diff + 战报；交互式询问（accept / reject-with-feedback）。非交互式 run 退出为 `needs-review`（可恢复）。用于业务正确性、审美、外部依赖正确性这类判断。

**顺序：** tier 1 → 2 → 3。tier 1 不过就短路（不浪费 tier 2/3）。任何一层不过 → 失败成为下一轮 计划 的反馈；在预算内重试。**执行端的自报只触发这条链，绝不是结论**（原则 3，结构性强制：execute 的输出不是任何一层「通过」判定的输入）。

### 8.7 状态 / 落盘（可观测性数据层）

SQLite，走 `modernc.org/sqlite`（纯 Go → 二进制全静态）。schema **只追加、为回放设计**，不只记当前状态。

表（草图）：

- `tasks(id, description, acceptance_criteria_json, task_type, source, created_at, updated_at)`
- `runs(id, task_id, started_at, ended_at, outcome, total_tokens, retry_count)`
- `steps(id, run_id, seq, role[triage|plan|execute|verify], skill, model_ref, input_hash, input_json, output_json, tokens_in, tokens_out, status[ok|fail|blocked], error, at)`
- `verifications(id, step_id, tier[1|2|3], passed, detail, at)`
- `transitions(id, task_id, from_status, to_status, reason, at)` —— 每次状态变更一行
- `budget_ledger(id, run_id, scope[call|task], kind[tokens|retry], amount, limit, at)`
- `reports(id, task_id, round, path, at)` —— 指向战报 `.md` 的指针

索引在 `(run_id, seq)`、`(task_id, at)`。trace 行（`steps`、`transitions`、`verifications`、`budget_ledger`）**只追加**；回放 = `SELECT … WHERE run_id=? ORDER BY seq`。战报是人读的 markdown，在 `.loop/reports/<task-id>-r<round>.md`，每轮一份——就是下一轮控制器要读的那段文字（「战报」/ 失忆将军早上的简报）。

### 8.8 预算

三道刹车，在每次模型调用和每次重试之前强制（原则 5）：每调用 token 上限、每任务 token 上限、最大重试（默认 3）。每次检查追加到 `budget_ledger`。碰到任何一道 → 中止 → `blocked` → `help` skill → 写 help_request → 结束。上限可配；**这三道刹车的存在本身不可配**。

### 8.9 worktree 隔离

每个子循环在一个全新 git worktree 里执行，路径 `.loop/worktrees/<run-id>/`。验证通过时，worktree 的 diff 就是产物（后续：提为 PR）。失败/中止时，worktree 分支丢弃。这是回滚原语（文章 2）：「跑飞了丢这个分支」。

### 8.10 模型集成（两条路径）

- **直连 API**（`apiClient`，anthropic Go SDK）：`triage`、`plan`。便宜、快、结构化 JSON 输出。triage 用小模型。让重量级的 `claude` 启动不拖累轻量判断。
- **`claude -p`**（`claudeClient`，`os/exec`）：`execute`、`verify`-tier2。execute 要用工具/改文件；verify-tier2 按文章要求需要全新 Claude Code 会话。`claudeClient` 每次都开**新**会话（不共享对话）——这是验证独立性强制的一部分。

循环只依赖 `model.Client`；两个实现可换、可 stub。

---

## 9. 任务与验收标准格式

一个任务（CLI 字符串或 `--task-file`，markdown/yaml）包含：

```yaml
description: "修复登录按钮的 500 错误"
task_type: bugfix
acceptance_criteria:
  - "用合法凭据点登录返回 200"        # → 映射到一条测试（tier 1）
  - "错误日志里没有新错误"            # → tier 1（日志检查）或 tier 2
  - "修复符合产品意图（不改 UX）"     # → tier 3（人审）
```

分诊判断标准是否充分、哪些可脚本化。验证 tier-1 把可脚本化的标准映射到配置的脚本；tier 2/3 处理其余。没有可检查的验收标准时，分诊返回 `startable=false` → `needs-info`。

---

## 10. 错误处理与可恢复性

- **分诊 `startable=false`** → 迁移到 `needs-info`，把 `missing_info` 写进战报，结束。
- **门禁拦截** → 迁移到 `needs-human-decision`，写原因，结束。
- **验证不过（tier 1/2）** 或 **人审打回** → 失败成为下一轮 计划 的反馈；预算内重试。
- **连续 3 次失败 或 预算耗尽** → 迁移到 `blocked`；`help` skill 生成 `help_request`；写进战报；结束。
- **工具崩溃** → SQLite 里存着最近一次已提交的状态变迁（每次变迁都是在继续前先写入的检查点）。`loop-eng run --resume <task-id>` 从最近的 `transition` + `run` 行重建 run 状态，继续往下。
- **确定性脚本报错**（区别于失败）→ 按「失败带详情」处理，绝不静默通过。

---

## 11. 测试策略

- **单测**（确定性，不联网）：预算强制、门禁规则、验证链顺序与短路、skill I/O 序列化/反序列化、配置校验、状态追加 + 回放读、worktree 创建/丢弃。
- **skill 回归**（原则 6）：每个 skill 附带一组 fixture（输入 → 预期输出**形状**）。`loop-eng skill test` 跑它们。两种模式：live-model（本地）和 recorded-fixture（CI，确定性）。skill 版本化；回归失败就拦住这次 skill 改动。
- **集成**：注入 stub `model.Client`（预设的 plan/verify 输出）+ 一个假的 `claude`（固定 diff）→ 确定性地驱动一次完整 run，断言状态变迁。
- **端到端验收**（v1「最小可跑通 loop」目标）：一个 hello-world 任务——*「创建文件 greet.txt，内容是 'hello'」*，标准是*「文件 greet.txt 存在且内容为 'hello'」*（tier-1 检查脚本）。端到端跑通 分诊 → 计划 → 执行 → 验证 → 写回 → done。

---

## 12. 关键决策（带理由）

- **选 Go 而非 Python** —— 单一静态二进制（部署）、goroutine 原生的 daemon（后续）、强类型的 skill I/O 契约。常被提起的「Python LLM 生态」反对理由在这里消解：这个工具只做薄 HTTP 调用 + shell out 到 `claude`，不需要 langchain。
- **`modernc.org/sqlite`（纯 Go）** 而非 CGO 驱动 —— 保持二进制全静态、可交叉编译。
- **两条模型路径**（triage/plan 走 API，execute/verify 走 `claude -p`）——忠于文章；让轻量判断保持便宜，又给 execute/verify 它们需要的工具使用/新鲜会话。
- **验证独立由结构强制**（独立包、不共享上下文、新会话）——命门是一个架构属性，不是一句约定。
- **skill 作为用户拥有的 markdown，覆盖 `go:embed` 默认** —— B 的所有权线；调优能在升级后存活。
- **只追加、可回放的 trace** —— 靠回放调试，不靠复现（文章 2）。
- **自报是信号不是结论** —— 硬规则，由「execute 的输出不进任何一层的通过判定」来强制。

---

## 13. 开放问题（实现前/早期要定）

1. **v1 默认 skill 的 prompt** —— 四个内置 skill 需要初稿。决定：作为子项目 1 的一部分写好，还是先上极简 stub、靠 seed 任务校准慢慢调（文章 2 的冷启动）？
2. **tier-3 交互模型默认** —— 交互式询问 vs 一律非交互退出可恢复。（提议默认：TTY 下交互式，否则可恢复退出。）
3. **`claude -p` 调用细节** —— execute 和新鲜 verify 各自的 flag / 输入格式。实现时按所装的 Claude Code 版本确认。
4. **冷启动 seed 任务集** —— 文章 2 要求用真实任务去调分诊阈值 / 验收标准范式。为子项目 1 的校准定这批 seed 任务。

---

## 14. 术语对照（文章术语 → 组件）

| 文章术语 | 本设计里对应 |
|---|---|
| 反馈控制环 | 整个 `loop-eng run` 流程 |
| 控制器 = LLM | 模型客户端（triage/plan 走 API，execute/verify 走 `claude -p`） |
| 设定值 / 期望 | 验收标准 |
| 观测值 / 现状 | 落盘的 SQLite 状态，每轮重读 |
| 失忆将军 | 无状态的 skill + 上下文即缓存原则 |
| 战报 | `.loop/reports/*.md` + `steps`/`transitions` 行 |
| 主循环 / 子循环（串级） | `mainloop.go` / `subloop.go` |
| 验证独立 | `internal/verify`、新会话、不共享上下文 |
| 落盘 | SQLite 只追加 trace |
| 预算刹车 | `internal/budget`（三道刹车） |
| skill（非确定性代码） | `internal/skill` + 回归框架 |

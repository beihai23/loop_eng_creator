# loop-eng

> 一个把「反馈控制环」落地到软件开发上的常驻 CLI + 运行时。控制器是 LLM，但它说了不算——独立验证层说了算。

`loop-eng` 是一个个人的 **loop engineering** 工具。它轮询工单（GitHub Issue 或本地文件）拿任务，每个任务跑一个**子循环**——计划 → 执行 → 验证 → 写回，并在不阻塞的情况下等人审。所有过程落盘成一份可回放的 trace。

- 纯 Go 单静态二进制（`CGO_ENABLED=0`，SQLite 用纯 Go 驱动 `modernc.org/sqlite`，不碰 CGO）。
- 模型统一走 `claude -p`（Claude Code CLI），无需 API key / SDK；工具永不接触你的 key。
- 每个子循环在独立 git worktree 里执行，done 自动 FF-merge 回主分支，跑飞的分支直接丢弃。

---

## 它解决什么问题

让一个 LLM 自己跑「写代码 → 自查 → 收尾」的闭环，最常翻车在三件事上：**它会自我安慰说「做完了」**、**它的记忆不可靠**、**没有预算护栏会一直烧**。`loop-eng` 用三条硬规则对症下药（来自[核心设计规格](docs/superpowers/specs/2026-07-02-loop-eng-core-design.md) §3）：

1. **上下文是缓存，不是真相源。** 任何要跨轮存活的东西，在当轮结束前就落盘到 SQLite。LLM 记忆只认落盘那份；活在上下文里的，按零算。
2. **验证独立于执行。** 验证端只读落盘的 `diff + 验收标准`，**永远不读**执行端的推理过程。独立性是结构性强制（独立包 + 每次新会话），不是一句约定。
3. **三道预算刹车从第一天起就有**——每调用 token 上限、每任务 token 上限、最大重试次数。没有这三样的 loop 就是烧钱。

**主循环形态：** 常驻 daemon，同一时刻只有 **1 个子循环**在 actively 跑代码（单活跃 + FIFO）。tier-3 人审时不阻塞终端——任务 **park** 起来（释放活跃位）、daemon 去跑队列里下一个任务；轮询到人的回复后再 **resume**。v1 不做并发 / 优先级 / 依赖调度（见 spec §12 的奥卡姆取舍）。

**工单即任务：** issue 就是任务，issue 的评论就是战报 + 人审通道。工单通道做成可插拔接口，v1 提供两种实现：`local`（本地文件）和 `github`（经 `gh` CLI）。

---

## 工作原理（一个任务的子循环）

```
计划   plan skill       读落盘状态 + 任务 + 验收标准 → 执行计划（不写代码）
执行   `claude -p`       在 worktree 里：计划 + 任务 + 标准 → diff + 自报（只是信号）
验证   三层链，按序短路：
         tier 1  确定性脚本（plan 按任务产出的验收脚本，在 worktree 里跑） ─不过→ 反馈
         tier 2  `claude -p` 新鲜会话：只给 diff + 验收标准        ─不过→ 反馈
         tier 3  异步人审：发 review-request 评论 → park → 等回复
       tier 1/2 全过 → 写回 → done → 自动 FF-merge 到主分支
写回   落盘 trace（SQLite）+ 战报（issue 评论 / outbox）+ 摄取回主循环
```

- tier 1/2 不过：失败成为下一轮 **计划** 的输入，预算内重试。
- 连续失败或预算耗尽：→ `blocked` → 发求助评论 → park。
- 执行端**不得自改验收标准**；发现标准本身有错 → 走人。

任务生命周期态：`new | running | needs-review | blocked | needs-info | needs-human-decision | done | error`。只有 `running` 占活跃位；这些态全落盘，daemon 重启后可从磁盘重建。

---

## 前置条件

- **Go 1.25+**（本仓库 `go.mod` 跟踪 1.25.x）。
- **`claude` CLI（Claude Code）** 已安装并登录、在 `PATH` 上——这是真正的模型引擎。默认配置带 `--dangerously-skip-permissions` 以便无人值守运行，**请按需审查**。
- **`git`**——子循环在 git worktree 里执行，目标仓库必须是 git 仓库。
- **`gh` CLI**（仅 `github` 通道需要）：先 `gh auth login`。

---

## 安装

```sh
# 方式一：编译到仓库根的 ./loop-eng（推荐）
make build          # 等价于 CGO_ENABLED=0 go build -o loop-eng ./cmd/loop-eng

# 方式二：装进 $GOPATH/bin（需该目录在 PATH 才能裸命令调用）
make install        # 等价于 CGO_ENABLED=0 go install ./cmd/loop-eng

# 方式三：只验证能编译（不产出二进制）
go build ./...
```

其它 make 目标：`make test`（全套测试）、`make fmt`、`make vet`、`make clean`、`make uninstall`。

---

## 快速上手

> 以下在一个 **git 仓库**里执行（worktree 隔离需要 git）。把 `./loop-eng` 换成你装好的命令名。

### 1. 初始化

```sh
./loop-eng init                  # 在当前仓库生成 .loop/
# 产物：.loop/config.yaml、.loop/skills/*.md、.loop/state.db、.loop/worktrees/
# 并自动把 .loop/ 追加进 .gitignore
```

### 2. 建一个任务

```sh
./loop-eng task new "创建 greet.txt，内容为 hello" --type feature
# → 生成 inbox/1.md，自动检测项目语言并建议验收标准 + verify 命令
```

`inbox/1.md` 长这样（可手动编辑验收标准）：

```markdown
# 任务
创建 greet.txt，内容为 hello
type: feature
## 验收标准
- [ ] ...（按检测到的语言自动建议）
```

> 你也可以直接手写 `inbox/<n>.md`，或用 GitHub issue（带 `loop:task` 标签）——格式同样是 `## 任务` / `type:` / `## 验收标准` + `- [ ]` 勾选项。

### 3a. 同步跑一个任务（`run-once`）

```sh
# 默认 real：真正的 claude -p 干活，需要 claude CLI
./loop-eng run-once --channel local
# outcome: done — ... → done 时自动 FF-merge 到主分支

# 没装 claude？用 fake 冒烟测试整套环（stub 模型响应，不产出真实代码改动）
./loop-eng run-once --channel local --models fake
```

### 3b. 常驻跑（`daemon`）

```sh
# 本地通道：轮询 inbox/，单活跃跑子 loop，park/resume
./loop-eng daemon --channel local --poll-interval 60s

# GitHub 通道：轮询带 loop:task 标签的 issue（先在 config 里设 channel.provider: github）
./loop-eng daemon --channel github
```

另开一个终端看实时视图：

```sh
./loop-eng status --watch        # 顶部活跃 task+phase，下面全部任务态；Ctrl-C 退出
```

### 4. 看结果 / 回放

```sh
./loop-eng status                # 列出所有任务态：<task_id> <status>
./loop-eng status --task task_xxxx
./loop-eng replay --run task_xxxx   # 按 seq 回放该 run 的每一步：<seq> <role> <status>
```

> `run_id` 就是 `task_id`（见 `internal/state`），所以 `status` 里看到的 id 直接喂给 `replay --run` 即可。

---

## CLI 命令一览

| 命令 | 用途 |
|---|---|
| `loop-eng init [--repo <path>]` | 在仓库生成 `.loop/`（config + skills + state.db + worktrees/），并把 `.loop/` 加入 `.gitignore`。 |
| `loop-eng task new <desc> [--repo <path>] [--type feature\|bugfix\|docs\|refactor]` | 生成 `inbox/<n>.md`；自动检测项目语言（go/node/rust/python/java）并建议验收标准 + verify 命令。 |
| `loop-eng run-once [--repo <path>] [--models real\|fake] [--channel local\|github]` | **同步入口**：从 inbox 捞第一个任务，跑完整 plan→execute→verify→writeback；`done` 自动 FF-merge 到主分支。 |
| `loop-eng daemon [--repo <path>] [--channel local\|github] [--models real\|fake] [--poll-interval <dur>] [--cooldown <dur>]` | **常驻引擎**：轮询工单、单活跃子 loop、FIFO 队列、park/resume、崩溃可恢复。`--cooldown` 是上游瞬时阻塞（如限流）后的派发冷却。 |
| `loop-eng status [--repo <path>] [--task <id>] [--watch]` | 打印任务态。`--task` 看单条；`--watch` 实时视图（活跃 task+phase + 全部任务态）。 |
| `loop-eng replay --run <id> [--repo <path>]` | 按 seq 回放某次 run 的 steps。 |
| `loop-eng skill test` | 列出内置 skill（占位；完整 skill 编辑/回归集见 spec §11，后续）。 |

> `loop-eng dashboard` 预留给子项目 2（看板 TUI），尚未实现。
> 命令行 `--channel` / `--models` 优先级高于配置文件。

**内置 skill**（`go:embed` 打进二进制，`.loop/skills/` 同名文件可覆盖）：`triage`、`plan`、`verify`、`help`。执行不是 skill——它直接调 `claude -p`。

---

## 配置（`.loop/config.yaml`）

`loop-eng init` 写出的默认配置：

```yaml
models:
  triage:  { via: claude-p, binary: claude, cmd: ["--dangerously-skip-permissions"] }
  plan:    { via: claude-p, binary: claude, cmd: ["--dangerously-skip-permissions"] }
  execute: { via: claude-p, binary: claude, cmd: ["--dangerously-skip-permissions"] }
  verify:  { via: claude-p, binary: claude, cmd: ["--dangerously-skip-permissions"] }
budget:
  per_call_tokens: 20000      # 单次模型调用 token 上限
  per_task_tokens: 200000     # 单任务 token 上限
  max_retries: 3              # 最大重试次数
verify:
  tier3_human: true           # 关掉则跳过 tier-3（纯自动跑时）
  # 注意：tier-1 的确定性验收脚本不在 config 里配——由 plan 按每个任务产出
  # （PlanOutput.verify_script），在 worktree 里跑。不可脚本化的任务直接落 tier-2。
isolation: { worktree: true } # 每个子循环在 .loop/worktrees/<run-id>/ 里执行
skills: { dir: .loop/skills } # 此目录下同名文件覆盖内置 skill
channel: { provider: local }  # local | github
daemon: { poll_interval: 60s }# daemon 轮询间隔（<=0 时回落 --poll-interval）
```

**字段说明**

| 段 | 关键项 | 说明 |
|---|---|---|
| `models.<role>` | `binary` / `cmd` | 四个角色（triage/plan/execute/verify）全走 `claude -p`，每次调用开**新会话**（验证独立性的一部分）。`cmd` 透传给 `claude`。 |
| | `name` | 可选：给某角色指定模型（→ `claude --model <name>`），默认用 claude 的默认模型。 |
| `budget` | 三道刹车 | **三者的存在本身不可配**；值缺失/非法（<=0）= 加载硬错误。 |
| `verify` | `tier3_human` | tier-1 的确定性验收脚本**不在 config 里**——由 plan 按每个任务产出（`PlanOutput.verify_script`：脚本内容 + 运行方式，技术栈由 plan 选），在 worktree 里跑，看退出码 + 输出。不可脚本化的任务 plan 不产出，直接落 tier-2。脚本报错按「失败带详情」处理，绝不静默通过。 |
| `channel` | `provider` | `local`：读 `<repo>/inbox/*.md`，写 `<repo>/outbox/`、`<repo>/status/`。`github`：经已认证的 `gh` CLI，按 `task_label` 过滤 issue，评论=战报，状态用 `loop:<status>` 标签。 |
| | `repo` / `task_label` | 仅 `github` 必填（`owner/name` + issue 过滤标签，如 `loop:task`）。 |

**加载校验（硬错误，不给静默默认值）：**
- `budget` 三项必须 `> 0`；
- 四个角色每个必须配 `name` 或 `binary` 之一；
- `channel.provider: github` 时必须同时给 `repo` 和 `task_label`。

---

## 里程碑状态

v1 = 子项目 1（核心地基）。分三个里程碑交付，**目前 M1/M2/M3 均已完成**。

| 里程碑 | 范围 | 状态 | 计划文档 |
|---|---|---|---|
| **M1** 核心地基 | 同步 `run-once` + 四个 skill + 完整三层验证链 + 三道预算 + worktree 隔离 + 只追加可回放的 SQLite trace + hello-world 端到端 | ✅ 完成 | [plans/2026-07-08-loop-eng-m1-core.md](docs/superpowers/plans/2026-07-08-loop-eng-m1-core.md) |
| **M2** 真工单通道 | 可插拔 `channel.Channel` + GitHub 通道（经 `gh`，无 SDK/无存 token）+ 本地通道 + `task new` 向导 | ✅ 完成 | [plans/2026-07-09-loop-eng-m2-real-channel.md](docs/superpowers/plans/2026-07-09-loop-eng-m2-real-channel.md) |
| **M3** 常驻 daemon | 常驻引擎（tick 循环 + 摄入去重）+ 单活跃派发 + park/resume + 异步 tier-3 人审 + `status --watch` + done 自动 land + 崩溃/重启恢复孤儿 running 任务 + 瞬时阻塞冷却 | ✅ 完成（自举 #7–#15：daemon 跑自己的 M3 任务建出来） | — |

**后续（未开始）：**
- **子项目 2：看板 TUI**——读 M1 落地的 SQLite，做可视化展示层。`loop-eng dashboard` 命令位已预留。
- v1 明确**不做**：并发执行、priority、depends_on、scope 串行/冲突检测、web 看板、多仓库 worktree 扇出、Jira/Linear 通道、webhook 接入（见 spec §4 / §12）。

---

## 设计文档

- [核心设计规格（v3）](docs/superpowers/specs/2026-07-02-loop-eng-core-design.md)——背景、原则、架构、数据流、术语对照。
- [M1 实现计划](docs/superpowers/plans/2026-07-08-loop-eng-m1-core.md)、[M2 实现计划](docs/superpowers/plans/2026-07-09-loop-eng-m2-real-channel.md)。

## 开发

```sh
make test     # CGO_ENABLED=0 go test ./...（loop-eng 自举时 plan 产出的 tier-1 验收脚本就是这条）
make fmt vet
```

仓库布局：`cmd/loop-eng`（入口）+ `internal/`（cli / config / state / loop / daemon / channel / skill / model / verify / budget / isolation）。每个包单一职责、可独立单测。

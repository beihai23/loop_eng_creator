# loop-eng

`loop-eng` 让一个 LLM 自己跑「写代码 → 自测 → 收尾」的闭环：你丢一个工单（GitHub issue 或本地文件），它计划、执行、独立验证、写回成一条能 merge 的分支——模型说了不算，独立验证层说了算。

典型用法：把一个 issue 变成一条已验证的 PR、让一个常驻进程自动消化你的 inbox、在预算护栏内反复重试直到验证通过。每一步都落盘，事后可逐帧回放。

- **纯 Go 单静态二进制**（`CGO_ENABLED=0`，SQLite 用纯 Go 驱动，不碰 CGO）。
- **模型引擎是 `claude -p`**（Claude Code CLI）——无需 API key / SDK，工具永不接触你的 key。
- **每个任务在独立 git worktree 里执行**，done 自动合回主分支；跑飞的分支直接丢弃。

---

## 为什么能信任它

让 LLM 自己跑闭环，最容易翻车的三件事：**它会自我安慰说「做完了」**、**它的记忆不可靠**、**没有预算会一直烧**。loop-eng 用三条硬规则对症：

**验证 ≠ 执行。** 给改动打分的那个模型，永远看不到写代码那个模型的推理过程——它只拿到最终的 diff 和你写的验收标准。所以模型没法靠「我觉得做完了」蒙混过关；验证不过，失败理由直接喂给下一轮计划，在预算内重试。

**全程落盘、事后可回放。** 每个任务的每一步（计划、执行、验证、写回）都追加写进本地 SQLite。任何任务都能事后按顺序逐帧回放，看到底跑了什么、卡在哪一步。要跨轮存活的东西，在当轮就落盘——活在模型上下文里但没落盘的，按零算。

**预算护栏从第一天就在。** 三道硬刹车：单次模型调用 token 上限、单任务 token 上限、最大重试次数。任一撞顶就停，不烧钱。这三项不能不配——漏配或填非正数，工具直接拒绝启动。

---

## 安装

**前置依赖：**

- **Go 1.25+**（`go.mod` 跟踪 1.25.x）。
- **`claude` CLI（Claude Code）** 已安装、登录并在 `PATH` 上——这是真正的模型引擎；工具不接触你的 API key / token。默认配置带 `--dangerously-skip-permissions` 以便无人值守运行，**请按需审查**。

  **权限边界（只读纵深）：** triage / plan / verify 三角色默认额外带 `--disallowedTools Edit Write NotebookEdit`，把写工具从 agent 上下文里**物理移除**——它们的「只读」由权限层强制（非仅靠 prompt 自觉）；`--dangerously-skip-permissions` 仍保留在全部角色上（headless 下只读 Bash——grep / `go doc` / `git ls-files`——需要它才能跑），而 deny 规则优先于 bypass，两者共存时 deny 生效。execute 不带 `--disallowedTools`，保留全部写权限（它就是干活的）。**残余风险：** Bash 仍是潜在写通道——worktree 隔离兜住了仓库内落笔（写坏了随树丢弃），但仓库外动作（网络、全局状态、`~/.loop` 之外的文件）属个人工具定位的残余风险，由你按需审查。
- **`git`**——每个任务在独立 git worktree 里执行，目标仓库必须是 git 仓库。
- **`gh` CLI**（仅 GitHub 通道需要）：先 `gh auth login`。

**编译：**

```sh
make build        # 产出 ./loop-eng（等价 CGO_ENABLED=0 go build -o loop-eng ./cmd/loop-eng）
```

其它：`make install`（装进 `$GOPATH/bin`）、`make test`、`make fmt`、`make vet`、`make clean`。

---

## 快速上手

> 以下在一个 **git 仓库**里跑（worktree 隔离需要 git）。把 `./loop-eng` 换成你装好的命令。

**1. 初始化** —— 在当前仓库生成 `.loop/`（配置 + skill + state.db + worktrees/），并自动把 `.loop/` 加进 `.gitignore`。

```sh
./loop-eng init
```

**2. 建一个任务** —— 生成 `inbox/1.md`，自动检测项目语言并建议验收标准。

```sh
./loop-eng task new "创建 greet.txt，内容为 hello" --type feature
```

**3. 跑一遍。** 60 秒冒烟（无需 claude CLI——stub 掉模型响应，把整套环跑通，不产真实代码）：

```sh
./loop-eng run-once --channel local --models fake
# outcome: done — ...
```

让真正的模型干活（需要 claude CLI；`done` 时本地仓库自动 FF-merge 到主分支，GitHub 仓库则发 PR）：

```sh
./loop-eng run-once --channel local            # --models real 是默认
```

**4. 看结果 / 回放。**

```sh
./loop-eng status                  # 列出所有任务：<id> <status>
./loop-eng status --task task_xxxx # 看单条
./loop-eng replay --run task_xxxx  # 按顺序逐帧回放该任务每一步：<seq> <role> <status>
```

> `run_id` 就是 `task_id`，所以 `status` 里看到的 id 直接喂给 `replay --run` 即可。

任务文件长这样（`inbox/1.md`，可手动编辑验收标准）；你也可以直接手写 `inbox/<n>.md`，或用带 `loop:task` 标签的 GitHub issue——格式都是 `# 任务` + `type:` + `## 验收标准` + `- [ ]` 勾选项：

```markdown
# 任务
创建 greet.txt，内容为 hello
type: feature
## 验收标准
- [ ] ...（按检测到的语言自动建议）
```

---

## 配置

`loop-eng init` 在 `.loop/config.yaml` 写出默认配置。绝大多数情况你只需要关心下面几项（命令行 `--channel` / `--models` 优先于配置文件）：

| 想干什么 | 改哪里 | 说明 |
|---|---|---|
| tier-1 验收脚本 | —（不在 config 里） | 由 **plan 按每个任务产出**（`PlanOutput.verify_script`），按项目技术栈写成脚本，在 worktree 里跑，看退出码。不可脚本化的任务 plan 不产出，直接落 tier-2。 |
| 关掉人工复核（纯自动跑） | `verify.tier3_human` | `false` 则跳过人工复核这一关 |
| 调预算（三道刹车） | `budget.per_call_tokens` / `per_task_tokens` / `max_retries` | 单次调用 / 单任务 / 最大重试。**三项必填且必须 > 0**，否则拒绝启动 |
| 给某角色换模型 | `models.<role>.name` | 可选；默认用 claude 的默认模型。`<role>` ∈ triage / plan / execute / verify |
| 换工单通道 | `channel.provider` | `local`（读 `<repo>/inbox/*.md`）或 `github`（经已认证的 `gh`，按标签过滤 issue） |
| GitHub 通道必填 | `channel.repo` + `channel.task_label` | `owner/name` + issue 过滤标签（如 `loop:task`） |
| daemon 轮询间隔 | `daemon.poll_interval` | 留空则用 `--poll-interval` |
| 覆盖内置 skill | `skills.dir` | 该目录下同名 `.md` 覆盖 `go:embed` 进二进制的内置 skill |

默认四个角色全走 `claude -p`、每次调用开新会话。

---

## CLI 命令一览

| 命令 | 用途 |
|---|---|
| `loop-eng config [--repo <path>]` | **主设置命令**（交互式）：缺 `.loop/` 先脚手架，然后引导选 channel provider（local / github / linear）并走完该 provider 的必要配置，写回 `.loop/config.yaml`。github 流程检测 `gh` 安装与登录；linear 的 API key 不进 config.yaml（env `LOOP_ENG_LINEAR_API_KEY` 或 gitignore 的 `.loop/linear.key`）。 |
| `loop-eng init [--repo <path>]` | （deprecated alias，内部调 `config`）在仓库生成 `.loop/`，并把 `.loop/` 加进 `.gitignore`。非交互 stdin（EOF）下只脚手架、保持默认配置。 |
| `loop-eng task new <desc> [--repo <path>] [--type feature\|bugfix\|docs\|refactor]` | 生成 `inbox/<n>.md`，自动检测项目语言（go/node/rust/python/java）并建议验收标准。 |
| `loop-eng run-once [--repo <path>] [--models real\|fake] [--channel local\|github]` | **同步**：捞 inbox 第一个任务，跑完整 plan→execute→verify→writeback；`done` 自动 land。 |
| `loop-eng daemon [--repo <path>] [--channel local\|github] [--models real\|fake] [--poll-interval <dur>] [--cooldown <dur>]` | **常驻**：轮询工单、逐个跑、park/resume、崩溃可恢复。`--cooldown` 是上游瞬时阻塞（如限流）后的派发冷却。 |
| `loop-eng status [--repo <path>] [--task <id>] [--watch]` | 打印任务态；`--watch` 实时刷新（Ctrl-C 退出）。 |
| `loop-eng replay --run <id> [--repo <path>]` | 按 seq 回放某次 run 的 steps。 |
| `loop-eng dashboard [--repo <path>]` | 交互式 TUI 看板：总览 / 详情 / 轨迹三 Tab，`j/k` 选择、`enter` 进详情、`t` 看轨迹、`r` 恢复、`x` 取消、`q` 退出。 |
| `loop-eng skill test` | 列出内置 skill。 |

内置 skill（`go:embed` 打进二进制，`.loop/skills/` 同名文件可覆盖）：`triage`、`plan`、`verify`、`help`。执行不是 skill——它直接调 `claude -p`。

---

## 原理

> 这一节给想知道内部机制的人。上手不需要读它。

### 一个任务的子循环

```
计划   读落盘状态 + 任务 + 验收标准 → 执行计划（不写代码）
执行   在 worktree 里：计划 + 任务 + 标准 → diff + 自报（只是信号）
验证   三层链，按序短路：
         第一层  确定性脚本（plan 按任务产出，按技术栈，在 worktree 里跑）   ─不过→ 反馈
         第二层  claude 新会话：只给 diff + 验收标准            ─不过→ 反馈
         第三层  异步人审：发 review 评论 → 挂起 → 等回复
       第一层 + 第二层全过 → 写回 → done → 自动 land 到主分支
写回   落盘 trace（SQLite）+ 战报（issue 评论 / outbox）+ 摄取回主循环
```

- 第一层 / 第二层不过：失败成为下一轮 **计划** 的输入，预算内重试。
- 连续失败或预算耗尽：→ `blocked` → 发求助评论。
- 执行端**不得自改验收标准**；发现标准本身有错 → 走人。

### 常驻 daemon：单活跃 + FIFO + park

`daemon` 是常驻引擎：同一时刻只有 **1 个子循环**在真正跑代码（单活跃），其余在队列里按 **FIFO** 排队（v1 不做并发 / 优先级 / 依赖调度）。轮询到需要人审（第三层）的任务时，它不干等——把任务 **park** 起来、释放活跃位、去跑队列里下一个；之后轮询到人的回复再 **resume**。daemon 崩溃重启后能从磁盘上的状态重建（含把残留的 `running` 任务收敛回来）。

> **派发是同步的，但摄入不是。** 一个 tick 的派发步骤会阻塞在 `RunTask` 里直到任务跑完（单活跃），所以主循环在一个任务运行期间不会再次 tick——这期间新建的 issue 会进不了 `state.db`、dashboard 也看不见。daemon 因此另起一条**后台摄入 goroutine**，按 3~10s 随机 jitter 独立地 `ListNewTasks → insert`，任务运行中也能把新 issue 落盘（摄入只 insert `new` 任务、绝不占活跃位，所以单活跃派发不破）。`state.db` 开了 WAL + `busy_timeout`，dashboard 用自己那条连接实时读到这些新任务。

### 任务生命周期

`new | running | needs-review | blocked | needs-info | needs-human-decision | done | error`——全落盘，只有 `running` 占活跃位。

### v1 明确不做

并发执行、优先级、依赖调度、scope 串行/冲突检测、web 看板、多仓库 worktree 扇出、Jira/Linear 通道、webhook 接入（详见设计文档）。

---

## 开发

```sh
make test      # CGO_ENABLED=0 go test ./...
make fmt vet
```

仓库布局：`cmd/loop-eng`（入口）+ `internal/`（cli / config / state / loop / daemon / channel / skill / model / verify / budget / isolation / tui）。每个包单一职责、可独立单测。

## 设计文档

- [核心设计规格](docs/superpowers/specs/2026-07-02-loop-eng-core-design.md)——背景、原则、架构、数据流、术语对照。
- `docs/superpowers/plans/` 下有各阶段的实现计划。

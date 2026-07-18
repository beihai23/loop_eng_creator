---
arc: subloop-headless-interactivity
started: 8f47f16
status: resolved
commits: []     # 无代码 commit；本篇由落它自身的 docs-only commit 携带（见 git log 中 Backstory 指针）
---

# 实证 subloop 不会被交互提问卡住（headless 契约调查）

## Intent

用户观察到交互模式的 Claude Code 会用 `AskUserQuestion` 弹窗等用户确认，
追问：subloop 自主运行时如何应对这种情况、避免任务卡住？这直接关系
loop-eng「无人值守跑任务」的核心假设——如果某个环节能静默等人类输入，
整个 loop 就会挂死。

## Process

- 初答基于**推断**：`-p` 无 TUI、stdin 即 prompt 且立即 EOF
  （`internal/model/claude.go:88`）、四角色默认带
  `--dangerously-skip-permissions`（`internal/cli/init.go:25-28`）。
  用户追问「你真确定？事实依据是啥？」→ 推断不够，转为实证。
- **关键实验**（本机，claude 2.1.214）：
  `echo '请立即使用 AskUserQuestion 工具问我一个问题…' | timeout 180 claude -p --model haiku`
  → 几秒内正常退出（EXIT_CODE=0），模型回复「我当前可用的工具列表里没有
  AskUserQuestion 这个工具」。结论比预期更强：headless（`-p` + 管道 stdin）
  下交互工具**根本不挂载进工具列表**，不是「调用了会失败」，而是模型连发起
  入口都没有。harness 按运行模式决定挂载哪些工具。
- 区分了两种「卡住」：**进程挂起**（结构上没有输入通道，不可能）与
  **语义空转**（模型在输出文本里写「请问…」，turn 正常结束、进程正常退出，
  但那轮 plan/execute 产出是废的）——后者靠 verify 驳回 + retry 兜底，
  不挂进程但浪费一轮。
- 顺带确认了真正需要人时的设计路径：不是中途提问，而是 tier-3 `NeedsHuman`
  → `needs-review` 挂起（`internal/loop/subloop.go:320-326`），通过 issue
  评论异步沟通，下一轮 `collectIssueComments` 读回。
- 识别出**真正的盲区**：`runOnce` 没有 per-call timeout——`claudeRetry`
  （`claude.go:104`）只处理「退出非零」，若 `claude -p` 进程 wedge（不退出
  也不报错，如网络挂死）而上游 ctx 无 deadline，调用会无限挂住。建议加
  `context.WithTimeout` + 可选 `--max-turns`——**用户明确拒绝**（「不要」），
  搁置。
- 延伸讨论了多 CLI 接入（opencode/codex）的借鉴：核心洞察是
  「自主运行不卡住」不是模型的属性，而是 **harness headless 模式的契约**，
  每家 CLI 必须逐一实证，不能从 claude 类推。
- 提议把探针固化为 conformance 测试套件 + CLI 升级回归哨兵（仅方向，未实施）。

## Decisions

- **确认当前 claude 接入满足「不卡住」契约**（2.1.214 实证），三层保障：
  交互工具不挂载 + stdin 即 prompt 立即 EOF + skip-permissions。
- **不做 per-call timeout**：用户决定搁置。已知风险边界：进程 wedge 场景
  无兜底；单接 claude 时可接受，未来多接几家行为未知的 CLI 时它会从
  「兜底」升级为「必需品」。

## Lessons

- **实证 > 推断**：`echo '逼它提问的 prompt' | timeout N <cli 的 headless 调用>`
  是验证「会不会等人答话」的最小探针，可直接复用到 codex/opencode 等任何
  新 backend 的接入验收。
- **结论绑定 CLI 版本**（本次 2.1.214）：CLI 升级可能改变 headless 行为
  （比如哪天 AskUserQuestion 在 `-p` 下变成读 stdin 等答案），升级后应重跑
  探针。
- **同一 CLI 两种行为**：AskUserQuestion 在交互 TTY 模式挂载、在
  `-p` + 管道 stdin 模式不挂载。「某工具存在吗」的答案按运行模式而不同。
- **「不卡住」≠「不浪费」**：模型用纯文本提问时进程正常退出，但那轮产出
  是废的，靠 verify/retry 兜回来。
- **接入新 CLI 的四层检查清单**（每层都要实证，不能类推）：
  ① headless 下交互工具是不挂载 / 调用报错 / 真等 stdin（第三种是坑）；
  ② stdin 语义（prompt 走 stdin 还是 argv；管道 stdin 会不会被当作持续
  会话输入而进程不退）；③ 权限/审批 flag 的语义边界（codex 的 sandbox
  不只是「不弹窗」，还决定可写目录）；④ fatal 错误嗅探
  （`fatalClaudeSignals` 这类关键词表是 per-CLI 的，要实测触发一次失效
  凭证才能写对）。

## Related

- `internal/model/claude.go` — `runOnce`（-p 调用）、`callWithRetry`（重试策略）
- `internal/loop/subloop.go` — needs-review 挂起路径、execute prompt 构造
- `internal/cli/init.go:25-28` — 四角色默认 `--dangerously-skip-permissions`
- 无代码 commit：纯调查 arc，结论已落本篇。

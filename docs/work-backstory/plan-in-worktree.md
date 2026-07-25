---
arc: plan-in-worktree
started: 32158d4
status: resolved
commits: [3384a3b]
---

# plan 阶段挪进 attempt worktree（§8.9 回滚原语覆盖 plan）

## Intent
起因是一场评估：用户问「claude code 的 plan mode 和 loop-eng 的 plan 合起来会不会更强大」。
独立判断的结论是不会——plan mode 是**减法**（剥夺写权限 + 换成「探索→呈交给人批准」的
工作流），对规划质量的三根支柱（能看到什么 / 什么模型 / 喂什么反馈）零贡献；且其
plan-then-execute 同会话语义与 loop-eng 刻意的新会话解耦（§8.10）方向相反。

但评估挖出了一个真实漏洞：plan 角色跑 `claude -p --dangerously-skip-permissions`、
cwd 是主仓库根（worktree 原本在 execute 前才创建），「只读不写」只是 plan.md 里的
prompt 层自觉，**§8.9 的回滚原语（跑飞了丢分支）不覆盖 plan 阶段**——plan agent 一旦
落笔，污染的是主仓库且无人兜底。用户拍板做 P0：worktree 创建挪到 attempt 开头，
plan/execute/verify 共用。（P1 = `--json-schema` 契约化 plan 输出、P2 = plan/triage/verify
只读工具集，记录在同次评估中，本 arc 不做。）

## Process
- 拒绝 plan mode 作为「只读载体」的三条理由（评估阶段核实 claude 官方 CLI 参考后定下）：
  ① headless（`-p`）下 ExitPlanMode 无人批准时的行为文档未定义，不能引进无人值守 loop；
  ② plan mode 产出的叙事计划对 loop-eng 的严格 JSON 契约是噪声；③ plan mode 会改变 Bash
  的权限处理，可能误伤 plan.md 要求的只读探索命令。真要物理只读，`--disallowedTools
  "Edit" "Write"`（deny 优先于 bypass）比 plan mode 干净——留给 P2。
- 接口决策：`Client.Call` 是冻结签名（agent.go 注释明确「budget / skill / subloop wiring
  stays untouched」），不能加目录参数。定 **DirClient 可选扩展接口**（`CallIn(ctx, dir,
  prompt)`）+ `Skill.RunIn`：Model 实现了 DirClient 且 dir 非空才走带目录路径，否则回落
  `Call`——存量测试桩（FakeClient 等）零改动自动走旧路径。ClaudeClient 与 agentClient
  实现 CallIn（后者透传 AgentRequest.Workdir，provider 中立：claude 走 cmd.Dir，codex
  走 --cd + cmd.Dir，适配器本来就认 Workdir）。
- worktree 创建从 execute 前挪到 attempt 开头后，**所有新增暴露的退出路径都要补
  `isolation.Discard`**：plan 调用错误重试、空 plan 重试、plan 预算 blocked、plan 后
  cancel。其中「execute 后、verify 前的 cancel 路径不丢 worktree」是 **pre-existing 泄漏**
  （旧代码 worktree 已存在却不丢弃），本次顺手补上——worktree 生命周期前移后这两条
  cancel 路径形态一致，一并处理。
- 顺带回答用户的设计问题（记录在案）：① 同一 attempt 内 plan/execute/verify 共用一棵
  worktree——是，这是设计本意（plan 探索的树 = execute 实现的树，物理一致，少一个
  drift 来源）。② 跨 run（整个 subloop 失败后再触发）**不复用** worktree——这是 §8.9
  回滚原语的定义，不是缺陷；失败现场经 verifyFailComment / 终态战报 → issue 评论 →
  下次 Run 的 collectIssueComments → plan 的 BattleReport 传递，跨进程、跨 reopen 不丢
  （in-memory priorFailure 只活单次 Run，落评论才持久）。例外：needs-review parked 任务
  的 worktree 保留待恢复。

## Decisions
- **plan/execute/verify 共用 attempt 的同一棵 worktree**，而不是 plan 单独一棵：plan 看到
  的树与 execute 物理一致；plan 的违规落笔随 attempt 失败丢弃，永远进不了主仓库。
- **每 attempt 仍是全新 worktree**（plan 失败也即建即弃）：不跨 attempt、不跨 run 复用，
  保持「跑飞了丢这个分支」的回滚语义不变。
- **DirClient 是可选能力而非签名变更**：冻结的 Client.Call 不动，skill 层类型断言 +
  回落，旧测试与 FakeClient 全兼容（compile guard `_ DirClient = (*agentClient)(nil)` 钉死）。
- plan.md 只加一段「当前目录是本轮 worktree、内容同 HEAD」的说明——探索指引不变，
  「只读不写」仍是 prompt 契约（物理兜底由 worktree 丢弃机制承担，不是靠 agent 自觉）。

## Lessons
- 评估「两个同名概念组合是否更强」先分层：一个是单 agent 的交互/安全模式，一个是跨
  agent 的编排契约。减法式能力（只读）叠加不产生协同；真正的增量在契约可靠性
  （claude `-p --json-schema`，P1 候选）与隔离模型补齐（本 arc）。
- worktree 生命周期一旦前移，必须枚举新暴露的全部退出路径补清理——提前创建的
  资源在每个 continue/return 上都是潜在泄漏。
- 「只读」靠 prompt 自觉 + 靠隔离兜底是两道不同的防线：前者会失效，后者不依赖
  agent 守规矩。安全相关设计优先要后者。

## Related
- internal/loop/subloop.go（attempt 开头建 worktree；plan 走 Plan.RunIn；各失败路径补 Discard）
- internal/skill/skill.go（RunIn + DirClient 回落）
- internal/model/model.go（DirClient 接口）、claude.go（CallIn）、agent.go（agentClient.CallIn）
- internal/cli/embed/skills/plan.md（worktree 说明；改 embed 须 rebuild）
- docs/superpowers/specs/2026-07-02-loop-eng-core-design.md §8.9（三角色共用 attempt worktree）
- 测试：internal/loop/plan_workdir_tier1_test.go（plan 在 worktree + 失败即弃）
- 姊妹 arc：[[plan-execute-contract-drift]]（P1 --json-schema 要解决的上游问题）

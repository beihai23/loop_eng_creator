---
arc: p5-real-tokens-step-override
started: 3e8258c
status: active
commits: []
---

# #71 落地：真实 token 采集（A）+ 步骤级 agent override（B）

## Intent
#71（#68 的 P5）被 loop 自己尝试过、三轮 blocked（病根见 [[plan-contract-visibility]]——
振荡，不是任务本身有错）。合同可见性修完后用户拍板：直接接手，亲手完成 #71。
两半：A. 至少一个 provider 走结构化输出取真实 token（替换 `len(Out)` 估算）落
`steps.tokens_in/out`；B. plan 产出 per-phase agent 提示，子循环按 **step → task →
role → 默认** 优先级选用。

## Process
- **A 的关键设计点（issue 已点名）**：开 `--output-format json` 会改变 stdout，而下游
  直接消费 `Out`（plan/verify 从 Out 抠 JSON）。做法：`runOnce` 统一加旗标（插在
  c.Args 前，config 透传仍可覆盖），`callWithRetry` 成功路径调 `parseClaudeResult`——
  信封解析出 result 作 Out（语义不变）+ 真实 usage；**任何形状偏差兜底原文+估算**，
  不致命不中断（#68 风险节：各家 JSON schema 不稳）。DoD 只要求一个 provider，
  codex 维持估算（其 `--json`/output-schema 有已知 bug，#71 风险节记录在案）。
- **顺带发现的缺口**：`steps.tokens_in/out` 列一直存在，但 SubLoop 的 AppendStep 从没
  填过——不只是「估算了」，是「根本没落库」。本次把 plan/execute 两行的 tokens 填上；
  verify 行的 usage 被 `verify.Chain` 的冻结 Tier 接口（Check 不返回 usage）挡住，留作
  后续（DoD 不卡这项）。
- **B 的接线选择**：SubLoop 不碰 config——新增 `AgentForRole func(role, provider)
  (model.Agent, error)` 闭包字段，cli 层（run-once/daemon 共用 `agentForRole` 助手）按
  forProvider 语义构建（换 provider+binary、保留 model 名、丢旧 provider 的 cmd 旗标）。
  hint 在 plan 返回后、execute 前解析（时序正好）；未知 provider 工厂报错 → 回落角色
  配置，不崩（plan 输出是不可信输入）。
- **B 的一个坑**：override verify 时不能只换 Model——cli 给 verify 套了 budget.Client
  装饰器（verify.Chain 的冻结 Tier 不带 Enforcer，装饰器是 verify token 计入预算的唯一
  通道）。override 路径用 `&budget.Client{Base: AsClient(a), Enf: sl.Budget}` 保留它。
- 优先级覆盖策略：step>task 用「ExecuteModelRef 模拟 task 级 label + hint 压过它」钉死；
  task>role 由既有 applyTaskAgent 测试覆盖；role>默认由 providerLabel/NewAgent 默认覆盖。

## Decisions
- **信封解析单点化**在 callWithRetry：Call/Exec/CallIn 三入口一次覆盖；codex 走
  runWithRetry 不受影响的同一份兜底语义。
- **AgentHints 只覆盖 execute/verify**（plan 不能事后换自己的 agent；triage 在 SubLoop
  之外）。字段 nil = 无 hint，优先级链自然成立。
- **plan.md 对 agent_hints 的指引是「默认不产出」**：角色配置已含任务级 override，
  多数任务没有换 agent 的理由——防 plan 把 hint 当装饰乱发。
- **冻结接口不动**（DoD 硬条款）：Client/Executer 签名零变更，override 全部经
  AsClient/AsExecuter 桥接；subloop_test.go 仅 tiersFor 签名（内部函数）随行更新。

## Lessons
- 「列存在」不等于「数据在流」：DoD 里「落 steps.tokens_in/out」真正的坑是 AppendStep
  从未填 tokens 字段——先验证数据通路两端，再谈解析。
- override 一条已有的装饰链时，先列出链上每一环的职责（这里是预算装饰器），否则
  换个 provider 就悄悄关了预算。

## Related
- internal/model/claude.go（--output-format json + parseClaudeResult + 兜底）
- internal/model/real_tokens_tier1_test.go（真实值 + 兜底）
- internal/skill/skill.go（AgentHints + PlanOutput.AgentHints）
- internal/loop/subloop.go（AgentForRole + resolveAgentHints + steps tokens 落库）
- internal/loop/agent_hint_tier1_test.go（override/优先级/回落/verify 四案）
- internal/cli/run_once.go（agentForRole 助手 + 接线）、internal/cli/daemon.go（接线）
- internal/cli/embed/skills/plan.md（agent_hints 输出契约 + 默认不产出铁律）
- spec §8.10（真实 token 采集 + 步骤级 override + 合同可见性三段）
- 上游：#68（provider 抽象）、#71（本 issue）、[[plan-contract-visibility]]（先修的振荡病根）
- 后续：verify step 的 usage 出 Chain（需动冻结 Tier 接口或旁路）；codex 真实 token

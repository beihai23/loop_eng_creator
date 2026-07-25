---
arc: plan-contract-visibility
started: 3e8258c
status: resolved
commits: [2c56a17]
---

# plan↔execute 合同可见性（#71 振荡的根治）

## Intent
承接 [[failure-scene-feedback]] 的遗留问题。挖 #71 三轮 blocked 的 state.db trace 时发现
病根不是能力而是结构：plan 把 `parseClaudeResult` 签名冻结进 tier-1 测试（4→3→2 每轮
重设计），execute 的实现（3→2→3）每轮瞎猜——**第 3 轮双方互换位置**（plan 向 execute
上轮的 2 靠、execute 向 plan 上轮的 3 靠），教科书级振荡。三层根因：① execute 看不到
plan 冻结的合同（execPrompt 里没有 plan 产出，"签名严格按此"写在只有 verify 能看到的
地方）；② 双方每轮从零重掷（现场回灌已修掉大半）；③ 双方同时对陈旧信号反应。
现场回灌修的是②，本 arc 修①③。用户拍板：补上，然后顺手接手完成 #71。

## Process
- 修法定为「合同有唯一所有者（plan）+ 双向可见」，而不是让 execute 也参与合同设计：
  - **execute 侧**：`executeContractSection` 把本轮 plan 的 steps（冻结签名）+ verify_script
    全文（file/body/run）直达 execPrompt，并明示「plan 已替你做完设计决策，不要发明
    第三种签名」。
  - **plan 侧**：`PriorPlanContract` 回灌上一轮合同（run 内内存直传；跨 run 新增
    `LatestPlanOutputByRef`——与现场回灌同款按 issue_ref 跨 task 行查询），plan.md 的
    指引是**默认保持稳定**：被驳回往往只是 execute 没对上合同，双方同时改才会振荡；
    只有驳回证明合同本身错误才修订并在 risks 说明。
- 与 [[plan-criteria-revision]] 的「修订权」是不同层：那是验收标准（WHAT），这是实现
  合同（HOW 的冻结签名/验收脚本）。合同的修订门槛刻意写得很高（驳回指向合同本身），
  因为振荡的代价 > 偶发的不最优签名。
- 测试铁证设计：within-run 断言 attempt-1 的 execute prompt 已含冻结签名（不等驳回）、
  attempt-2 的 plan prompt 含 attempt-1 合同；跨 run 断言经 SQLite 拿到。

## Decisions
- **合同投影 = {plan steps, verify_script}**：risks/revised_criteria 走原有通道不进投影
  （contractOf），回灌/注入只带约束实现的两块。
- **合同与现场分字段不分层**：RejectedDiff（execute 写了什么）与 PriorPlanContract
  （plan 冻结了什么）各自独立渲染——振荡治理需要同时看到「双方各自的位置」。
- **execute 侧每次 attempt 都注入本轮合同**（不只重试时）：第一次实现就该对着合同写，
  而不是被驳回后才看到。

## Lessons
- 两个互不可见的 fresh session 各自维护同一份契约的一半，必然漂移；给契约指定唯一
  所有者并让另一方只读可见，比「双方都更聪明」便宜得多。
- 反馈通道要按「谁需要看到什么」设计：判决给 plan 有用，给 execute 半有用；合同给
  execute 才是命门。

## Related
- internal/loop/contract.go（contractOf / loadPriorPlanContract / executeContractSection）
- internal/state/state.go（LatestPlanOutputByRef）
- internal/skill/skill.go（PlanInput.PriorPlanContract）、internal/cli/embed/skills/plan.md
- internal/loop/subloop.go（lastPlanContract 接线 + execPrompt 合同段）
- 测试：internal/loop/contract_tier1_test.go
- 上游：[[failure-scene-feedback]]（现场回灌；本 arc 是其验尸后的第二刀）、[[plan-in-worktree]]
- 下游：[[p5-real-tokens-step-override]]（合同可见性落地后亲手完成的 #71）

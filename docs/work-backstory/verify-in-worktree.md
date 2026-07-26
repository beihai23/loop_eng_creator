---
arc: verify-in-worktree
started: e8175f9
status: active
commits: []
---

# verify tier-2 进 worktree + 预算错误即停（#81/#86/#87 三连败的修复）

## Intent
分工改为 plan/execute=claude、verify=kimi 后的第一次实战，#81/#86/#87 三连 blocked。
挖 state.db trace 发现是两个独立根因：
- **#81**：verify tier-2 不带 workdir、跑在主仓库根。claude 时代它按 prompt 判 diff
  文本；kimi 更 agentic，主动拿 diff 对照文件系统 ground-check——但脚下是主仓库
  （HEAD 干净），不是 execute 的 worktree，于是假驳回「diff 未实际落到工作区」。
  实现其实已完成且 tier-1 通过。
- **#86/#87**：真实 token 记账上线后，plan+execute 单轮烧 ~180k（合同段/现场段/
  issue 全文让 prompt 膨胀），200k 的 per_task 是 len(Out) 估算时代定的——verify
  连跑都没跑就被 budget.Client 拒付，retry-gate trace 签名逐字是
  `verify error: budget: per-task token cap exceeded`。subloop 还把它当可重试错误，
  又空烧两轮 plan+execute 才撞墙。

用户拍板人工接手修复（不等 loop 排队）。预算问题同时定调：预算是**固定值**
（操作者旋钮），不是 triage/plan 估算——刹车不能让被刹车者定价；#86 的方向是
估算依据来自上一轮真实用量，不是模型拍脑袋。

## Process
- **修法选了「给它正确的目录」而不是「禁止它看」**：kimi 的 ground-check 本能是
  对的（比盲信 diff 文本更强的验证），错在我们没告诉它改动在哪。verify.md 顺势
  明确「鼓励 ground-check，判定只看 diff+标准；主仓库看不到改动是正常的」——把
  隐性假设写成了显性契约。
- **Tier 接口不破**：LLM 加内部 Dir 字段，Check 走 RunIn（P0 的 DirClient 机制），
  tiersFor 统一注入 wt——三角色自此物理同树，subloop 的 package 注释（plan/
  execute/verify 共用一棵树）从「两实一虚」变成名实相符。
- **预算错误即停是顺手做的最小正确修复**：errors.Is(ErrPerCall/ErrPerTask) →
  blocked，与 plan/execute BeforeCall 的预算闸语义对齐。#86 的全量重设计
  （估算表/入账面/triage+help 进预算）仍留给 loop——不把「立即止血」和「重新
  设计」混为一谈。
- 直接代价：config 先把 per_call/per_task 提到 100k/1M 解锁队列（旋钮语义待 #86）。
- P0 的 TestTier1PlanRunsInAttemptWorktree 断言过期（planDirs 从 1 变 2：plan+
  verify）——这正是「行为变化要扫断言语义而不是扫红」的实例：不是测试坏了，
  是系统真的变了。

## Decisions
- **ground-check 鼓励但要站在对的树里**：不为了防假驳回而削弱 verify 的主动性。
- **Dir 空 = 旧行为**：纯测试/旧装配零影响，Tier 签名冻结不破。
- **预算错误 vs 质量错误的分岔进 subloop**：预算=即停，质量=重试（零增益门槛
  管），flake=重试——三类失败三条路，不再同构处理。

## Lessons
- 换 verify 厂商暴露的是 claude 时代被「温顺行为」掩盖的架构缺口——交叉厂商
  配置的价值不在省钱，在于把隐性假设逼成显性 bug。
- 「错误」不是一类东西：预算是资源判决（重试不可能自愈）、质量是过程反馈
  （重试可能自愈）、flake 是环境噪声（重试就是解药）——错误分类决定重试策略。

## Related
- internal/verify/llm.go（LLM.Dir + RunIn）、internal/cli/embed/skills/verify.md（worktree 语境）
- internal/loop/subloop.go（tiersFor 注入 wt + 预算错误即停分支）
- internal/loop/verify_dir_tier1_test.go（三角色同树 + 预算即停）、plan_workdir_tier1_test.go（断言跟进）
- 病根 trace：.loop/state.db run_6233812a6ea47e99 / run_e5639ed70a2bc0fb / run_fecc11a7030ad6d8
- 上游：[[plan-in-worktree]]（P0 的 DirClient 机制复用）、#86（全量预算重设计，未做）

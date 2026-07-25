---
arc: failure-scene-feedback
started: 32158d4
status: resolved
commits: [3384a3b]
---

# 失败现场回灌 + worktree GC（判决之外，现场也传；树是缓存，SQLite 是档案）

## Intent
紧接 [[plan-in-worktree]]。用户质疑 P0 答辩里的一个论断：「跨 run 不复用 worktree 是特性，
失败现场经工单评论传递」。被要求用批判性思维重想后，发现这个辩护有两个实打实的错误：

1. **混淆了「不落地」和「不留存」**。回滚原语的安全边界在 landing（未验证的工作永不
   merge 进 main），不在树的存亡。一棵 blocked 的失败树躺在 `.loop/worktrees/` 对主仓库
   威胁为零——「复用失败的树 = 拿没验证的现场当起点」把「不落地未验证的工作」（必要）
   偷换成了「销毁未验证的工作」（不必要）。
2. **高估了战报通道的保真度**。verifyFailComment 传的是判决（驳回理由+建议），不是现场。
   真正的现场（execute 的完整 diff）早已落在 steps.output_json，但**没有任何代码路径把它
   读回来**——信息被捕获了，然后被浪费了。判决在引用一份已销毁的证据。

往下挖发现场景丢失是系统性的：run 内每个 attempt 也丢现场（驳回→弃树→从零重掷）；
execute 连驳回理由都看不到（execPrompt 没有 priorFailure，唯一例外是 #46 给编译错误
打的特化补丁 compileErrorSection——那是同一系统性问题的症状性补丁）。用户拍板做
「第 1 层 MVP（diff 回灌）+ GC」，宽限期定为 **48h**（原提案 1h，用户改）。

## Process
- 拒绝了「直接复用失败 worktree 当起点」这个另一端极端：HEAD 可能已移动（旧树陈旧）、
  锚定效应（LLM 对沉没成本同样敏感）、生命周期/GC 无主、与「每 attempt 全新树」不变量
  冲突。正确解法是**分离「证据保全」与「执行起点」**。
- 关键推理（整个 arc 的支点）：既然未验证的工作永远不会落地（verify 是闸门），那么把
  被驳回的 diff 注入下一轮 prompt——甚至将来 git apply 预置进新树——都同样安全。
  **树的新旧从来不是安全边界**，这允许我们比旧设计激进得多地保存和复用现场。
- 回灌双通道：run 内不绕 SQLite（驳回处内存里的 diff 就地更新 lastRejectedDiff）；跨 run
  由 Run 开头按 issue_ref 查 LatestExecuteOutputByRef（steps⨝runs⨝tasks）——同一个查询
  覆盖 daemon 同 task 行重跑与 run-once 每次新建 task 行两种形态（跨 task 行关联是刻意
  的，run-once 场景靠它才能找回现场）。
- 注入两侧：plan 走 PlanInput.RejectedDiff + plan.md 条件块；execute 走
  rejectedSceneSection（判决+现场合一）——顺带补上「execute 看不到驳回理由」的缺口。
  prompt 里的锚定风险用文案对冲：明确标注「它被判过不合格，沿用还是推倒由你判断」。
- GC 的设计前提先于策略：**SQLite 是档案，worktree 是缓存**——只要复用/回灌都从
  SQLite 读、不依赖树存活，GC 就与正确性彻底解耦，最坏的误删丢的是调试便利。策略
  刻意简单：48h 宽限期（防状态滞后误删活树）→ 超期问 task_status（真相源）→ 活态
  （running/needs-review/needs-info/needs-human-decision）与 done（land-salvage 留人）
  保留，blocked 超 7d TTL 删，其余删。触发点：daemon 每 tick + `loop-eng clean --dry-run`。
- Prune（GC 专用）与 Discard（loop 热路径）分开：Prune 宽容（worktree remove 失败回落
  RemoveAll + worktree prune，branch -D best-effort），不给热路径的严格语义掺水。
- done 状态的树 GC 一律保留：正常路径 land 后调用方自清，留下的都是 land 失败的
  salvage 现场，罕见且需要人——不为了「干净」给 GC 加误删风险。

## Decisions
- **判决与现场分路传递**：判决（战报/评论/priorFailure/RetryDiagnosis）照旧；现场走
  新通道（run 内内存 + 跨 run SQLite），两侧（plan/execute）都注入。
- **末轮才保留现场树**：非末轮 attempt 树即建即弃（diff 已双份留存：lastRejectedDiff +
  steps.output_json），只有重试耗尽的最后一轮保留 worktree——「每个任务最多一棵现场树」，
  天然限制磁盘占用。
- **48h 宽限期**（用户拍板，原提案 1h）：宽限期内不问状态一律保留；状态判定只在宽限期
  之后发生。GC 常量：Grace=48h、SceneTTL=7d（`loop-eng clean` 可用 flag 覆盖）。
- **GC 状态驱动而非时间驱动**：纯 TTL 不可靠（parked 两周的树可能正是人在审的那棵），
  task_status 是保留与否的第一判据，时间只是宽限与 TTL。
- **execute 侧注入段合并判决+现场**：rejectedSceneSection(priorFailure, diff)——execute
  过去连判决都看不到（#46 只给编译错误开了小灶），这一步把通用通道补上了。

## Lessons
- 「销毁现场」曾被当成安全机制来辩护，其实它只是懒惰的卫生。找安全边界要看闸门在哪
  （landing 的 verify gate），而不是看哪里「看起来保守」。
- 判决形状 vs 现场形状的反馈通道是两种东西；只传判决的系统会逼出 #46 那样的症状性
  补丁（一个症状开一个专用通道），通用修法是把现场接回回路。
- 缓存/档案分层一旦确立（档案在 append-only 的 SQLite，树是缓存），GC 策略就可以简单
  粗暴——复杂 GC 往往是「缓存被当成了档案」的症状。
- run-once 每次新建 task 行：跨 run 关联必须按 issue_ref 跨 task 行查，按 taskID 查会在
  run-once 形态下静默丢失全部历史。

## Related
- internal/loop/scene.go（回灌通道）、gc.go（状态驱动 GC，48h 宽限期）
- internal/loop/subloop.go（lastRejectedDiff 接线、末轮现场保留、blocked 战报注明路径）
- internal/state/state.go（LatestExecuteOutputByRef，按 issue_ref 跨 task 行）
- internal/skill/skill.go（PlanInput.RejectedDiff）、internal/cli/embed/skills/plan.md（现场块；改 embed 须 rebuild）
- internal/isolation/wt.go（List/Prune）、internal/daemon/engine.go（GC hook，tick step 3.6）
- internal/cli/clean.go（loop-eng clean --dry-run）、internal/cli/daemon.go（GC 接线）
- 测试：scene_tier1_test.go（run 内/跨 run 回灌 + 末轮保留）、gc_tier1_test.go（decide 分支 + 集成 + dry-run）
- spec：§8.6（判决/现场两路反馈）、§8.9（末轮保留 + GC；安全边界在 landing 的澄清）
- 上游：[[plan-in-worktree]]（P0；本 arc 是其答辩中被质疑的一环）；对照 [[plan-execute-contract-drift]]
- 后续候选：layer 2（git apply 预置上一轮 diff 作起点，HEAD 冲突回落纯上下文）

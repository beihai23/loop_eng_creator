---
arc: battle-report-readback
started: 3c17330c6d8653cce7f825a00d2f78ce878e3dc1
status: resolved
commits: [133217c]
---

# 战报只写不读：机器记忆与人可读写回分通道

## Intent

dashboard 详情页里 execute/plan 的初始提示词出现大量重复表述。排查确认不是 dashboard
显示问题（input_json 就是真实 prompt），而是 prompt builder 把同一信息经多条通道
重复注入。病根：战报（VERIFY-FAIL 评论、DONE/BLOCKED 终态评论、NEEDS-INFO 等）本是
**写给人看的 issue 写回**，却被 `collectIssueComments` 无过滤读回、当作 daemon 自己的
跨 run 记忆——一条人可读散文线程被迫兼任机器数据库，导致自回声（priorFailure 在
prompt 里出现 2-3 次）、无界膨胀（评论随轮次线性增长）、无结构（只能全量倾倒，
plan/execute 不分阶段共享一锅汤）。

## Process

- 第一性原理重建信息流：DB（runs/steps/verifications）是机器记忆的唯一 source of
  truth；issue 评论只承载「人写的内容」进 prompt；战报只写不读。verify 阶段（diff +
  标准 + 最新驳回）已是目标形态，不动；triage 的人反馈本就走 DB resume_feedback
  （正确姿势），但它自己发的 NEEDS-INFO 评论也是污染源。
- 盘点全部 daemon 发评论点（7 类，均有确定性前缀）：VERIFY-FAIL、DONE:/BLOCKED:/
  NEEDS-REVIEW:/CANCELLED:、NEEDS-INFO:、NEEDS-HUMAN-DECISION:、REVIEW-REQUEST、
  PR 待合并：、LAND PARTIAL。决定：新发评论统一加 `<!-- loop-eng:bot -->` 隐形标记，
  过滤以标记为准、前缀做存量（标记引入前已发出的评论）兜底。
- 发现 execute 的跨 run 回归风险：过滤掉 VERIFY-FAIL 评论后，跨 run 重跑的 execute
  会失去「上一轮驳回理由」（现场 diff 有 loadPriorSceneDiff 跨 run 回灌，但判决理由
  没有对应通道）。决定 run history（DB 构建）同时喂 plan 和 execute，而非只喂 plan。
- PlanInput.BattleReport 改名 HumanFeedback：旧名字本身就是被消除的混淆概念
  （「战报」当 prompt 输入），留着会 perpetuate 误读。priorFailure 不再拼进
  HumanFeedback——attempt≥2 由 RetryDiagnosis 逐字引用（同条件注入），resume 反馈
  本身是人评论、已在过滤后的评论里，拼进来必是同文两份。
- triage 也接上 RunHistory：已反复 blocked 的任务被 reopen 后，triage 能看到历史、
  直接判 needs_human_decision，而非再放行空烧。runTriage（测试钉死的并行实现）
  没接——它与线上 triageFn 的 drift 是既有现象（PriorFeedback 也只有 triageFn 传），
  照既有先例处理。
- repo 基线在 Go 1.25 gofmt 下本就不净（doc comment 重排规则），新文件沿用旧式
  注释风格保持一致，不做全仓重排。

## Decisions

- **战报只写不读**：report/verifyFailComment/parkByTriage/Human.Check/land 注记的
  产出逻辑一字不改，只是发出的评论带 bot 标记、且不再被读回喂 prompt。
- **过滤判据双层**：新评论 `<!-- loop-eng:bot -->` 标记（权威）+ 存量评论确定性
  前缀兜底；`Reply` 没有 author 字段，前缀/标记是现成的确定性杠杆（与
  failureSignature 同款思路）。误滤代价有界（一条人反馈没进 prompt，落地闸门是
  verify 不是 prompt）。
- **机器跨 run 记忆 = BuildRunHistory(DB)**：每个已终结 run 一行（outcome + 最后
  一次 verify 驳回截断），≤8 行——大小随 run 数有界，不随重试轮次膨胀。state 层
  只取原始 output_json（RunHistoryByRef），解析格式化在 loop 层（verifyTrace 是
  loop 私有形状）——与 LatestExecuteOutputByRef 同款分工。
- **verify 不动**：它本就是目标形态（diff + 标准 + 最新驳回，O(1)），且判官不该
  看到 run history/人反馈——防止被「之前怎么判的」带偏。

## Lessons

- 用「人可读写回通道」兼任「机器记忆」必然三病齐发：自回声、无界、无结构。修
  prompt 膨胀时先问每条信息的受众是谁，按受众分通道，而不是在文本里做取舍。
- 过滤掉一条旧通道前，先盘点它暗中承载的跨边界职责：VERIFY-FAIL 评论同时是
  「人看的驳回公示」和「execute 跨 run 的驳回理由来源」——砍掉读回而不补 DB 通道，
  execute 就会在跨 run 重跑时丢失判决理由（只剩现场 diff）。
- daemon 与模型共用一面墙（issue 线程）时，谁写的必须可机械识别；标记要在
  写入侧加，靠内容启发式事后猜是补不回来的。

## Related

- 关键文件：internal/channel/botcomment.go（标记+识别）、internal/state/state.go
  RunHistoryByRef、internal/loop/runhistory.go（BuildRunHistory）、
  internal/loop/subloop.go（collectIssueComments 过滤、plan/execute 接线）、
  internal/cli/embed/skills/{plan,triage}.md、internal/cli/daemon.go（triageFn）
- 兄弟 arc：failure-scene-feedback（现场回灌，同款「DB 而非评论」姿势的先例）、
  plan-contract-visibility（合同回灌）、triage-gate


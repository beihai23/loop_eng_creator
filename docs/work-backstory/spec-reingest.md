---
arc: spec-reingest
started: 39549b2
status: resolved
commits: [f86a441]
---

# issue 正文变更的重新摄入（spec re-ingest）

## Intent
问题：daemon 摄入按 IssueRefs 去重，已知任务的正文/验收标准永远停留在首次摄入的快照；
issue 正文被人编辑（不可避免）后，重跑/resume 仍用旧 spec。用户要求增加「重新摄入正文」
机制，且明确「每次都去读能 work 但不够好」——要更聪明。

关键发现（设计支点）：GitHub `ListNewTasks` 每次轮询本来就用
`gh issue list --json number,title,body,...` 拉全部 open 带标签 issue 的**完整正文**，
Local channel 每次也重读 inbox 全部 .md——新鲜正文**已经在手里**，去重逻辑把它扔掉了。
初始预期：ingest 里做「比对才写」（compare-and-update），零额外 channel 请求。

## Process
- 否决方案「派发前逐任务 gh issue view 刷新」：每次启动多一次 API 调用，且对排队中任务的
  dashboard 展示无帮助；而轮询载荷里已有全量正文，额外读纯属浪费。
- 快照语义保持不变：运行中的 run 持有 dispatch 时构造的 channel.Task 副本，正文热更新只影响
  下一次派发/resume——run 内 plan/execute/verify 看到一致的 spec，这是特性不是缺陷。
- 实现落点：`state.TaskSpecsByRef()`（ref→{id,desc,criteria,updated_at} 快照）+
  `state.UpdateTaskSpec()`；`engine.ingest()` 已知 ref 走 compare-and-update（解析后的
  desc+criteria 与落库内容同构比对，逐 tick 零 no-op 写）。新 ref 照旧 InsertTask。
- 小坑：ingest 原来用 `IssueRefs() map[string]bool` 去重，改造后整表换成 specs map 兼任去重；
  同批次内重复 ref 靠 `specs[t.Ref] = row` 防双插（row 无 id 没关系，同批次比对用不到 id）。
- 测试上为「无 no-op 回写」的断言给 TaskRow 加了 UpdatedAt 字段（仅 TaskSpecsByRef 填充）——
  没有它 daemon 包（外部包）无法观测 updated_at 是否被摸过。

## Decisions
- 「聪明」= 零额外 channel 读 + 写只在真变更时：复用每次轮询已有的 ListNewTasks 全量正文载荷，
  compare-and-update；比对解析后字段（与落库同构），不经 hash 不加 updatedAt 探测。
- 不限定任务状态：done/blocked 的任务正文被编辑也照刷——reopen/resume 时本就该用新 spec。
- 观测性只做 daemon log（`ingest: task <ref> spec updated`）+ tasks.updated_at 时间戳；
  不加 transitions/事件表——status 没变，写转场会污染状态语义。

## Lessons
- 排查「要不要额外读」之前先看现有轮询载荷——这个系统里 channel 读是最贵的资源（gh CLI
  子进程 + rate limit），而 ListNewTasks 已经在拉全量正文，很多「刷新」需求可以免费满足。

## Related
- internal/daemon/engine.go（ingest compare-and-update）
- internal/state/state.go（TaskSpecsByRef / UpdateTaskSpec / TaskRow.UpdatedAt）
- 测试：engine_test.go TestIngestResyncsEditedSpec、state_test.go TestUpdateTaskSpec
- 姊妹篇：[[dashboard-initial-prompt]]（plan/execute prompt 落盘后，新 spec 是否生效可直接
  在详情页的「初始提示词」里验证）

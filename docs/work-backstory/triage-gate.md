---
arc: triage-gate
started: f2efa95
status: resolved
commits: [99fb0c4]
---

# triage 接线进 daemon 派发门（triage/gate 里程碑）

## Intent
起因：修「triage 输入看不到全文」时发现 triage 是**完全的死代码**——TriageInput 全仓库
只有测试用到，run_once.go 构建后 `_ = triage` 丢弃，注释谎称「M3 daemon 调用」；
needs-info 状态的样式/cancel 都留着但没有任何代码把任务转进去。用户拍板：接线进
daemon + 全文输入（spec 里 triage/gate 里程碑的落地）。

设计：Engine 加 TriageFunc 回调（与 RunTaskFunc 同模式，engine 不依赖 model）；
派发前（NextReadyTask 之后、占活跃位之前）对队首跑 triage——!startable → needs-info
（发 NEEDS-INFO 评论说明缺什么，人回复后 pollSignals 唤醒、重新分诊）；
needs_human_decision/!loop_doable → needs-human-decision；startable → 照跑。
TriageInput.Body + triage.md 全文段。

## Process
- 关键设计点：triage 放在**派发时**（just-in-time）而非摄入时——队首才分诊，看到的是
  spec re-ingest 热更新后的最新正文；摄入时不为每个新 issue 烧模型调用。
- triage 调用不走 budget Enforcer（Enforcer 是 per-run 作用域，triage 在 run 开始之前；
  一次小调用/次派发，与 collectIssueComments 同级）。
- triage 基础设施错误不 gate：记日志照跑——分诊器挂了不该阻塞整个 FIFO（可用性优先）。
- run-once 路径不接 gate：操作者显式单跑一个任务，分诊拦阻违背操作者意图。
- **抓到一个上游 bug**：triage 测试发现分诊器拿到的 Body 为空——issue-full-body arc 漏了
  ingest 的 InsertTask（新任务 Body 不落库，靠下轮 compare-and-update 回填两步走）。已在
  engine.go ingest 补上；并回写 [[issue-full-body]] 的 Lessons。

## Decisions
- Engine 加 TriageFunc 回调（与 RunTaskFunc 同模式，engine 不碰 model 包以外的东西——
  只依赖 skill.TriageOutput 类型）；nil = 无门（旧行为，测试兼容）。
- 挂起语义复用现有机器：needs-info / needs-human-decision 状态 + NEEDS-INFO /
  NEEDS-HUMAN-DECISION 评论 + pollSignals 轮询唤醒（新增两类 poll）+ 唤醒后重新分诊。
  没有发明新生命周期。
- resume/cancel 命令同步支持两个新状态（TUI 的 r/x 对挂起任务可用）。
- needs-human-decision 补 TUI 展示：状态符号 ℹ、蓝色（同 needs-info）、中文「待人工裁决」、
  概览排序与 needs-info 同组。

## Lessons
- 「死代码接线」类任务先验证它真的死了：triage 的注释声称「M3 daemon 调用」，实际从未
  接线——注释会撒谎，grep 调用点才是真相。
- 新写法的测试（TriageFunc 断言输入 Body）顺手抓到了上游 arc 的漏网 bug：给新消费者
  写端到端断言是审计既有链路的好办法。

## Related
- internal/daemon/engine.go（TriageFunc + tick 门 + parkByTriage + pollSignals 扩展 +
  applyCommand 两新状态 + ingest 补 Body）
- internal/skill/skill.go（TriageInput.Body）、internal/cli/embed/skills/triage.md（全文段）
- internal/cli/daemon.go（triageFn 接线——buildModels 第四个返回值首次被使用）
- internal/state/state.go（NeedsInfoTasks/NeedsHumanDecisionTasks + 概览排序）
- internal/tui/{styles,trace}.go（needs-human-decision 展示）
- 测试：engine_test.go TestTriageGate / TestTriageGateHumanDecision

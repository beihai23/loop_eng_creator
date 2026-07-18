---
arc: issue-full-body
started: 32be37c
status: resolved
commits: [667c3ca]
---

# issue 正文全文保留（不再只取首行 + 复选框）

## Intent
承接 [[plan-criteria-revision]] 里发现的信息损失：parseLocalTask 只从 issue 正文提取
首行（description）+ `- [ ]` 行（criteria），**其余叙述文字（背景、约束、上下文）在
摄入时就被丢弃**，plan/execute/verify 从来看不到。用户拍板：全文保留。

初始预期：channel.Task 加 Body（原始全文）；tasks 表加 body 列（best-effort ALTER，
随 spec re-ingest 的 compare-and-update 一并热更新）；PlanInput 加 Body 进 plan.md
模板；execute prompt 加全文段。verify（tier-2）暂不喂全文——它判的是 plan 承诺的
criteria 合同，全文已通过 plan 的修订间接影响 verify；记录为已考虑的延迟项。

## Process
- 回填点选在 `parseLocalTask`（local.go）而非各 channel 调用点：GitHub 的 parseIssuesJSON
  复用同一个解析函数，一处回填两个 channel 都拿到全文。
- 关键链路坑：daemon 的 runTask 从 **DB 的 TaskRow**（NextReadyTask）重建 channel.Task——
  只在 channel 层加 Body 不够，tasks 表必须加 body 列且 NextReadyTask/GetTask/scanTaskRows/
  TerminalTasks/TaskSpecsByRef 全部透传，否则 daemon 路径拿到空 Body（run-once 路径直接用
  ListNewTasks 的返回，反而天然没问题）。
- body 纳入 ingest 的 compare-and-update 比对：desc/criteria 没变、只改正文叙述（补背景/
  约束）也必须触发回写——这正是全文保留要服务的场景。存量任务（body=NULL）升级后首轮
  ingest 会统一回填一次（每个任务一条 spec updated 日志），属预期。

## Decisions
- **存储**：tasks 表加 body 列（best-effort ALTER，与 last_comment_at/run_id 同模式）；
  COALESCE(body,'') 读，旧行兼容。
- **喂给谁**：plan（plan.md 加「Issue 全文」段）+ execute（prompt 加全文段）；
  **tier-2 verify 暂不喂**——它判的是 plan 承诺的 criteria 合同，全文已通过 plan 的评审/
  修订间接影响 verify；直接喂全文会让 verify 越过合同按「意图」判，与 criteria 合同制冲突。
  记录为已考虑的延迟项。
- plan.md 模板用 `{{if .Body}}` 守卫：run-once/老数据无 body 时模板不渲染空段。

## Lessons
- 「在 channel 层加一个字段」和「字段真正到达模型」之间隔着整条持久化链路：channel →
  InsertTask → tasks 表 → NextReadyTask（daemon）→ channel.Task → SubLoop。改信息契约时
  必须把每一跳都列出来验证（本次靠 TestSubLoopFeedsFullBodyToPlanAndExecute 端到端钉死）。
- **续（triage-gate arc 抓到的漏网之鱼）**：本 arc 恰恰漏了 ingest 的 InsertTask 那一跳——
  新任务摄入时 Body 根本没落库，全靠下轮 compare-and-update 的「spec updated」回填，两步
  才完整且每个新任务多刷一条日志。当时「存量首轮回填属预期」的判断掩盖了它。triage-gate
  的测试（分诊器拿到的 Body 为空）把它揪了出来，已于 engine.go ingest 补上 `Body: t.Body`。
  教训：端到端测试要测「字段到达最终消费者」，只测存储层会漏链路中间的重建点。

## Related
- internal/channel/{channel,local}.go（Task.Body + parseLocalTask 回填）
- internal/state/state.go（body 列 + 全读查询透传 + UpdateTaskSpec 带 body）
- internal/daemon/engine.go（比对含 body）、internal/cli/daemon.go（runTask 透传）
- internal/skill/skill.go（PlanInput.Body）、internal/cli/embed/skills/plan.md（全文段）
- internal/loop/subloop.go（planIn.Body / execPrompt 全文段）
- 测试：state_test.go TestUpdateTaskSpec/TestTaskBodyRoundTrip、engine_test.go
  TestIngestResyncsBodyOnlyEdit、subloop_test.go TestSubLoopFeedsFullBodyToPlanAndExecute
- 上游：[[plan-criteria-revision]]（发现此问题）、[[spec-reingest]]（body 走同一热更新通道）

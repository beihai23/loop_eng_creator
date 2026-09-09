# loop-eng tier-3 成真 + agent→人移交包（HandoffPackage）

> 日期：2026-09-09
> 来源：两轮外部借鉴分析（Addy Osmani《Agentic Autonomy Levels》解读 + Baton（beihai23/Baton）交接设计）+
> 实施前代码勘察发现的三处 tier-3 结构性缺口。
> 状态：**P0 + P1 + P2 已交付**（全量 `go test ./...` 绿）。改动面：isolation（Create
> reuse-safe + WorktreePath）、loop（park 即 commit + land_branch + buildHandoff/
> extractEnvNotes + execPrompt 自报段）、verify（Handoff/HandoffAware + Chain 注入 +
> reviewRequest 移交包渲染）、cli（humanTierFor 接线 + acceptParkedReview 拦截）、
> README。P3+ 见 §4。

---

## 0. 背景

两轮分析的可执行结论收敛为：loop-eng 在机器↔机器方向（判决 priorFailure、现场
lastRejectedDiff、合同 lastPlanContract、历史 runHistory 四通道）已达到「现场级」交接，
**真正的缺口在 agent→人方向**——tier-3 人审请求和 blocked 求助还是「判决摘要级」。

但实施前勘察（2026-09-09）发现：写好一份人审评论之前，tier-3 本身在生产上是断的。

### 勘察发现的三处结构性缺口

1. **真 tier-3 从未接线（bug 级）。** `verify.Human`（发 review-request、产 NeedsHuman）
   仅存在于自身单测；生产两处 SubLoop 构造（`cli/run_once.go`、`cli/daemon.go`）只设置
   `Tier3Human` 布尔，而 `loop.tiersFor` 里该布尔只会挂 `verify.HumanStub`（自动通过占位）。
   结果：`tier3_human: true`（默认模板值）在生产行为 = 静默自动过，spec §8.6/§10 与
   README 承诺的「tier-3 异步人审 + park/resume」实际从未发生。daemon 侧的
   pollSignals/ParkedTasks/resume 机器整装待发，却等不到生产者。

2. **accept 语义缺失。** spec §8.6 写「人 accept → 写回 → done；reject → 带反馈重试」，
   但 resume 反馈只会变成下一轮 `priorFailure`（subloop.go:278-281）→ 重跑 → 再 park。
   即使接上线，人也**永远无法「接受并落地」**，只会人审→重跑→再人审的乒乓（每次回复
   烧一轮 plan/execute/verify token）。

3. **park 时现场不落盘。** needs-review 时 worktree 保留但路径不进任何持久字段
   （`Outcome.Worktree/Branch` 仅在 done 路径设置；report() 不携带）。accept-to-land
   无从找回现场。

## 1. P0：让 tier-3 成真（与 P1 同批交付，接线不带 accept 是脚枪）

### 0a. park 即 commit + Outcome 携带现场

subloop 的 NeedsHuman 分支：**先 commit**（复用 done 路径的 `commitWorktree`，
分支名 `branchName(taskID, attempt)`）再 park，`Outcome.Worktree/Branch` 照 done 路径
设置。理由：commit 后分支进主仓库对象库，落地（FF-merge / PR）**不再依赖 worktree
存活**（worktree 只是共享对象库的一个检出）；即使树被 GC，accept 仍可落地。

daemon 的 runTask 闭包：`out.Status == "needs-review"` 且 `out.Branch != ""` 时
`SetLandBranch(task.ID, out.Branch)`——复用现有 `land_branch` 列（reconcile 只看
done+open 的行，needs-review 不受影响；accept 消费时读取）。

commit 失败降级：维持现状 park（无 branch），accept 时自然回落到「带反馈重跑」路径。

### 0b. 接线真 tier-3（按通道能力）

cli 层新增 `humanTierFor(cfg, ch, ref)`：`tier3_human=true` 且 provider ∈ {github,
linear}（ListReplies 有真实实现）→ 注入 `verify.Human{Ch, Ref}`；**local 不接**——
local 的 ListReplies 是 no-op（local.go:81），park 后无回复通道，接了只会搁浅任务
（TUI 'r' 可救但体验为隐性依赖）。local 保持 stub，并在计划/README 记录原因。
run_once 与 daemon 两处构造共用该 helper。

### 0c. accept 令牌：`loop:accept`

与既有 `loop:task` / `loop:running` / `loop:blocked` 令牌族一致，语言中立、可 grep。

- **评论教学**：review-request 评论（P1 渲染器）末尾明示：「回复含 `loop:accept` =
  按现状接受并自动落地（FF-merge / PR）；回复其他内容 = 驳回并按反馈重做」。
- **检测放 cli 层 runTask（engine 零改动）**：resume 重排本身不变（needs-review → new），
  但 runTask 开头先 `GetResumeFeedback`（peek 不清）：含 `loop:accept` 且 land_branch
  非空 → **跳过整个 SubLoop.Run**，直接走既有落地（本地 FF-merge 关单 / GitHub
  createPR + finalizeLand）→ 返回 done；`PopResumeFeedback` 清掉反馈。
  无令牌 → 照旧进 SubLoop.Run 带反馈重跑（reject 语义）。

关键正确性：**accept 绝不重跑 loop**——重跑会从 HEAD 新建 worktree 重做实现，
产出的可能是人没见过的新代码；accept 必须落地人审过的那份 commit。

## 2. P1：移交包（本计划的主目标，Baton HandoffPackage 字段映射）

### 数据结构（verify 包）

```go
// Handoff 是 agent→人的移交包：tier-3 人审请求从「判决摘要」升级为结构化完整现场。
type Handoff struct {
    Attempt, MaxRetries int           // 第几轮 / 共几轮
    Worktree, Branch    string        // 工作现场（park 前已 commit，分支在主仓库对象库）
    RunHistory          string        // 历轮 run 摘要（DB 构建，有界）
    EnvNotes            string        // execute 自报的「环境与复现」段（宽松提取，可空）
    Risks               []string      // plan 识别的风险（open threads：接手方必读）
    CriteriaNotes       string        // plan 修订验收标准的理由（可空）
    TierOutcomes        []TierOutcome // 本轮 tier-1/2 判词（Chain 注入：机器已验过什么）
    TokensIn, TokensOut int           // 本 run 累计 token（透明度）
}

// HandoffAware 由消费移交包的 tier 实现（tier-3）。Tier.Check 冻结签名不动
// （llm.go / budget/client.go 三处明言 frozen contract）。
type HandoffAware interface{ SetHandoff(Handoff) }
```

### 注入通道

`verify.Chain` 增第 6 参 `ho Handoff`：循环内对实现 HandoffAware 的 tier，注入
快照（TierOutcomes = 已跑过的 tier 结果副本）。调用方仅 subloop.go:565 一处生产
+ verify 包内 4 处测试补零值。

### reviewRequest 重渲染（保留 `## REVIEW-REQUEST` 头——botcomment.go 识别清单依赖）

结构（人扫读优先级排序）：移交包元信息（轮次/token/现场）→ 验收标准 →
**机器已验过什么**（tier-1/2 判词：人不用重做机器已做的）→ diff（60 行 → 200 行，
截断指向完整版）→ 环境与复现 → 风险/标准修订 → 历轮记录 → 要人判断的点 +
**accept/驳回操作教学**（0c 令牌）。

### subloop 侧 buildHandoff（新文件 internal/loop/handoff.go）

verify 调用点组装：attempt/MaxRetries/wt/branch、runHistory（已在作用域）、
Risks=planOut.Risks、CriteriaNotes（criteriaRevised 时）、token 累计（run 内
plan/execute/tier-2 三处 Usage 求和，注释说明不含 help）、EnvNotes=extractEnvNotes(execOut)。

## 3. P2：execute 自报「环境与复现」段

execute 不是 skill（直接 `claude -p`），指令在 subloop 的 execPrompt 组装处：
要求自报末尾附 `### 环境与复现`（装的依赖 / 如何运行验证 / 注意事项）。
`extractEnvNotes` 宽松提取（找标题行，截到下一同级或更高级标题；找不到返回空串，
旧格式自报按「无环境说明」渲染，绝不阻塞）——与 tier-1 契约「宽容调用面」同款纪律。

## 4. P3+ 延后（记录不实施）

- **metrics 聚合命令**（`loop-eng metrics`）：SQLite 已有全部数据——重试率、tier-2
  拒绝率、人审拒绝率（缺陷逃逸）、每个 done 的 token 成本、park 时长（干预间平均时间）。
- **blocked 求助评论同款补齐**：已有 help 三字段 + sceneKept 路径，补 env/runHistory/token。
- **自主性派生**：`tier3_human` 从全局静态开关变为按任务验证强度派生（triage 判
  停止条件可判定性；不可判定 → needs-info 钉标准，而非进循环烧预算）。
- **任务 schema 契约扩展**：non-goals / scope（允许改动路径，为 L4 并行备好所有权
  原语）/ per-task budget 覆盖。

## 5. 明确不做（及理由）

- **黑板 / 抢单 / 主驾副驾 / rev 乐观锁 / 幂等键**（Baton 复杂度属「平等多 agent
  并发协作」；loop-eng 是固定流水线 + 单活跃，搬来只有编排税）。
- **local 通道真 tier-3**（无回复通道，见 0b）。
- **progress percent 自评**（流水线进度=客观阶段状态机）。
- **上下文搬进 issue 卡片 JSON**（DB 结构化 + 定向注入 + 有界截断 优于通用卡片）。

## 6. 验收

- 每阶段附测试：0a（needs-review detail/Outcome 携带 branch）、0b（humanTierFor
  三态）、0c（accept 令牌 → runTask 落地短路；无令牌 → 照旧重跑）、P1（Chain 注入、
  渲染器分段、buildHandoff、extractEnvNotes 宽松性）、P2（提取边界）。
- `make test`（CGO_ENABLED=0 go test ./...）全绿；gofmt 仅限本次新建/改动文件。
- README 的 tier-3 描述从「承诺」变为「真实」：补 local 通道限制与 accept 令牌说明。

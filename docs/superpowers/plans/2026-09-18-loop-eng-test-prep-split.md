# M1：出题权分离 —— test-prep 角色（M2：争议路由，同文档续）

日期：2026-09-18。来源：2026-09-17 设计讨论（出题权与实施权分离 / 争议路由 / 盲出题的第一性依据）。

## 背景

现状：`PlanOutput` 同时携带 `VerifyScript`（tier-1 脚本，全仓唯一来源）与
`RevisedCriteria`/`CriteriaNotes`（验收合同修订权）——plan 一个人既写实施合同又出考卷。
「判卷」环节早有结构独立（tier-2 新鲜会话、只喂 diff+标准）；「出题」环节没有。
plan-criteria-revision arc 记录的软肋：「防放水是 prompt 铁律 + 审计轨迹，不是结构约束」。

本里程碑把出题权从实施侧整体移到独立角色 test-prep，把软约束变硬约束：
**实施方的规划输出不得是验收合同的输入**（原则 3 的向上延伸）。

## 设计决定（讨论已拍板）

1. **盲的精确边界**：被考方是实施侧联合产物（diff 凝固 plan 的解读；直接被考人
   execute，间接被考方 plan）。串行时序下 diff 尚不存在，对实现的盲是天然的；
   **plan 输出是唯一需要立法的泄露通道**。test-prep 输入白名单：
   `{需求全文, 标准原件, repo HEAD, 上轮考卷}`——白名单之外一切不进。
   出题盲（对 plan），判卷不盲（tier-2 对 diff）。
2. **串行不并行**：独立性来自喂什么，不来自什么时候跑。保持「单活跃、FIFO」不变量。
   并行化挂 M3，需墙钟数据证明收益。
3. **调用时机 = 方案 a**：test-prep 每轮 attempt 重跑（同构于 plan 的每轮重读状态，
   零状态分支），输入带上轮考卷，铁律「默认沿用，仅驳回证据证明考卷错误才修订」。
   M2 的争议修订与首考是同一机制。
4. **可用性优先**（triage-gate arc 同款先例）：test-prep 基础设施错误不 gate loop——
   记 step fail、回落 issue 原版标准、tier-1 缺席（tier-2 判），照常跑。
5. **M2 class=exam 时暂不跳过 re-execute**：保守第一版，max_retries 兜底；
   分类误判率数据出来后再优化（挂 M3）。
6. **config 开关 opt-in**：`models.test_prep` 空 = legacy（plan 兼出题，行为与旧版
   逐字节一致）；非空 = 启用。这就是回滚开关。
7. **不改名 `PlanVerifyScript`**：preflight 冻结签名钉着它（改前须先改测试）；
   test-prep 复用同型。改名留到冻结契约修订时一起。

## 改动清单

| 触点 | 内容 |
|---|---|
| `internal/skill/skill.go` | `TestPrepInput{Task, AcceptanceCriteria 原件, Body, PriorExam}` / `TestPrepOutput{Criteria, CriteriaNotes, VerifyScript, Risks}`；`PlanInput.ExamSeparate`（模板条件化用） |
| `internal/config/config.go` | `Models.TestPrep ModelRef yaml:"test_prep"`（可选）；`ModelRef.IsZero()`；validate：配置了但缺 name/binary → 硬错误 |
| `internal/budget/budget.go` | `roleFloor["test-prep"] = 20000` |
| `internal/cli/embed/skills/test-prep.md` | 新 prompt：白名单制 + 出题铁律（可观测判据 / 签名只钉已印证稳定接口 / 删除给理由 / 默认沿用上轮考卷 / 反模式=通用套件假绿灯） |
| `internal/cli/embed/skills/plan.md` | `{{if .ExamSeparate}}` 条件化：启用时移除验收标准评审 + verify_script 职责段，产出瘦身为 plan+risks+agent_hints |
| `internal/state/state.go` | `LatestTestPrepOutputByRef`（镜像 LatestPlanOutputByRef，role='test-prep'） |
| `internal/loop/subloop.go` | 启用路径：plan 后插 test-prep step（seq attempt*10+2；execute/verify 顺延 +3/+4，legacy 布局不变）；effTask 食源换 exam.Criteria；tiersFor 改收有效脚本；done 战报附出题说明；RetryDiagnosis 仅 legacy 注入 |
| `internal/loop/testprep.go`（新） | runTestPrep（预算/记账/trace/错误回落）、loadPriorExam（跨 run 回灌，≤8000 runes） |
| `internal/loop/contract.go` | `executeContractSection(planOut, script)`：脚本段与 plan 步骤解耦，启用时展示 test-prep 考卷 |
| `internal/loop/handoff.go` | buildHandoff 增 exam 参数：Risks 合并、CriteriaNotes 按 exam 优先 |
| `internal/cli/run_once.go` | buildModels 第 6 返回值 tp（未配置=nil，raw client 与 plan 同款手动记账）；applyTaskAgent 经 forProviderIfSet 保持未配置不误启用；run-once SubLoop 接线 |
| `internal/cli/daemon.go` | runTask SubLoop 接线 TestPrep/TestPrepModelRef |
| `internal/cli/config_wizard.go` | TUI 向导新增「出题权分离（可选）」取舍步 + 引擎选择步：默认不启用；再配置场景光标预置「启用」（回车=维持现状，不静默抹配置）；显式不启用 → 保存时清零；启用写入强制 `readonly: true`；确认页摘要含 test-prep 行 |
| `internal/cli/doctor.go` | providerPreflight 对 test-prep 配置了才体检 CLI（零值走 legacy 无可查） |
| `internal/cli/config.go` | roleMeaning 补 test-prep 一行（管道式非交互流程不动——行协议兼容，新增提问会移位既有 piped 脚本的输入行） |
| `internal/skill/registry.go` | Defaults 加 test-prep 条目 |
| TUI trace | role 原文渲染，自动兼容；in_flight 值 "test-prep" 原样显示，无代码改动 |

## 明确不做（M1 范围外）

- 并行化（M3，数据驱动）；争议路由/verify 分类（M2）；管道式非交互 config 流程
  加 test-prep 提问（piped 行协议兼容优先，TUI 向导已覆盖交互场景）；
  `PlanVerifyScript` 改名；跳过 re-execute 优化（M3）。
  （wizard 原划在范围外，实施日补齐——向导是主配置入口，配不出来的特性等于不存在。）
- 已知 M1 局限：结构性不可满足的标准在启用路径下暂无 plan 修订权兜底
  （修订权移交 test-prep，其知情修订依赖 M2 争议包）——重试耗尽 → blocked，记录在案。

## 验收标准

- `go test ./...` 全绿（硬门槛；trace-empty-runs 为已知时间 flake）。
- legacy 路径零改动：`models.test_prep` 不配置时，全部既有测试不改一行通过。
- 启用路径单测：step 布局（+1/+2/+3/+4）、effTask 食源、tier-1 脚本来自考卷、
  错误回落、PriorExam 回灌（run 内 + 跨 run）、execute 契约段展示考卷。
- 观察项（试点后盯）：script Valid()=false 率、done 率、放水案例、出题说明人审覆盖率。

## 风险记录（失败模式换方向）

从「plan 放水（看不见）」换成「test-prep 盲出题结构性失败（空转，看得见）」。
管理机制：判据可观测化铁律、知情修订（M2）、M3 拨盘（开放 plan 冻结签名给 test-prep）。
本次权力转移与 plan-criteria-revision arc 拍板方向相反（从实施侧收回出题权），
对错靠数据说话；该争议记录保留在案，不回摆。

---

# M2：争议路由（同日落地）

分歧路由表（考卷之争先行，事实之争挂 M3，需求之争零新基建）：

| 争端 | 归因 class | 裁决者 | 机制 |
|---|---|---|---|
| 实现缺陷 | work（缺省） | 重试 | 现行路径一行不改 |
| 考卷缺陷 | exam | test-prep | 争议包回灌 DisputePacket → 知情修订（首考仍盲） |
| 需求缺陷 | requirement | 人 | needs-human-decision 挂起（复用 triage-gate 全套基建） |

**改动**：`skill.FailureClass` + `VerifyOutput.FailureClasses`（可选字段，旧输出
nil=全 work 零兼容）；`verify.VerifyResult` 透传 + `validClasses` 过滤（非法
class/空 criterion 丢弃，LLM 输出不可信）；verify.md 归因指令（拿不准一律 work
——不轻易上交人）；`loop/dispute.go`（examDisputeOf facts-only，修订纪律在模板
承载避免两处指令漂移）；subloop reject 路径三路分流（requirement 先于驳回评论，
树即弃，diff 已在 output_json）；test-prep.md 修订模式条件块（白名单的唯一设计
例外：判卷人指控出题人出错，出题人理应看到指控）；驳回评论附「归因分类」段；
engine 日志分支。

**有界性**：零新增调用——争议修订搭每轮 test-prep 既有调用（方案 a）；max_retries
照旧兜底；requirement 挂起后 resume 经 PopResumeFeedback 进 priorFailure、重新分诊。

**观察项**：verify 的分类质量（work 误判成 exam → 考卷被无谓修订，防线=修订必附
诊断+审计+tier-3）；requirement 挂起率（过高=verify 把难度当需求缺陷）；争议
不成立率（test-prep 说「争议不成立」的比例——高=verify 乱指控）。

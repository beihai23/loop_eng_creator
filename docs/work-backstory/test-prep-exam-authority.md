---
arc: test-prep-exam-authority
started: ef8f477
status: open # 代码已落地（M1+M2），观察清单等试点数据；数据到后追记再收弧
commits: [3ceb19a, fb02a14] # M1 出题权分离 / M2 争议路由
---

# 出题权分离：test-prep 角色（M1）

## Intent
起因：用户观察到 loop 的 plan/execute/verify 是线性流水线，想改成真实团队形态——
「需求同步清楚后，开发与测试分头行动，测试出题、开发实施、测试验收」。讨论收敛为：
**分权是好主意，分身不是**——把两个捆绑的改动拆开：
- 改动 A（出题权独立）：价值高、成本低 → M1 落地；
- 改动 B（与 execute 并行 fork）：价值低（LLM 调用成本下关键路径恒为 execute）、
  成本高（破「单活跃、FIFO」设计决策）→ 挂 M3，数据驱动。

问题定义：判卷环节早有结构独立（tier-2 新鲜会话、只喂 diff+标准），但**出题**环节
没有——`PlanOutput.VerifyScript` + `RevisedCriteria` 都出自 plan，出题人=半个做题人。
[[plan-criteria-revision]] 记录的软肋「防放水是 prompt 铁律+审计，不是结构约束」
正是指这里。M1 把出题权移到独立角色 test-prep，把软约束变硬约束：**实施方的规划
输出不得是验收合同的输入**（原则 3 的向上延伸）。

## Process（关键设计澄清，讨论中用户两处追问推动）
- **「被考方是谁」**：我最初说「plan 是被考方」，用户纠正「被考的是 executor」。
  精确结论：直接被考人是 execute（考卷考 diff），但 diff 凝固着 plan 的解读，
  plan 是间接被考方——被考方是**实施侧联合产物**。由此得出更强的禁令依据：
  plan 的输出本身就是被考内容的一部分（它对需求的解读对不对，正是考卷要能抓出的
  错误类型），被考内容进考卷=泄题。且串行时序下 diff 尚不存在，对实现的盲是天然的，
  **plan 输出是唯一在时序上先于出题、又属于实施侧的产物——禁令只需落在它身上**。
  故 test-prep 输入用**白名单制**（需求全文/标准原件/repo HEAD/上轮考卷），不写黑名单。
- **「盲出题是不是黑盒测试」**：不精确。准确原则是「考卷从被要求的导出，不从被
  打算的导出」——出题盲（对 plan），判卷不盲（tier-2 对 diff）。盲是买独立性付的
  代价（考卷与实施可能各自想象不收敛，#71 的镜像风险），管理机制三件套：
  判据可观测化铁律、M2 知情修订、M3 拨盘（开放 plan 冻结签名给 test-prep）。
- **考卷产出的消费点**：复用 effTask 既有流向（execute prompt / verify.Chain /
  verifyFailComment / tier-3 评论）——[[plan-criteria-revision]] 的教训（盘点全部
  消费点，漏一个就撕裂）直接复用，只换食源。
- **测试方案（用户质疑「要重新规划吗」后定稿）**：框架不推翻，三处修订进方案
  （跨 run 考卷回灌 / 白名单制 prompt / 精确文档措辞）+ 一个拍板点：test-prep 每轮
  attempt 重跑（方案 a，与 plan 每轮重读状态同构、零状态分支、M2 争议修订与首考
  同机制）vs 仅首轮出题（省调用但有状态分支）→ 拍板 a。

## 实施中的决定
- **config 开关 opt-in**：`models.test_prep` 零值=legacy（plan 兼出题，旧行为逐字节
  一致，全套旧测试零改动通过——已验证），非零=启用。这就是回滚开关。
- **可用性优先**（triage-gate arc 同款）：test-prep 基础设施错误/空考卷不 gate loop
  ——step 记 fail、回落 issue 原版标准、tier-1 缺席（tier-2 判）。
- **不改名 `PlanVerifyScript`**：preflight 冻结签名钉着它；test-prep 复用同型，
  改名留到冻结契约修订时一起。
- **seq 布局**：legacy +1/+2/+3 不变；启用时 +1/+2/+3/+4（test-prep 占 +2，
  execute/verify 顺延）。retry-gate 恒 +9。
- **RetryDiagnosis 抑制**：启用路径不再给 plan 注入重试诊断——该元指令指向
  revised_criteria，plan 已无可行使的修订权（M1 已知局限：结构性不可满足的标准
  暂由 blocked 兜底，知情修订走 M2）。
- **`forProviderIfSet`**：任务级 agent override 对未配置的 test-prep 保零值——
  否则 `agent: codex` 会把 legacy 任务误推进出题权分离模式。
- **buildModels(nil cfg) 容忍**：既有测试传 nil config，tpEnabled 加 nil 保护，
  legacy 行为不变。
- **wizard 补齐（实施日追加，原划范围外）**：用户问「config 过程也改了吗」戳中
  要害——向导是主配置入口，配不出来的特性等于不存在。新增取舍步（默认不启用，
  光标随现状预置：再配置回车=维持现状不静默抹配置；显式拒绝才清零）+ 引擎选择步
  （readonly 强制 true）。管道式非交互流程**不动**：piped stdin 有行协议兼容包袱，
  新增提问会移位既有脚本的输入行；TUI 向导已覆盖交互场景。
- **wizard 流程测试的修法**：所有走到 confirm 的流程测试补一次回车（新取舍步默认
  不启用）。教训：向导加步骤是「行协议变更」，TUI 键序测试全体要跟——加步骤前
  先 grep 所有 drive() 序列。

## 观察清单（试点后要盯的）
1. 考卷 script `Valid()`=false 率（盲出题的结构性失败信号；高了开 M3 拨盘）。
2. done 率 vs legacy 期（空转减少 vs 盲出题新失败）。
3. 放水案例（出题说明被 tier-3 抽查；criteria_notes 是否真被读）。
4. M2 分类质量：work 误判成 exam（考卷被无谓修订）、requirement 挂起率
   （过高=verify 把难度当需求缺陷）、争议不成立率（高=verify 乱指控）。

## M2 追记（同日落地：争议路由）
- **关键机制发现**：引擎按 `AppendTransition(running → status)` 落库决定挂起桶，
  `needs-human-decision` 的评论/打标/pollSignals/resume/重新分诊在 triage-gate
  arc 已全部建好——SubLoop 返回该状态即完成 requirement 路由，引擎只补了一行日志。
  「复用基建」的判断被证实到极致。
- **争议包是 facts-only**（examDisputeOf 只装指控+证据），修订纪律全部住在
  test-prep.md 的条件块里——指令单点承载，避免 Go 侧与模板侧两处各说各话漂移。
- **知情修订的立法措辞**：白名单的例外必须显式自洽——「判卷人指控出题人出错，
  出题人理应看到指控」；模板里「你看不到实现侧」的段落与争议段并存，靠这句
  把张力说破，模型才不会困惑。
- 分类质量防线：validClasses 解析端过滤（非法 class 丢弃）；「拿不准一律 work」
  写进 verify.md（重试便宜，上交人贵）；修订必附诊断（#47 标准），不成立时
  test-prep 要写「争议不成立」——把 verify 的乱指控也变成可观测信号。

## Lessons
- 「分权 vs 分身」要先拆开估值：独立性来自**喂什么**（信息流），并行只关乎
  **什么时候跑**（调度）——串行零损失独立性，两者根本正交。
- 用户对概念措辞的追问（被考方是谁）会直接改变设计的立法精度：黑名单→白名单、
  禁令范围从角色收敛到「时序上唯一存在的泄露通道」。
- 失败模式换方向记录：从「plan 放水（看不见）」换成「盲出题结构性失败（空转，
  看得见）」。方向可逆（config 开关），对错靠数据说话——与
  [[plan-criteria-revision]] 的拍板方向相反，争议记录保留，不回摆。

## spec 更新（宪法同步，M1+M2 落地同日）
core-design spec 是现行真相的载体，落地后必须同步（否则下一个读者按旧宪法理解系统）：
- §8.4 编排：计划→（出题）→执行→验证；test-prep 白名单制与立法依据浓缩进来；
- §8.5：四 skill → 五 skill（test-prep 条目含三态输入：正常/沿用/争议修订）；
- §8.6：tier-1 食源改「本轮生效的验收脚本」；新增争议路由段（三路+有界性）；
  原则 3 延伸句（实施方规划输出不得是验收合同的输入）；
- §8.10：四角色→五角色（test-prep 可选强制 readonly）；合同可见性段补考卷回灌；
- 示例 config：删陈旧的 `verify.deterministic` 静态列表，加注释的 test_prep 行。
教训：改「信息契约」类功能时，spec 里的示例 config 和 §10 的分诊措辞也是契约的
消费点——陈旧的「配置的脚本」字样会误导下一个读者，全仓 grep 一遍才敢说同步完。

## Related
- docs/superpowers/plans/2026-09-18-loop-eng-test-prep-split.md（方案与拍板记录）
- internal/loop/testprep.go（runTestPrep/loadPriorExam/examJSONOf/retryDiagnosisForAttempt）
- internal/loop/subloop.go（食源分叉点：effTask/effScript/examNotes；seq 布局）
- internal/cli/embed/skills/test-prep.md（白名单+出题铁律；改 embed 须 rebuild）
- internal/cli/embed/skills/plan.md（{{.ExamSeparate}} 条件化）
- 测试：loop/testprep_test.go、config/testprep_test.go、cli/testprep_test.go、
  loop/handoff_test.go TestBuildHandoffExamAudit
- 上游：[[plan-criteria-revision]]（修订权语义的来源与争议）、[[triage-gate]]
  （可用性优先先例）、[[tier1-contract-is-planner-imagined]]（签名漂移的镜像风险）

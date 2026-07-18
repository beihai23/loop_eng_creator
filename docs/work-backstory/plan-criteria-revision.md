---
arc: plan-criteria-revision
started: 4149e3b
status: resolved
commits: [eafa238]
---

# plan 对验收标准的独立评审与修订权

## Intent
起因：用户指出「plan 的职责是规划任务的实施和验收，所以它对接收到的信息要有自己的独立
判断」。现状盘点（2026-07-18）：issue 验收标准在 plan/execute/verify 三个环节都是原文
直通——plan.md 模板没有任何让 plan 评审标准本身的指令，verify.Chain 直接吃
task.AcceptanceCriteria。设计初衷是「标准=ground truth 合同，plan 只管 HOW」，防 loop
自我放水；代价是含糊/矛盾/不可判的标准无人批判性 engagement，只能 tier-2 反复驳回后由人
从战报里发现。

用户拍板：**plan 可修订标准**（三选项中最激进的一档），接受「verify 按 plan 修订版判」，
由 tier-3 人审做兜底。

初始预期：PlanOutput 加 RevisedCriteria（nil=未修订，非 nil=plan 承诺的最终合同）+
CriteriaNotes（修订理由，审计用）；plan.md 增加标准评审职责与防放水约束；SubLoop 下游
（execute prompt / verify.Chain / verifyFailComment）统一改用修订后标准；plan step 落
OutputJSON 使修订可审计。

## Process
- 用户追问「合同是指 issue 正文吗」时发现更大的信息损失：parseLocalTask 只提取正文首行
  （description）+ `- [ ]` 行（criteria），**issue 正文的其余叙述文字在摄入时就丢了**，
  plan/execute/verify 从来看不到。本次不做全文保留（另一个改动），但记录在案。
- 签名决策：verifyFailComment(task, attempt, res) 签名被两个测试文件钉死，不改签名——
  SubLoop 在调用点构造 effTask（criteria 替换为修订版）传入，语义自然流动，旧测试零改动。

## 争议（2026-07-18 复盘，用户要求记录在案）

这是本轮四个改进中唯一**权力转移**（而非信息保真）的改动，意义尚未被证明：

- **失败模式换了方向**：从「loop 在烂标准上空转到 blocked（看得见、人会介入）」换成
  「loop 可能悄悄按放水后的标准给自己判过（看不见，除非人审读修订说明）」。
- **防放水是 prompt 铁律 + 审计轨迹，不是结构约束**——成立依赖两个前提：plan 模型守
  规矩；`Tier3Human` 真的开着且有人读 done 战报里的修订说明。Tier3Human 关着跑时，
  这条的风险是实打实的。
- 实施时 Claude 推荐的是保守档（评审+上报、不改合同），用户拍板激进档（plan 可修订、
  verify 按修订版判）。方向是用户定的，对错要靠数据说话。

**观察清单（后续 run 要盯的）**：
1. plan 到底修不修订——从不行使 = 无害死代码；经常修订 = 抽查修订质量。
2. 修订质量：是否忠于任务意图，有没有「删除实质性要求」的放水案例。
3. done 率变化：含糊标准任务的空转减少 vs. 质量回退。
4. done 战报的修订说明是否真的被人读到（人审流程是否覆盖）。
5. 首个样本：task #31（2026-07-18 重触发，新二进制首轮实战）。

**第一批数据（2026-07-18，#46/#47 实战）**：
- **修订权零行使**：两个任务的 plan 输出（共 5 轮）均无 revised_criteria。尤其 #47——
  verify 按字面死磕「buildChannel 必须出 diff」（其实调用已兼容、合法为空），这是
  修订权的标准使用场景，plan 三轮都没用。目前落在「无害死代码」分支，样本仍少。
- 已在 #47 的 issue 评论里**显式提示** plan 可行使 revised_criteria，看重触发后是否
  行使——这能区分「模型不知道有这个权力」vs「知道但不用」。
- 详见 [[plan-execute-contract-drift]]。

## Decisions
- **RevisedCriteria 用 \*[]string 而非 []string**：nil=「未修订」（沿用 issue 原版）与
  「修订成空清单」必须可区分——encoding/json 下裸切片做不到。
- **修订版流向三处下游**：execute prompt（含 CriteriaNotes，告诉 execute 为什么改）、
  verify.Chain 判据、verifyFailComment 的改进建议基准。issue 原始标准只保留在 tasks 表
  （合同原件）与 plan prompt 里（评审对象）。
- **防放水不靠代码靠审计**：prompt 铁律（忠于任务意图/删除必须给理由/拿不准就保留并标
  risks）+ 三重审计轨迹——plan step output_json 落盘、done 战报附修订说明、tier-3 人审。
  代码层不做「修订幅度」硬校验（语义问题，硬规则只会误伤）。
- **未修订（nil）路径行为与旧版完全一致**——存量任务与全部旧测试零影响。

## Lessons
- 改「信息契约」类功能时，先盘点该信息的全部消费点（这次：plan/execute/verify/驳回评论
  四个），漏一个就会出现「plan 修订了但 verify 还按原版判」的撕裂。
- parseLocalTask 只取正文首行 + `- [ ]` 行，issue 正文其余叙述在摄入时即丢失——这是比
  「标准照单全收」更上游的信息损失，值得作为独立 arc 处理（全文保留 or 结构化段落）。

## Related
- internal/skill/skill.go（PlanOutput.RevisedCriteria/CriteriaNotes）
- internal/cli/embed/skills/plan.md（评审职责 + 防放水铁律 + 示例；改 embed 须 rebuild）
- internal/loop/subloop.go（effTask 有效标准流 + plan step output_json + done 战报附注）
- 测试：subloop_test.go TestSubLoopPlanRevisedCriteria
- 上游：[[spec-reingest]]（正文热更新后，plan 评审的对象才是新鲜的）

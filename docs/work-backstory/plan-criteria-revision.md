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

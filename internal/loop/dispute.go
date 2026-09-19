package loop

// dispute.go —— M2 争议路由：verify 驳回的归因分类 → 分流。
//
// 分歧路由表（设计定稿，见 docs/superpowers/plans/2026-09-18-loop-eng-test-prep-split.md）：
//   work        → 现行重试路径，一行不改（分类缺省即 work，旧输出零兼容成本）；
//   exam        → 争议包回灌下一轮 test-prep 的 DisputePacket，知情修订（首考仍盲）；
//   requirement → 需求本身矛盾/缺信息：重试无意义（实现与考卷都可能没错），
//                 挂 needs-human-decision 等人裁决——复用 triage-gate 的全部基建
//                 （评论/打标/pollSignals/resume/重新分诊），SubLoop 只负责返回该状态。
//
// 有界性：不新增预算机器——max_retries 就是界；争议修订搭每轮 test-prep 的既有
// 调用（方案 a），零新增调用。审计：争议包全文进 test-prep step 的 input_json，
// 修订说明进 output_json 与战报——tier-3 人审的复核对象。

import (
	"fmt"
	"strings"

	"loop-eng/internal/skill"
	"loop-eng/internal/verify"
)

// hasClass 报告归因分类里是否出现某个类。
func hasClass(classes []skill.FailureClass, class string) bool {
	for _, fc := range classes {
		if fc.Class == class {
			return true
		}
	}
	return false
}

// maxDisputeEvidenceRunes 是争议包里单条证据的截断上限（争议包进 prompt，
// 不能被一条长证据吃光预算；完整驳回 detail 在 verify step 的 output_json）。
const maxDisputeEvidenceRunes = 500

// examDisputeOf 从 verify 结果提取 exam 类指控，构造争议包（facts only——修订
// 纪律由 test-prep.md 的条件块承载，这里不带指令，避免两处指令漂移）。
// 无 exam 类指控返回空串（下一轮 test-prep 走正常出题，不进修订模式）。
func examDisputeOf(res verify.VerifyResult) string {
	var b strings.Builder
	first := true
	for _, fc := range res.FailureClasses {
		if fc.Class != "exam" {
			continue
		}
		if first {
			b.WriteString("verify 驳回理由（原文）:\n" + strings.TrimSpace(res.Detail) + "\n\n归因指控（class=exam，考卷缺陷）:\n")
			first = false
		}
		fmt.Fprintf(&b, "- 标准: %s\n  证据: %s\n", truncateStr(fc.Criterion, maxDisputeEvidenceRunes), truncateStr(fc.Evidence, maxDisputeEvidenceRunes))
	}
	return strings.TrimSpace(b.String())
}

// requirementDisputeComment 构造 needs-human-decision 的战报 detail（report 会
// 加 "NEEDS-HUMAN-DECISION: " 前缀）：逐条列出被判为需求问题的标准 + 证据，
// 其余驳回理由附后，给人一个可照着回复的裁决入口。
func requirementDisputeComment(res verify.VerifyResult) string {
	var b strings.Builder
	b.WriteString("verify 判定以下验收标准疑似**需求本身的问题**（矛盾/缺关键信息），重试无法自愈，需要人裁决：\n")
	for _, fc := range res.FailureClasses {
		if fc.Class != "requirement" {
			continue
		}
		fmt.Fprintf(&b, "- 标准: %s\n  证据: %s\n", truncateStr(fc.Criterion, maxDisputeEvidenceRunes), truncateStr(fc.Evidence, maxDisputeEvidenceRunes))
	}
	if d := strings.TrimSpace(res.Detail); d != "" {
		b.WriteString("\n完整驳回理由:\n" + truncateStr(d, 2000) + "\n")
	}
	b.WriteString("\n请在本 issue 回复裁决（修改验收标准 / 补充需求 / 确认放弃）；daemon 会拾起回复重新派发。")
	return b.String()
}

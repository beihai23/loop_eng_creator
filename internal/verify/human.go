package verify

import (
	"context"
	"fmt"
	"strings"

	"loop-eng/internal/channel"
)

// Human is the real tier-3 tier (spec §8.6/§10): an asynchronous human review
// that never blocks the SubLoop. Check posts a review-request comment on the
// task's ticket, then returns {Passed:false, NeedsHuman:true}. SubLoop routes
// NeedsHuman → needs-review (park, release the active slot, commit + keep the
// worktree); the daemon later polls channel.ListReplies and resumes the task
// carrying the human's feedback.
//
// 评论体是结构化移交包（Handoff，经 Chain 注入）：接手的人拿到「机器已验过什么
// + 工作现场 + 环境复现 + 风险 + 历轮 + 接受/驳回操作」的完整现场，而非一份
// diff 摘要。零值包（未注入/测试直构）按空段省略，评论仍有标准 + diff + 判断点。
type Human struct {
	Ch  channel.Channel
	Ref string
	// pkg 是 Chain 注入的移交包（SetHandoff）。值拷贝于 Check，注入只发生在
	// Check 前（Chain 循环内），无并发面。
	pkg Handoff
}

// SetHandoff 实现 HandoffAware（指针接收者：注入要改 pkg）。Chain 在每次 Check
// 前调用，传完整快照。
func (h *Human) SetHandoff(p Handoff) { h.pkg = p }

func (h Human) Check(ctx context.Context, diff string, criteria []string, priorFailure string) (VerifyResult, error) {
	body := channel.MarkBotComment(reviewRequest(h.pkg, diff, criteria, priorFailure))
	if err := h.Ch.PostComment(ctx, h.Ref, body); err != nil {
		return VerifyResult{}, err
	}
	return VerifyResult{
		Passed:     false,
		NeedsHuman: true,
		Detail:     "tier-3 人审已请求：已发 review-request 评论",
	}, nil
}

// reviewRequest 渲染 tier-3 人审评论。段序按人扫读优先级：交接现场（轮次/token/
// 分支）→ 验收标准 → 机器已验过（人不必重做）→ diff → 环境复现 → 风险 → 历轮 →
// 操作指引（accept 令牌 / 驳回反馈）。空字段按段省略——零值包仍有可用评论。
func reviewRequest(ho Handoff, diff string, criteria []string, priorFailure string) string {
	var b strings.Builder
	b.WriteString("## REVIEW-REQUEST（tier-3 人审）\n\n")
	if s := sceneSection(ho); s != "" {
		b.WriteString(s + "\n")
	}
	b.WriteString("### 验收标准\n")
	b.WriteString(criteriaList(criteria))
	if ho.CriteriaNotes != "" {
		b.WriteString("\n（标准经 plan 评审修订：" + ho.CriteriaNotes + "）\n")
	}
	b.WriteString("\n")
	if s := machineVerifiedSection(ho.TierOutcomes); s != "" {
		b.WriteString(s + "\n")
	}
	b.WriteString("### diff\n")
	b.WriteString(summarizeDiff(diff))
	b.WriteString("\n\n")
	if s := strings.TrimSpace(ho.EnvNotes); s != "" {
		b.WriteString("### 环境与复现（execute 自报）\n" + s + "\n\n")
	}
	if s := risksSection(ho.Risks); s != "" {
		b.WriteString(s + "\n")
	}
	if s := strings.TrimSpace(ho.RunHistory); s != "" {
		b.WriteString("### 历轮记录\n" + s + "\n\n")
	}
	if msg := strings.TrimSpace(priorFailure); msg != "" {
		b.WriteString("### 上一轮反馈（prior failure）\n")
		b.WriteString(msg)
		b.WriteString("\n\n")
	}
	b.WriteString("### 请你判断\n")
	b.WriteString("- 机器已过机械（tier-1）与语义（tier-2）验证；请聚焦业务正确性、审美、外部影响等机器判不了的部分。\n")
	b.WriteString("- **接受**：回复含 `loop:accept` —— 按现状自动落地（本地 FF-merge / GitHub 发 PR），不重跑实现。\n")
	b.WriteString("- **驳回**：回复具体反馈 —— 带反馈重做后再请你审。\n")
	return b.String()
}

// sceneSection 渲染交接现场元信息：轮次 / token / 分支与 worktree / 完整版去向。
// 全空（零值包）返回空串。
func sceneSection(ho Handoff) string {
	var lines []string
	if ho.MaxRetries > 0 {
		lines = append(lines, fmt.Sprintf("- 进度：第 %d/%d 轮", ho.Attempt, ho.MaxRetries))
	}
	if ho.TokensIn > 0 || ho.TokensOut > 0 {
		lines = append(lines, fmt.Sprintf("- 本 run token：in %d / out %d", ho.TokensIn, ho.TokensOut))
	}
	if ho.Branch != "" {
		line := "- 分支：" + ho.Branch + "（已 commit，worktree 清理也不丢）"
		if ho.Worktree != "" {
			line += "；worktree：" + ho.Worktree
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return ""
	}
	return "### 交接现场\n" + strings.Join(lines, "\n") + "\n"
}

// machineVerifiedSection 渲染「机器已验过什么」：本轮 tier-1/2 的判词。人不必
// 重做机器已做的验证——这是证据包的核心：审查依据独立产出，而非模型自述。
func machineVerifiedSection(outcomes []TierOutcome) string {
	if len(outcomes) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("### 机器已验过（你不必重做）\n")
	for _, o := range outcomes {
		verdict := "FAIL"
		if o.Passed {
			verdict = "PASS"
		}
		line := fmt.Sprintf("- tier-%d: %s", o.Tier, verdict)
		if d := strings.TrimSpace(o.Detail); d != "" {
			line += " — " + d
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

// risksSection 渲染 plan 识别的风险（open threads：接手方必读）。
func risksSection(risks []string) string {
	if len(risks) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("### 风险与未决（plan 标注）\n")
	for _, r := range risks {
		b.WriteString("- " + r + "\n")
	}
	return b.String()
}

func criteriaList(c []string) string {
	if len(c) == 0 {
		return "(未提供)\n"
	}
	var b strings.Builder
	for _, line := range c {
		b.WriteString("- ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// summarizeDiff 把 diff 渲染进评论。上限 200 行（原 60 行不够人审真实改动；
// 200 行内 diff 全量给审的人，超长截断并指向完整版）——完整 diff 永远在
// state.db steps.output_json 与 worktree 里。
func summarizeDiff(diff string) string {
	diff = strings.TrimRight(diff, "\n")
	if diff == "" {
		return "(空 diff —— 未检测到改动)"
	}
	lines := strings.Split(diff, "\n")
	const maxLines = 200
	head := diff
	truncated := false
	if len(lines) > maxLines {
		head = strings.Join(lines[:maxLines], "\n")
		truncated = true
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d 行改动：\n```\n%s\n```", len(lines), head)
	if truncated {
		fmt.Fprintf(&b, "\n(仅显示前 %d 行；完整 diff 见 worktree / state.db steps.output_json)", maxLines)
	}
	return b.String()
}

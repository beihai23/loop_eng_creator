package verify

import (
	"context"
	"fmt"
	"strings"

	"loop-eng/internal/channel"
)

// Human is the real tier-3 async human-review tier (spec §8.6). On Check it
// posts a review-request comment to the channel (diff summary + acceptance
// criteria + what the human must judge) and returns NeedsHuman=true, Passed=false
// — the signal that parks the task for daemon.ListReplies polling.
//
// HumanStub in contrast is the M1 placeholder: Passed=true, NeedsHuman=false,
// no comment — an auto-pass that never blocks the chain. When Human is injected
// as SubLoop.HumanTier, it replaces the stub with a real tier-3 that actually
// posts a review request and yields NeedsHuman.
type Human struct {
	Channel channel.Channel
	Ref     string // task issue reference (e.g. issue number as string)
}

func (h Human) Check(ctx context.Context, diff string, criteria []string, priorFailure string) (VerifyResult, error) {
	body := reviewRequestBody(diff, criteria, priorFailure)
	if err := h.Channel.PostComment(ctx, h.Ref, body); err != nil {
		return VerifyResult{}, fmt.Errorf("human review: post review-request comment: %w", err)
	}
	return VerifyResult{
		Passed:     false,
		NeedsHuman: true,
		Detail:     "tier-3 human review requested; task parked awaiting human reply",
	}, nil
}

// reviewRequestBody formats the review-request comment body posted by Human.Check.
// It includes a diff summary (truncated for readability), the acceptance criteria
// checklist, and guidance on what the human should judge — echoing the execute
// prompt's conventions so the human sees the same criteria the model was given.
func reviewRequestBody(diff string, criteria []string, priorFailure string) string {
	var b strings.Builder
	b.WriteString("## 🤖 Tier-3 Human Review Request\n\n")
	b.WriteString("loop-eng 自动验证（tier-1 确定性 + tier-2 LLM）已完成，")
	b.WriteString("此任务进入 tier-3 异步人审阶段。\n\n")
	b.WriteString("### 验收标准\n\n")
	if len(criteria) == 0 {
		b.WriteString("(未提供)\n\n")
	} else {
		for _, c := range criteria {
			b.WriteString(fmt.Sprintf("- [ ] %s\n", c))
		}
		b.WriteString("\n")
	}
	b.WriteString("### 需人判断的点\n\n")
	b.WriteString("- 变更是否真的满足了所有验收标准？\n")
	b.WriteString("- 是否有边界情况未覆盖？\n")
	b.WriteString("- diff 中是否有任何不可接受的质量/安全问题？\n")
	if priorFailure != "" {
		b.WriteString(fmt.Sprintf("- 上一轮反馈：%s\n", priorFailure))
	}
	b.WriteString("\n### Diff 摘要\n\n")
	b.WriteString("```diff\n")
	b.WriteString(truncateDiff(diff, 4000))
	b.WriteString("\n```\n\n")
	b.WriteString("---\n")
	b.WriteString("请在此 issue 下回复您的判断：通过（approve / LGTM）或驳回（说明未满足的标准）。")
	b.WriteString("daemon 将在轮询到回复后恢复该任务。\n")
	return b.String()
}

// truncateDiff limits the diff to at most n bytes with a "… [truncated]" marker
// at the end. A full repo diff can be huge; the comment is for human skim, not
// exhaustive review — the human can always check the worktree directly.
func truncateDiff(diff string, n int) string {
	if len(diff) <= n {
		return diff
	}
	return diff[:n] + "\n… [truncated — full diff in worktree]"
}

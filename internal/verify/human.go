package verify

import (
	"context"
	"fmt"
	"strings"

	"loop-eng/internal/channel"
)

// Human is the real tier-3 tier (spec §8.6/§10): an asynchronous human review
// that never blocks the SubLoop. Check posts a review-request comment (diff
// summary + acceptance criteria + the points needing human judgment) on the
// task's ticket, then returns {Passed:false, NeedsHuman:true}. SubLoop routes
// NeedsHuman → needs-review (park, release the active slot, keep the worktree);
// the daemon later polls channel.ListReplies and resumes the task carrying the
// human's accept/reject feedback.
type Human struct {
	Ch  channel.Channel
	Ref string
}

func (h Human) Check(ctx context.Context, diff string, criteria []string, priorFailure string) (VerifyResult, error) {
	body := channel.MarkBotComment(reviewRequest(diff, criteria, priorFailure))
	if err := h.Ch.PostComment(ctx, h.Ref, body); err != nil {
		return VerifyResult{}, err
	}
	return VerifyResult{
		Passed:     false,
		NeedsHuman: true,
		Detail:     "tier-3 人审已请求：已发 review-request 评论",
	}, nil
}

func reviewRequest(diff string, criteria []string, priorFailure string) string {
	var b strings.Builder
	b.WriteString("## REVIEW-REQUEST（tier-3 人审）\n\n")
	b.WriteString("### 验收标准\n")
	b.WriteString(criteriaList(criteria))
	b.WriteString("\n")
	b.WriteString("### diff 摘要\n")
	b.WriteString(summarizeDiff(diff))
	b.WriteString("\n\n")
	if msg := strings.TrimSpace(priorFailure); msg != "" {
		b.WriteString("### 上一轮反馈（prior failure）\n")
		b.WriteString(msg)
		b.WriteString("\n\n")
	}
	b.WriteString("### 要人判断的点\n")
	b.WriteString("- diff 是否真正满足每条验收标准？\n")
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

func summarizeDiff(diff string) string {
	diff = strings.TrimRight(diff, "\n")
	if diff == "" {
		return "(空 diff —— 未检测到改动)"
	}
	lines := strings.Split(diff, "\n")
	const maxLines = 60
	head := diff
	truncated := false
	if len(lines) > maxLines {
		head = strings.Join(lines[:maxLines], "\n")
		truncated = true
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d 行改动：\n```\n%s\n```", len(lines), head)
	if truncated {
		fmt.Fprintf(&b, "\n(仅显示前 %d 行；完整 diff 见 worktree)", maxLines)
	}
	return b.String()
}

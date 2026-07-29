package channel

import (
	"strings"
	"testing"
)

// TestMarkBotComment：加标记幂等——首次加在首行，二次调用原样返回。
func TestMarkBotComment(t *testing.T) {
	body := "DONE: 全部通过"
	got := MarkBotComment(body)
	if !strings.HasPrefix(got, BotMarker) {
		t.Fatalf("标记应在首行: %q", got)
	}
	if !strings.Contains(got, body) {
		t.Fatalf("正文应保留: %q", got)
	}
	if again := MarkBotComment(got); again != got {
		t.Fatalf("MarkBotComment 不幂等: %q → %q", got, again)
	}
}

// TestIsBotCommentByMarker：带标记的评论（任意正文）判为 bot。
func TestIsBotCommentByMarker(t *testing.T) {
	if !IsBotComment(MarkBotComment("人也能写出这种正文 BLOCKED: x")) {
		t.Fatal("带 BotMarker 的评论应判为 bot（标记优先于正文内容）")
	}
}

// TestIsBotCommentByLegacyPrefix：存量评论（无标记）按确定性前缀兜底识别——
// 逐个钉死 7 类产出点的前缀，防产出点文案改动后过滤静默失效。
func TestIsBotCommentByLegacyPrefix(t *testing.T) {
	legacy := []string{
		"## VERIFY-FAIL（第 2 轮 verify 驳回）\n\n### 失败理由\n...",
		"## REVIEW-REQUEST（tier-3 人审）\n\n### 验收标准\n...",
		"DONE: 全部通过",
		"BLOCKED: retries exhausted: ...",
		"NEEDS-REVIEW: tier-3 人审已请求",
		"CANCELLED: cancelled by TUI",
		"NEEDS-INFO: 分诊判断任务信息不足，暂不开工。",
		"NEEDS-HUMAN-DECISION: 分诊判断此任务需要人来拍板。",
		"PR 待合并：https://example.com/pr/1，合并后自动关闭",
		"[LAND PARTIAL: 仅本地分支 loop/x，push 失败……]",
		"  \n BLOCKED: 前导空白不影响前缀判定",
	}
	for _, body := range legacy {
		if !IsBotComment(body) {
			t.Errorf("存量 daemon 评论未识别: %q", body)
		}
	}
}

// TestIsBotCommentHumanReplies：人写的反馈（包括谈论战报内容的）不得误滤。
func TestIsBotCommentHumanReplies(t *testing.T) {
	human := []string{
		"plan.md 例子是瞎讲，脚本要按 plan 实现个性化",
		"上一轮人审意见：请先修编译",
		"我看了 VERIFY-FAIL 的理由，其实标准写错了，应该……", // 人引用战报但不以前缀起头
		"reopen：请改用 cobra 重写",
		"",
		"   ",
	}
	for _, body := range human {
		if IsBotComment(body) {
			t.Errorf("人反馈被误滤: %q", body)
		}
	}
}

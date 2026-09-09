package verify

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"loop-eng/internal/channel"
)

var _ channel.Channel = (*fakeReviewChan)(nil)

type fakeReviewChan struct {
	comments map[string][]string
	statuses map[string]string
	err      error
}

func (f *fakeReviewChan) ListNewTasks(context.Context) ([]channel.Task, error) {
	return nil, nil
}
func (f *fakeReviewChan) ListReplies(context.Context, []string, time.Time) (map[string][]channel.Reply, error) {
	return nil, nil
}
func (f *fakeReviewChan) PostComment(_ context.Context, ref, body string) error {
	if f.err != nil {
		return f.err
	}
	if f.comments == nil {
		f.comments = map[string][]string{}
	}
	f.comments[ref] = append(f.comments[ref], body)
	return nil
}
func (f *fakeReviewChan) UpdateStatus(_ context.Context, ref, status string) error {
	if f.statuses == nil {
		f.statuses = map[string]string{}
	}
	f.statuses[ref] = status
	return nil
}
func (f *fakeReviewChan) CloseIssue(context.Context, string) error { return nil }
func (f *fakeReviewChan) GetTaskStates(context.Context, []string) (map[string]channel.TaskState, error) {
	return nil, nil
}

// ---- Human: tier-3 异步人审（spec §8.6/§10） ----

// TestHumanCheckReturnsNeedsHuman 核验核心契约：Human.Check 必须返回
// Passed=false、NeedsHuman=true —— daemon 据此 park 任务并轮询人审回复。
func TestHumanCheckReturnsNeedsHuman(t *testing.T) {
	ch := &fakeReviewChan{}
	h := Human{Ch: ch, Ref: "42"}
	res, err := h.Check(context.Background(), "diff", []string{"std-1"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Passed {
		t.Fatal("Human.Check 必须返回 Passed=false（park，而非 done）")
	}
	if !res.NeedsHuman {
		t.Fatal("Human.Check 必须返回 NeedsHuman=true（信号 daemon park + 轮询）")
	}
	if res.Detail == "" {
		t.Fatal("Human.Check 的 Detail 必须非空（说明为何 park）")
	}
}

// TestHumanCheckPostsComment 核验 Human.Check 确实经 channel 发了
// review-request 评论，而非静默 park 不通知人。
func TestHumanCheckPostsComment(t *testing.T) {
	ch := &fakeReviewChan{}
	h := Human{Ch: ch, Ref: "42"}
	if _, err := h.Check(context.Background(), "diff-content", []string{"std-a", "std-b"}, ""); err != nil {
		t.Fatal(err)
	}
	got := ch.comments["42"]
	if len(got) != 1 {
		t.Fatalf("want 1 次 PostComment 调用，got %d", len(got))
	}
	body := got[0]
	if body == "" {
		t.Fatal("PostComment body 不能为空（人审需要上下文）")
	}
	if !strings.Contains(body, "diff-content") {
		t.Fatal("review-request body 必须包含 diff")
	}
	if !strings.Contains(body, "std-a") || !strings.Contains(body, "std-b") {
		t.Fatal("review-request body 必须包含验收标准")
	}
}

// TestHumanCheckPostCommentError 核验错误传播：channel.PostComment 失败时，
// Human.Check 把错误返回，绝不静默吞掉一次失败的 review-request 投递。
func TestHumanCheckPostCommentError(t *testing.T) {
	ch := &fakeReviewChan{err: fmt.Errorf("github down")}
	h := Human{Ch: ch, Ref: "42"}
	if _, err := h.Check(context.Background(), "diff", nil, ""); err == nil {
		t.Fatal("PostComment 错误必须从 Check 向上传播")
	}
}

// TestReviewRequestIncludesPriorFailure 核验 review-request 评论带上
// priorFailure（上一轮反馈），让人审拿到完整上下文。
func TestReviewRequestIncludesPriorFailure(t *testing.T) {
	body := reviewRequest(Handoff{}, "diff", []string{"std-1"}, "上一轮 LLM 驳回: 缺错误处理")
	if !strings.Contains(body, "上一轮 LLM 驳回") {
		t.Fatal("review-request body 必须包含 priorFailure，供人审看完整上下文")
	}
}

// TestSummarizeDiffTruncation 核验 diff 摘要：空 diff 有标记、超长 diff 被截断带
// 提示、短 diff 不截断——评论保持「可略读」的人审体量。
func TestSummarizeDiffTruncation(t *testing.T) {
	if got := summarizeDiff(""); !strings.Contains(got, "空 diff") {
		t.Fatalf("空 diff 必须有显式标记，got: %q", got)
	}
	// 250 行 > 200 行阈值，必须截断。
	many := strings.Repeat("line\n", 250)
	got := summarizeDiff(many)
	if !strings.Contains(got, "仅显示前") {
		t.Fatalf("超长 diff 必须带截断提示，got: %q", got[:min(80, len(got))])
	}
	// 200 行恰好不截断（阈值内全量给审的人）。
	exact := summarizeDiff(strings.Repeat("line\n", 200))
	if strings.Contains(exact, "仅显示前") {
		t.Fatal("200 行 diff 在阈值内，不应截断")
	}
	// 短 diff 不应被截断。
	short := "only one line of diff"
	if got := summarizeDiff(short); strings.Contains(got, "仅显示前") {
		t.Fatal("短 diff 不应被截断")
	}
}

// ---- 移交包渲染（Handoff → review-request 评论） ----

// fullHandoff 是渲染测试的典型包：全部字段非零。
func fullHandoff() Handoff {
	return Handoff{
		Attempt: 2, MaxRetries: 3,
		Worktree: "/tmp/wt-x", Branch: "loop/task_x-r2",
		RunHistory:    "- verify-FAIL — 缺错误处理",
		EnvNotes:      "需要 go 1.25；跑 make test 验证",
		Risks:         []string{"并发路径未覆盖"},
		CriteriaNotes: "删掉了不可脚本化的第 3 条",
		TierOutcomes:  []TierOutcome{{Tier: 1, Passed: true, Detail: "make test 退出码 0"}, {Tier: 2, Passed: true, Detail: "语义满足"}},
		TokensIn:      12000, TokensOut: 6000,
	}
}

// TestReviewRequestRendersHandoffPackage 核验移交包全字段进评论：现场元信息、
// 机器已验过、环境复现、风险、标准修订、历轮——审的人拿到完整现场而非摘要。
func TestReviewRequestRendersHandoffPackage(t *testing.T) {
	body := reviewRequest(fullHandoff(), "diff-body", []string{"std-1"}, "")
	for _, want := range []string{
		"第 2/3 轮",               // 轮次透明度
		"in 12000 / out 6000",   // token 透明度
		"loop/task_x-r2",        // 分支
		"make test 退出码 0",       // 机器已验过：tier-1 判词
		"环境与复现",                 // execute 自报段
		"并发路径未覆盖",               // plan 风险
		"标准经 plan 评审修订",         // 标准修订审计线索
		"- verify-FAIL — 缺错误处理", // 历轮记录
		"loop:accept",           // accept 操作教学
	} {
		if !strings.Contains(body, want) {
			t.Errorf("review-request 缺少移交包字段 %q", want)
		}
	}
}

// TestReviewRequestZeroHandoffStillUsable 核验零值包（未注入/测试直构）不产生
// 空段噪音，评论仍有标准 + diff + 判断点可用。
func TestReviewRequestZeroHandoffStillUsable(t *testing.T) {
	body := reviewRequest(Handoff{}, "d", []string{"std-1"}, "")
	for _, ghost := range []string{"### 交接现场", "### 机器已验过", "### 环境与复现", "### 风险与未决", "### 历轮记录", "第 0"} {
		if strings.Contains(body, ghost) {
			t.Errorf("零值包不应渲染空段 %q", ghost)
		}
	}
	for _, want := range []string{"### 验收标准", "### diff", "### 请你判断"} {
		if !strings.Contains(body, want) {
			t.Errorf("零值包评论仍须有 %q", want)
		}
	}
}

// TestChainInjectsHandoffToAwareTier 核验注入通道：Chain 对 HandoffAware tier
// 在 Check 前注入快照，TierOutcomes 含此前已过的 tier 结果；非 aware tier 不受影响。
func TestChainInjectsHandoffToAwareTier(t *testing.T) {
	captured := &Handoff{}
	aware := handoffSpyTier{captured: captured}
	tiers := []Tier{stubTier{pass: true, detail: "t1"}, stubTier{pass: true, detail: "t2"}, aware}
	if _, err := Chain(context.Background(), tiers, "d", nil, "", Handoff{Attempt: 2, Branch: "loop/b"}); err != nil {
		t.Fatal(err)
	}
	if captured.Attempt != 2 || captured.Branch != "loop/b" {
		t.Fatalf("注入的包字段丢失: %+v", *captured)
	}
	if len(captured.TierOutcomes) != 2 || !captured.TierOutcomes[0].Passed {
		t.Fatalf("TierOutcomes 应含此前 2 个已过 tier 的快照, got %+v", captured.TierOutcomes)
	}
}

// handoffSpyTier 记录 Chain 注入的移交包（实现 HandoffAware）。
type handoffSpyTier struct {
	captured *Handoff
}

func (s handoffSpyTier) SetHandoff(p Handoff) { *s.captured = p }
func (s handoffSpyTier) Check(context.Context, string, []string, string) (VerifyResult, error) {
	return VerifyResult{Passed: false, NeedsHuman: true}, nil
}

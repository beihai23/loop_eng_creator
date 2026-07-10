package verify

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"loop-eng/internal/channel"
	"loop-eng/internal/model"
	"loop-eng/internal/skill"
)

func TestDeterministicPass(t *testing.T) {
	d := Deterministic{Label: "true", Cmd: []string{"true"}}
	r, err := d.Check(context.Background(), "", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if !r.Passed {
		t.Fatal("true should pass")
	}
}

func TestDeterministicFail(t *testing.T) {
	d := Deterministic{Label: "false", Cmd: []string{"false"}}
	r, _ := d.Check(context.Background(), "", nil, "")
	if r.Passed {
		t.Fatal("false should fail")
	}
	if r.Detail == "" {
		t.Fatal("want detail on fail")
	}
}

func TestChainShortCircuitsOnTier1Fail(t *testing.T) {
	fail := Deterministic{Label: "tests", Cmd: []string{"false"}}
	called := false
	t2 := tierSpy{called: &called}
	res, _ := Chain(context.Background(), []Tier{fail, t2}, "", nil, "")
	if res.Passed {
		t.Fatal("should fail")
	}
	if called {
		t.Fatal("tier2 must not run when tier1 fails")
	}
}

func TestChainPassesWhenAllPass(t *testing.T) {
	ok := Deterministic{Label: "tests", Cmd: []string{"true"}}
	human := HumanStub{}
	res, _ := Chain(context.Background(), []Tier{ok, human}, "", nil, "")
	if !res.Passed {
		t.Fatal("ok+tier3-stub should pass (stub doesn't block in M1 chain)")
	}
}

type tierSpy struct{ called *bool }

func (s tierSpy) Check(context.Context, string, []string, string) (VerifyResult, error) {
	*s.called = true
	return VerifyResult{Passed: true}, nil
}

// ---- detailFor: 驳回可观测性兜底（修 #10 黑箱：passed=false detail= 全空） ----

// TestDetailForRejectEmptyReasonSynthesizes: 驳回 + reason 空 + 无 failing_criteria
// → Detail 仍非空（合成消息）。这是 #10 的核心兜底——驳回永远可解释。
func TestDetailForRejectEmptyReasonSynthesizes(t *testing.T) {
	d := detailFor(skill.VerifyOutput{Passed: false, Reason: ""})
	if d == "" {
		t.Fatal("reject with empty reason must still yield non-empty Detail (no black box)")
	}
}

// TestDetailForRejectReasonWins: 驳回 + reason 非空 → Detail == reason（逐字），
// 即使同时给了 failing_criteria——模型自述最准，优先。
func TestDetailForRejectReasonWins(t *testing.T) {
	d := detailFor(skill.VerifyOutput{Passed: false, Reason: "x", FailingCriteria: []string{"a", "b"}})
	if d != "x" {
		t.Fatalf("Detail = %q, want %q (non-empty reason must win verbatim)", d, "x")
	}
}

// TestDetailForRejectFallsBackToFailingCriteria: 驳回 + reason 空 + 有 failing_criteria
// → Detail 拼接 failing_criteria（每条都在，可 debug、可指导下轮重试）。
func TestDetailForRejectFallsBackToFailingCriteria(t *testing.T) {
	d := detailFor(skill.VerifyOutput{Passed: false, Reason: "", FailingCriteria: []string{"std-a", "std-b"}})
	if d == "" {
		t.Fatal("reject with failing_criteria must fall back to them, got empty")
	}
	if !strings.Contains(d, "std-a") || !strings.Contains(d, "std-b") {
		t.Fatalf("Detail = %q, want failing_criteria joined (std-a, std-b)", d)
	}
}

// TestDetailForPassUnaffected: 通过不受兜底影响——Detail == reason（可为空）。
// 兜底只作用于驳回；通过是信息性的，空 reason 合法。
func TestDetailForPassUnaffected(t *testing.T) {
	if d := detailFor(skill.VerifyOutput{Passed: true, Reason: ""}); d != "" {
		t.Fatalf("pass with empty reason: Detail = %q, want empty (unaffected by fallback)", d)
	}
	if d := detailFor(skill.VerifyOutput{Passed: true, Reason: "all good"}); d != "all good" {
		t.Fatalf("pass Detail = %q, want %q", d, "all good")
	}
}

// TestLLMCheckSynthesizesDetailOnEmptyReason: 端到端 wiring——LLM.Check 必须用
// detailFor，故模型返回 {passed:false,reason:""} 时 Detail 非空（防有人忘了接线）。
func TestLLMCheckSynthesizesDetailOnEmptyReason(t *testing.T) {
	fake := model.NewFake(map[string]string{
		"VERIFY:": mustJSONStr(skill.VerifyOutput{Passed: false, Reason: "", FailingCriteria: []string{"std-1"}}),
	})
	vs := LLM{Skill: skill.Skill[skill.VerifyInput, skill.VerifyOutput]{
		Name:       "verify",
		PromptTmpl: "VERIFY:",
		ParseJSON:  parseVerifyOutput,
		Model:      fake,
	}}
	res, err := vs.Check(context.Background(), "diff", []string{"std-1"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Passed {
		t.Fatal("want reject")
	}
	if res.Detail == "" {
		t.Fatal("LLM.Check must synthesize non-empty Detail when model leaves reason blank")
	}
	if !strings.Contains(res.Detail, "std-1") {
		t.Fatalf("Detail = %q, want failing_criteria fallback (std-1)", res.Detail)
	}
}

func parseVerifyOutput(b []byte) (skill.VerifyOutput, error) {
	var o skill.VerifyOutput
	return o, json.Unmarshal(b, &o)
}

func mustJSONStr(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// ---- Human: tier-3 async human review (spec §8.6) ----

// TestHumanCheckReturnsNeedsHuman verifies the core contract: Human.Check must
// return Passed=false, NeedsHuman=true — the signal the daemon uses to park the
// task and begin polling for a human reply.
func TestHumanCheckReturnsNeedsHuman(t *testing.T) {
	ch := &channelSpy{}
	h := Human{Channel: ch, Ref: "42"}
	res, err := h.Check(context.Background(), "diff", []string{"std-1"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Passed {
		t.Fatal("Human.Check must return Passed=false (park, not done)")
	}
	if !res.NeedsHuman {
		t.Fatal("Human.Check must return NeedsHuman=true (signal daemon to park + poll)")
	}
	if res.Detail == "" {
		t.Fatal("Human.Check Detail must be non-empty (explain why parked)")
	}
}

// TestHumanCheckPostsComment verifies that Human.Check actually posts a
// review-request comment via the channel — not silently park without messaging.
func TestHumanCheckPostsComment(t *testing.T) {
	ch := &channelSpy{}
	h := Human{Channel: ch, Ref: "42"}
	_, err := h.Check(context.Background(), "diff-content", []string{"std-a", "std-b"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ch.comments) != 1 {
		t.Fatalf("want 1 PostComment call, got %d", len(ch.comments))
	}
	c := ch.comments[0]
	if c.ref != "42" {
		t.Fatalf("PostComment ref = %q, want %q", c.ref, "42")
	}
	if c.body == "" {
		t.Fatal("PostComment body must not be empty (human needs context)")
	}
	if !strings.Contains(c.body, "diff-content") {
		t.Fatal("PostComment body must contain the diff")
	}
	if !strings.Contains(c.body, "std-a") || !strings.Contains(c.body, "std-b") {
		t.Fatal("PostComment body must contain acceptance criteria")
	}
}

// TestHumanCheckPostCommentError verifies error propagation: if the channel's
// PostComment fails, Human.Check returns the error so the caller doesn't
// silently swallow a failed review-request post.
func TestHumanCheckPostCommentError(t *testing.T) {
	ch := &channelSpy{postErr: fmt.Errorf("github down")}
	h := Human{Channel: ch, Ref: "42"}
	_, err := h.Check(context.Background(), "diff", nil, "")
	if err == nil {
		t.Fatal("PostComment error must propagate up from Check")
	}
	if !strings.Contains(err.Error(), "review-request") {
		t.Fatalf("error must wrap context, got: %v", err)
	}
}

// TestReviewRequestBodyIncludesPriorFailure ensures the review-request comment
// includes the prior-failure feedback so the human reviewer has the full picture.
func TestReviewRequestBodyIncludesPriorFailure(t *testing.T) {
	body := reviewRequestBody("diff", []string{"std-1"}, "上一轮 LLM 驳回: 缺错误处理")
	if !strings.Contains(body, "上一轮 LLM 驳回") {
		t.Fatal("review-request body must include priorFailure for full context")
	}
}

// TestTruncateDiff ensures long diffs are truncated with a readable marker so
// the comment stays at a skim-friendly size.
func TestTruncateDiff(t *testing.T) {
	short := "small diff"
	if truncateDiff(short, 100) != short {
		t.Fatal("short diff must not be truncated")
	}
	long := strings.Repeat("x", 5000)
	got := truncateDiff(long, 100)
	if len(got) >= len(long) {
		t.Fatal("long diff must be truncated")
	}
	if !strings.Contains(got, "truncated") {
		t.Fatal("truncated diff must include a marker so the human knows it's partial")
	}
}

// channelSpy records PostComment calls for test assertions.
type channelSpy struct {
	comments []struct{ ref, body string }
	postErr  error // if set, PostComment returns this error
}

func (s *channelSpy) ListNewTasks(ctx context.Context) ([]channel.Task, error)  { return nil, nil }
func (s *channelSpy) ListReplies(ctx context.Context, refs []string) (map[string][]channel.Reply, error) {
	return nil, nil
}
func (s *channelSpy) UpdateStatus(ctx context.Context, ref, status string) error { return nil }
func (s *channelSpy) PostComment(ctx context.Context, ref, body string) error {
	s.comments = append(s.comments, struct{ ref, body string }{ref, body})
	return s.postErr
}

// compile-time interface check
var _ channel.Channel = (*channelSpy)(nil)

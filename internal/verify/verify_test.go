package verify

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

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
	res, _ := Chain(context.Background(), []Tier{fail, t2}, "", nil, "", Handoff{})
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
	res, _ := Chain(context.Background(), []Tier{ok, human}, "", nil, "", Handoff{})
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

// TestLLMCheckPopulatesUsage 单元级钉死 usage 旁路契约（修 verify step 行恒为 0）：
// LLM{Usage:&model.Usage{}, Skill:...} 调 Check 后 *Usage 等于 Skill.Run 返回的
// model.Usage（非零真值）。Tier.Check 冻结签名不动——usage 经指针旁路带出。既有
// nil Usage 不崩（TestLLMCheckSynthesizesDetailOnEmptyReason 等用 LLM{Skill:...}
// 裸构造，Usage 为 nil）由那些用例覆盖。
func TestLLMCheckPopulatesUsage(t *testing.T) {
	fake := model.NewFake(map[string]string{
		"VERIFY:": mustJSONStr(skill.VerifyOutput{Passed: true, Reason: "ok"}),
	})
	vs := LLM{
		Usage: &model.Usage{},
		Skill: skill.Skill[skill.VerifyInput, skill.VerifyOutput]{
			Name:       "verify",
			PromptTmpl: "VERIFY:",
			ParseJSON:  parseVerifyOutput,
			Model:      fake,
		},
	}
	if _, err := vs.Check(context.Background(), "diff", []string{"c"}, ""); err != nil {
		t.Fatal(err)
	}
	// 旁路写的必须等于 Skill.Run 返回的 model.Usage（fake 推导：TokensIn=len(prompt),
	// TokensOut=len(out)）。同一 skill+input 两次 Run 渲染同一 prompt → usage 一致。
	_, want, err := vs.Skill.Run(context.Background(), skill.VerifyInput{Diff: "diff", AcceptanceCriteria: []string{"c"}})
	if err != nil {
		t.Fatal(err)
	}
	if *vs.Usage != want {
		t.Fatalf("usage bypass = %+v, want Skill.Run's %+v", *vs.Usage, want)
	}
	if vs.Usage.TokensIn == 0 || vs.Usage.TokensOut == 0 {
		t.Fatalf("usage must be non-zero, got %+v", *vs.Usage)
	}
}

// ---- Chain 填充 Tiers（spec §4.6 逐 tier 落盘）----

type stubTier struct {
	pass   bool
	detail string
}

func (s stubTier) Check(context.Context, string, []string, string) (VerifyResult, error) {
	return VerifyResult{Passed: s.pass, Detail: s.detail}, nil
}

func TestChainFillsTiersAndShortCircuits(t *testing.T) {
	// tier1 过、tier2 挂 ⇒ Tiers 有两条（tier2 失败），tier3 不跑不出现。
	tiers := []Tier{
		stubTier{pass: true, detail: "t1 ok"},
		stubTier{pass: false, detail: "t2 nope"},
		stubTier{pass: true, detail: "t3"},
	}
	res, err := Chain(context.Background(), tiers, "diff", nil, "", Handoff{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Passed {
		t.Fatalf("want not passed")
	}
	if len(res.Tiers) != 2 {
		t.Fatalf("want 2 tier outcomes (short-circuit), got %d", len(res.Tiers))
	}
	if res.Tiers[0].Tier != 1 || !res.Tiers[0].Passed || res.Tiers[1].Tier != 2 || res.Tiers[1].Passed {
		t.Fatalf("Tiers=%+v", res.Tiers)
	}
}

func TestChainFillsTiersAllPass(t *testing.T) {
	tiers := []Tier{stubTier{pass: true, detail: "t1"}, stubTier{pass: true, detail: "t2"}}
	res, _ := Chain(context.Background(), tiers, "diff", nil, "", Handoff{})
	if !res.Passed || len(res.Tiers) != 2 {
		t.Fatalf("want passed + 2 tiers, got %+v", res)
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

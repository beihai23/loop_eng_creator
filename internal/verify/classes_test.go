package verify

// M2 争议路由：VerifyOutput.FailureClasses → VerifyResult 的透传与过滤。
// LLM 输出不可信——非法 class/空 criterion 按「未分类」丢弃（路由回落 work）。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"loop-eng/internal/model"
	"loop-eng/internal/skill"
)

func TestValidClassesFiltersJunk(t *testing.T) {
	got := validClasses([]skill.FailureClass{
		{Criterion: "标准甲", Class: "exam", Evidence: "不可判定"},
		{Criterion: "标准乙", Class: "banana"},   // 非法 class → 丢弃
		{Criterion: "", Class: "requirement"}, // 空 criterion → 丢弃
		{Criterion: "  ", Class: "work"},      // 空白 criterion → 丢弃
		{Criterion: "标准丙", Class: "requirement"},
	})
	if len(got) != 2 {
		t.Fatalf("want 2 valid classes, got %d: %+v", len(got), got)
	}
	if got[0].Class != "exam" || got[1].Class != "requirement" {
		t.Fatalf("valid classes mismatch: %+v", got)
	}
	if validClasses(nil) != nil {
		t.Fatal("nil 输入应返回 nil（未分类=全 work）")
	}
}

// fakeVerifyClient 返回固定 payload 的 model.Client（Check 透传测试用）。
type fakeVerifyClient struct{ out string }

func (f fakeVerifyClient) Call(context.Context, string) (string, model.Usage, error) {
	return f.out, model.Usage{}, nil
}

func TestCheckCarriesFailureClasses(t *testing.T) {
	out := `{"passed":false,"reason":"r","failing_criteria":["c"],"failure_classes":[{"criterion":"c","class":"exam","evidence":"不可判定"}]}`
	l := LLM{Skill: skill.Skill[skill.VerifyInput, skill.VerifyOutput]{
		Name: "verify", PromptTmpl: "VERIFY:",
		ParseJSON: func(b []byte) (skill.VerifyOutput, error) {
			var o skill.VerifyOutput
			return o, json.Unmarshal(b, &o)
		},
		Model: fakeVerifyClient{out: out},
	}}
	res, err := l.Check(context.Background(), "diff", []string{"c"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.FailureClasses) != 1 || res.FailureClasses[0].Class != "exam" {
		t.Fatalf("FailureClasses 未透传: %+v", res.FailureClasses)
	}
	if !strings.Contains(res.Detail, "r") {
		t.Fatalf("detail 透传破坏: %q", res.Detail)
	}
}

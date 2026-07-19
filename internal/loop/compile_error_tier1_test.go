package loop

import (
	"context"
	"strings"
	"testing"

	"loop-eng/internal/budget"
	"loop-eng/internal/channel"
	"loop-eng/internal/model"
	"loop-eng/internal/skill"
	"loop-eng/internal/state"
	"loop-eng/internal/verify"
)

// captureExecs 记录每次 execute 收到的 prompt（不写文件 → 空 diff），按 attempt 顺序存。
type captureExecs struct{ prompts []string }

func (c *captureExecs) Exec(_ context.Context, _ string, prompt string) (string, model.Usage, error) {
	c.prompts = append(c.prompts, prompt)
	return "ok", model.Usage{}, nil
}

// TestTier1CompileErrorSectionGatingAndText 钉死 compileErrorSection 的门控与文本：
// 非编译错误（含提到符号 Foo 的 tier-2 语义驳回）→ 空串；编译/构建错误 → 含 header
// 锚点 + 逐字原文（含未定义符号）。硬依赖仅 compileErrorSection 一个函数——detector
// 的内部命名自由（宽容调用面，防 plan/execute 名字漂移本身致编译失败）。
func TestTier1CompileErrorSectionGatingAndText(t *testing.T) {
	for _, detail := range []string{
		"",
		"语义驳回：函数 Foo 的返回值未处理 nil 情况",
		"实现未覆盖验收标准第 2 条",
	} {
		if got := compileErrorSection(detail); got != "" {
			t.Fatalf("compileErrorSection(%q) = %q; want empty (no compile markers)", detail, got)
		}
	}
	detail := "internal/loop/x.go:7:6: undefined: Foo"
	got := compileErrorSection(detail)
	if got == "" {
		t.Fatal("compileErrorSection(compile-error detail) must be non-empty")
	}
	for _, want := range []string{"上一轮编译错误", "Foo", "undefined:", detail} {
		if !strings.Contains(got, want) {
			t.Fatalf("section missing anchor %q:\n%s", want, got)
		}
	}
}

// slWithFailingVerify 构造一个 verify 永远以 verifyDetail 驳回的 SubLoop：tier-2 LLM
// 返回 Passed=false + Reason=verifyDetail，detailFor 逐字落进 res.Detail → priorFailure
// （subloop.go 的 res.Detail 赋值点）。镜像 retry_diagnosis_tier1_test.go 的固定配置。
func slWithFailingVerify(t *testing.T, exec model.Executer, verifyDetail string) *SubLoop {
	t.Helper()
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	t.Cleanup(func() { st.Close() })
	fake := model.NewFake(map[string]string{
		"PLAN:":   validPlanJSON(), // non-empty plan ⇒ ≥1 step ⇒ Execute actually runs (an empty PlanOutput{} yields no steps and Execute is never called)
		"VERIFY:": mustJSON(skill.VerifyOutput{Passed: false, Reason: verifyDetail}),
	})
	return &SubLoop{
		Repo:       repo,
		Store:      st,
		Budget:     budget.New(100000, 1000000, 2),
		Execute:    exec,
		Plan:       mkSkill[skill.PlanInput, skill.PlanOutput]("PLAN:", fake),
		VerifyLLM:  verify.LLM{Skill: mkSkill[skill.VerifyInput, skill.VerifyOutput]("VERIFY:", fake)},
		Tier3Human: true,
		Channel:    channel.NewLocal(t.TempDir()),
	}
}

// TestTier1CompileErrorSectionInjectedAboveBattleReport 端到端：attempt 1 verify 以编译
// 错误（undefined: Foo）驳回 → attempt 2 execute prompt 顶部出现「上一轮编译错误」独立段
// （含 Foo）；attempt 1（priorFailure 空）无此段；若战报段在场，编译错误段在它之前（未埋进散文）。
func TestTier1CompileErrorSectionInjectedAboveBattleReport(t *testing.T) {
	exec := &captureExecs{}
	sl := slWithFailingVerify(t, exec, "internal/loop/x.go:7:6: undefined: Foo")
	task := channel.Task{Ref: "#52", Description: "编译错误直达 execute", AcceptanceCriteria: []string{"c"}}
	// 预发一条 issue 评论让「战报/反馈」段在场，才能断言编译错误段在它之前（未埋进战报散文）。
	_ = sl.Channel.PostComment(context.Background(), task.Ref, "上一轮人审意见：请先修编译")
	out, _ := sl.Run(context.Background(), task)
	if out.Status != "blocked" {
		t.Fatalf("want blocked (verify exhausts retries), got %s", out.Status)
	}
	if len(exec.prompts) < 2 {
		t.Fatalf("want >=2 execute prompts (attempt 1+2), got %d", len(exec.prompts))
	}
	if strings.Contains(exec.prompts[0], "上一轮编译错误") {
		t.Fatal("attempt 1 (fresh, no prior failure) must NOT have compile-error section")
	}
	p2 := exec.prompts[1]
	if !strings.Contains(p2, "上一轮编译错误") {
		t.Fatal("attempt 2 execute prompt missing dedicated 上一轮编译错误 section")
	}
	if !strings.Contains(p2, "Foo") {
		t.Fatal("attempt 2 execute prompt missing undefined symbol Foo in section")
	}
	if iSec, iBR := strings.Index(p2, "上一轮编译错误"), strings.Index(p2, "战报/反馈"); iBR >= 0 && iSec >= iBR {
		t.Fatal("compile-error section must precede 战报/反馈 (not buried in battle report)")
	}
}

// TestTier1CompileErrorSectionAbsentForSemanticReject 端到端：tier-2 语义驳回（无编译特征，
// 但同样提到 Foo）→ attempt 2 execute prompt 不出现「上一轮编译错误」段（门控只对编译/构建错误特化）。
func TestTier1CompileErrorSectionAbsentForSemanticReject(t *testing.T) {
	exec := &captureExecs{}
	sl := slWithFailingVerify(t, exec, "语义驳回：函数 Foo 的返回值未处理 nil 情况")
	task := channel.Task{Ref: "#52", Description: "语义驳回不触发编译段", AcceptanceCriteria: []string{"c"}}
	out, _ := sl.Run(context.Background(), task)
	if out.Status != "blocked" {
		t.Fatalf("want blocked, got %s", out.Status)
	}
	if len(exec.prompts) < 2 {
		t.Fatalf("want >=2 execute prompts, got %d", len(exec.prompts))
	}
	if strings.Contains(exec.prompts[1], "上一轮编译错误") {
		t.Fatal("semantic reject must NOT trigger compile-error section")
	}
}

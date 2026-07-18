package loop

import (
	"context"
	"os"
	"strings"
	"testing"

	"loop-eng/internal/budget"
	"loop-eng/internal/channel"
	"loop-eng/internal/model"
	"loop-eng/internal/skill"
	"loop-eng/internal/state"
	"loop-eng/internal/verify"
)

// TestTier1RetryDiagnosisGatingAndText 钉死重试诊断的门控与文本契约：attempt<2
// 或空 priorFailure → 不注入（空串）；attempt≥2 + 真实失败 → 诊断，且含必备锚点。
func TestTier1RetryDiagnosisGatingAndText(t *testing.T) {
	for _, c := range []struct {
		attempt int
		prior   string
	}{
		{1, "verify rejected: whatever"},
		{0, "fail"},
		{2, ""},
		{2, "   "},
		{3, "\t\n"},
	} {
		if got := retryDiagnosisFor(c.attempt, c.prior); got != "" {
			t.Fatalf("retryDiagnosisFor(%d, %q) = %q; want empty (no inject)", c.attempt, c.prior, got)
		}
	}
	prior := "verify rejected: diff must embed grep output of NewLinear callers"
	got := retryDiagnosisFor(2, prior)
	if got == "" {
		t.Fatal("retryDiagnosisFor(2, non-empty) must inject diagnosis")
	}
	for _, want := range []string{"重试诊断", "结构性不可满足", "revised_criteria", prior} {
		if !strings.Contains(got, want) {
			t.Fatalf("diagnosis missing anchor %q:\n%s", want, got)
		}
	}
}

// TestTier1RetryDiagnosisAttemptGatePersisted 端到端：verify 连续驳回 ⇒
// attempt 1 plan step input_json 不含诊断；attempt 2 含诊断（落进 steps.input_json）。
func TestTier1RetryDiagnosisAttemptGatePersisted(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()
	planTmpl := "PLAN:\n{{if .RetryDiagnosis}}{{.RetryDiagnosis}}{{end}}"
	fake := model.NewFake(map[string]string{
		"PLAN:":    mustJSON(skill.PlanOutput{}),
		"EXECUTE:": "ok",
		"VERIFY:":  mustJSON(skill.VerifyOutput{Passed: false, Reason: "nope"}),
	})
	sl := &SubLoop{
		Repo:       repo,
		Store:      st,
		Budget:     budget.New(100000, 1000000, 2),
		Execute:    fake,
		Plan:       mkSkill[skill.PlanInput, skill.PlanOutput](planTmpl, fake),
		VerifyLLM:  verify.LLM{Skill: mkSkill[skill.VerifyInput, skill.VerifyOutput]("VERIFY:", fake)},
		Tier3Human: true,
		Channel:    channel.NewLocal(t.TempDir()),
	}
	out, _ := sl.Run(context.Background(), channel.Task{Ref: "77", Description: "d"})
	if out.Status != "blocked" {
		t.Fatalf("want blocked (verify exhausts retries), got %s", out.Status)
	}
	views, _ := st.TasksByStatus()
	runs, _ := st.RunsOfTask(views[0].ID)
	steps, _ := st.Replay(runs[0].ID)
	planByAttempt := map[int]string{}
	for _, s := range steps {
		if s.Role == "plan" {
			planByAttempt[s.Seq/10] = s.InputJSON
		}
	}
	a1, ok1 := planByAttempt[1]
	a2, ok2 := planByAttempt[2]
	if !ok1 || !ok2 {
		t.Fatalf("want plan steps for attempt 1 and 2; got %v", planByAttempt)
	}
	if strings.Contains(a1, "重试诊断") {
		t.Fatalf("attempt 1 plan input must NOT contain diagnosis:\n%s", a1)
	}
	if !strings.Contains(a2, "重试诊断") {
		t.Fatalf("attempt 2 plan input MUST contain diagnosis:\n%s", a2)
	}
}

// TestTier1RetryDiagnosisEmbedReferencesField 钉死生产 embed 渲染该字段
// （否则不进 input_json / 不到达 LLM）。runtime 读 embed，非 .loop/skills 镜像。
func TestTier1RetryDiagnosisEmbedReferencesField(t *testing.T) {
	b, err := os.ReadFile("../cli/embed/skills/plan.md")
	if err != nil {
		t.Fatalf("read plan embed (cwd should be internal/loop): %v", err)
	}
	if !strings.Contains(string(b), ".RetryDiagnosis") {
		t.Fatal("plan embed does not reference {{.RetryDiagnosis}}; diagnosis will not render into input_json")
	}
}

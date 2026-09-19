package loop

// M1 出题权分离（test-prep）的接线测试。覆盖面与方案文档对齐
// （docs/superpowers/plans/2026-09-18-loop-eng-test-prep-split.md）：
//   - 启用路径：step 布局 plan(+1)/test-prep(+2)/execute(+3)/verify(+4)、
//     effTask 食源（execute prompt 吃考卷标准）、tier-1 脚本来自考卷、
//     done 战报附出题说明、execute 契约段展示考卷脚本。
//   - 盲出题（白名单制）：test-prep 的 prompt 只含白名单字段；启用时 plan 的
//     重试诊断被抑制（revised_criteria 已不可行使）。
//   - 回灌：run 内 attempt N+1 的考卷输入带 attempt N 的考卷；跨 run 经
//     LatestTestPrepOutputByRef 回灌（新 SubLoop 同 ref 首轮即带上轮考卷）。
//   - 可用性优先：test-prep 基础设施错误 → step fail、回落 issue 原版标准、
//     tier-1 缺席，loop 照常跑到 done。
// legacy 路径（TestPrep==nil）零改动的回归由既有全套测试保障。

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

// examRecordingClient 记录 test-prep 的渲染后 prompt（Call/CallIn 双通道都记），
// 其余转发给内层 FakeClient。
type examRecordingClient struct {
	inner   *model.FakeClient
	tpCalls []string
}

func (c *examRecordingClient) Call(ctx context.Context, prompt string) (string, model.Usage, error) {
	if strings.HasPrefix(prompt, "TEST-PREP:") {
		c.tpCalls = append(c.tpCalls, prompt)
	}
	return c.inner.Call(ctx, prompt)
}

func (c *examRecordingClient) CallIn(ctx context.Context, dir, prompt string) (string, model.Usage, error) {
	if strings.HasPrefix(prompt, "TEST-PREP:") {
		c.tpCalls = append(c.tpCalls, prompt)
	}
	return c.inner.Call(ctx, prompt)
}

func (c *examRecordingClient) Exec(ctx context.Context, wt, prompt string) (string, model.Usage, error) {
	return c.inner.Exec(ctx, wt, prompt)
}

func examFakeOutput() skill.TestPrepOutput {
	return skill.TestPrepOutput{
		Criteria:      []string{"考卷标准一", "考卷标准二"},
		CriteriaNotes: "原标准不可判定，已改写为两条可判定条款",
		VerifyScript:  &skill.PlanVerifyScript{Label: "exam-tier1", Run: []string{"true"}},
		Risks:         []string{"阈值未指定，留人审"},
	}
}

// tpTpl 把考卷输入渲染进 prompt，供断言 PriorExam/原件确到达模型。
const tpExamTpl = "TEST-PREP: exam[{{.PriorExam}}] crit[{{.AcceptanceCriteria}}]"

// planTpl 渲染 ExamSeparate/RetryDiagnosis，供断言条件化与诊断抑制。
const planExamTpl = "PLAN: examsep[{{.ExamSeparate}}] diag[{{.RetryDiagnosis}}]"

func newTestPrepSubLoop(t *testing.T, repo string, st *state.Store, rec *examRecordingClient, maxRetries int) *SubLoop {
	t.Helper()
	tp := mkSkill[skill.TestPrepInput, skill.TestPrepOutput](tpExamTpl, rec)
	return &SubLoop{
		Repo: repo, Store: st, Budget: budget.New(100000, 1000000, maxRetries),
		Execute:          rec,
		Plan:             mkSkill[skill.PlanInput, skill.PlanOutput](planExamTpl, rec),
		TestPrep:         &tp,
		TestPrepModelRef: "claude/exam",
		VerifyLLM:        verify.LLM{Skill: mkSkill[skill.VerifyInput, skill.VerifyOutput]("VERIFY:", rec)},
		Tier3Human:       true,
		Channel:          channel.NewLocal(t.TempDir()),
	}
}

func examTask(ref string) channel.Task {
	return channel.Task{Ref: ref, Description: "d", AcceptanceCriteria: []string{"原始标准一"}}
}

// stepsBySeq 取该任务全部 step 按 seq 索引（跨 run 的断言不用它）。
func stepsBySeq(t *testing.T, st *state.Store) map[int]state.StepRow {
	t.Helper()
	statuses, err := st.ListStatuses()
	if err != nil || len(statuses) != 1 {
		t.Fatalf("want exactly 1 task status, got %d (err %v)", len(statuses), err)
	}
	rows, err := st.StepsOfTask(statuses[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	bySeq := map[int]state.StepRow{}
	for _, r := range rows {
		bySeq[r.Seq] = r
	}
	return bySeq
}

func TestSubLoopTestPrepAuthoredExam(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()
	rec := &examRecordingClient{inner: model.NewFake(map[string]string{
		"PLAN:":      validPlanJSON(),
		"TEST-PREP:": mustJSON(examFakeOutput()),
		"EXECUTE:":   "ok",
		"VERIFY:":    mustJSON(skill.VerifyOutput{Passed: true}),
	})}
	sl := newTestPrepSubLoop(t, repo, st, rec, 3)

	out, err := sl.Run(context.Background(), examTask("tp-done"))
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "done" {
		t.Fatalf("want done, got %s (%s)", out.Status, out.Detail)
	}

	// step 布局：plan(11)/test-prep(12)/execute(13)/verify(14)。
	bySeq := stepsBySeq(t, st)
	for seq, wantRole := range map[int]string{11: "plan", 12: "test-prep", 13: "execute", 14: "verify"} {
		s, ok := bySeq[seq]
		if !ok || s.Role != wantRole || s.Status != "ok" {
			t.Fatalf("step seq=%d want role=%s ok, got %+v", seq, wantRole, s)
		}
	}
	if bySeq[12].ModelRef != "claude/exam" {
		t.Fatalf("test-prep step model_ref 未落库: %q", bySeq[12].ModelRef)
	}

	// effTask 食源：execute prompt 吃考卷标准与出题说明，不吃 issue 原件标准。
	exec := bySeq[13].InputJSON
	if !strings.Contains(exec, "考卷标准一") || !strings.Contains(exec, "test-prep 独立出题") {
		t.Fatalf("execute prompt 未采用考卷标准/出题说明: %s", exec)
	}
	if strings.Contains(exec, "原始标准一") {
		t.Fatalf("execute prompt 不应再含 issue 原件标准: %s", exec)
	}
	// 契约可见性：execute 契约段展示考卷的 tier-1 脚本（plan 无脚本）。
	if !strings.Contains(exec, "tier-1 验收脚本") || !strings.Contains(exec, "运行: true") {
		t.Fatalf("execute 契约段应展示考卷脚本（run 命令）: %s", exec)
	}

	// tier-1 来自考卷脚本（Run=["true"] 必过）：verifications 含 Deterministic 行。
	vs := verificationsOf(t, st)
	if len(vs) != 3 {
		t.Fatalf("want 3 verification rows (tier1+tier2+tier3), got %d: %+v", len(vs), vs)
	}
	if vs[0].Tier != 1 || !vs[0].Passed {
		t.Fatalf("考卷 tier-1 应存在且通过: %+v", vs[0])
	}

	// done 战报附出题说明（人审的审计线索）。
	if !strings.Contains(out.Detail, "验收标准由 test-prep 独立出题") {
		t.Fatalf("done 战报应附出题说明: %s", out.Detail)
	}

	// 盲出题：考卷 prompt 不含 plan 产出（白名单外的实现侧产物）。
	if len(rec.tpCalls) != 1 {
		t.Fatalf("want 1 test-prep call, got %d", len(rec.tpCalls))
	}
	if strings.Contains(rec.tpCalls[0], "实现任务以满足验收标准") {
		t.Fatalf("考卷 prompt 泄漏了 plan 输出: %s", rec.tpCalls[0])
	}
	// 白名单正面断言：原件标准到达。
	if !strings.Contains(rec.tpCalls[0], "原始标准一") {
		t.Fatalf("考卷 prompt 应含标准原件: %s", rec.tpCalls[0])
	}
}

func TestSubLoopTestPrepFallbackOnError(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()
	rec := &examRecordingClient{inner: model.NewFake(map[string]string{
		"PLAN:":      validPlanJSON(),
		"TEST-PREP:": "不是 JSON 的摆烂输出", // 解析失败 → 出题不可用
		"EXECUTE:":   "ok",
		"VERIFY:":    mustJSON(skill.VerifyOutput{Passed: true}),
	})}
	sl := newTestPrepSubLoop(t, repo, st, rec, 3)

	out, err := sl.Run(context.Background(), examTask("tp-fallback"))
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "done" {
		t.Fatalf("出题器挂掉不应阻塞 loop，want done, got %s (%s)", out.Status, out.Detail)
	}
	bySeq := stepsBySeq(t, st)
	if s := bySeq[12]; s.Role != "test-prep" || s.Status != "fail" {
		t.Fatalf("test-prep step 应记 fail: %+v", s)
	}
	// 回落：execute 吃 issue 原版标准；无考卷说明、无考卷脚本。
	exec := bySeq[13].InputJSON
	if !strings.Contains(exec, "原始标准一") || strings.Contains(exec, "考卷标准一") {
		t.Fatalf("回落应使用 issue 原版标准: %s", exec)
	}
	// tier-1 缺席：只有 tier-2 + tier-3 两行。
	if vs := verificationsOf(t, st); len(vs) != 2 {
		t.Fatalf("出题失败时 tier-1 应缺席（2 rows）, got %d", len(vs))
	}
}

func TestSubLoopTestPrepPriorExamFedOnRetry(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()
	rec := &examRecordingClient{inner: model.NewFake(map[string]string{
		"PLAN:":      validPlanJSON(),
		"TEST-PREP:": mustJSON(examFakeOutput()),
		"EXECUTE:":   "ok",
		"VERIFY:":    mustJSON(skill.VerifyOutput{Passed: false, Reason: "nope"}),
	})}
	sl := newTestPrepSubLoop(t, repo, st, rec, 2)

	out, _ := sl.Run(context.Background(), examTask("tp-retry"))
	if out.Status != "blocked" {
		t.Fatalf("want blocked, got %s", out.Status)
	}
	if len(rec.tpCalls) != 2 {
		t.Fatalf("want 2 test-prep calls (每 attempt 一轮), got %d", len(rec.tpCalls))
	}
	// attempt 1：无上轮考卷（exam[] 为空）；attempt 2：带上轮考卷 JSON（默认沿用）。
	if strings.Contains(rec.tpCalls[0], `"criteria"`) {
		t.Fatalf("attempt 1 不应带上轮考卷: %s", rec.tpCalls[0])
	}
	if !strings.Contains(rec.tpCalls[1], `"criteria"`) || !strings.Contains(rec.tpCalls[1], "考卷标准一") {
		t.Fatalf("attempt 2 应带回 attempt 1 的考卷: %s", rec.tpCalls[1])
	}
	// 盲出题的另一半：重试诊断（引用 revised_criteria 语义）在启用路径被抑制。
	if !strings.Contains(rec.tpCalls[1], "exam[") {
		t.Fatalf("sanity: 渲染模板失效")
	}
	planAttempt2 := ""
	// plan prompt 经 CallIn 记不进 examRecordingClient（只记 TEST-PREP 前缀）——
	// 改从 step trace 断言：attempt 2 的 plan step seq=21。
	statuses, err := st.ListStatuses()
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := st.StepsOfTask(statuses[0].ID)
	for _, r := range rows {
		if r.Seq == 21 && r.Role == "plan" {
			planAttempt2 = r.InputJSON
		}
	}
	if !strings.Contains(planAttempt2, "diag[]") || !strings.Contains(planAttempt2, "examsep[true]") {
		t.Fatalf("启用路径 plan 的重试诊断应被抑制、ExamSeparate 应为 true: %s", planAttempt2)
	}
}

func TestSubLoopTestPrepCrossRunReadback(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()
	rec1 := &examRecordingClient{inner: model.NewFake(map[string]string{
		"PLAN:":      validPlanJSON(),
		"TEST-PREP:": mustJSON(examFakeOutput()),
		"EXECUTE:":   "ok",
		"VERIFY:":    mustJSON(skill.VerifyOutput{Passed: true}),
	})}
	sl1 := newTestPrepSubLoop(t, repo, st, rec1, 3)
	if out, err := sl1.Run(context.Background(), examTask("tp-cross")); err != nil || out.Status != "done" {
		t.Fatalf("run1 want done, got %s (%v)", out.Status, err)
	}

	// 第二次 run（新 SubLoop、新 task 行、同 issue_ref）：首轮即从 DB 读回上轮考卷。
	rec2 := &examRecordingClient{inner: model.NewFake(map[string]string{
		"PLAN:":      validPlanJSON(),
		"TEST-PREP:": mustJSON(examFakeOutput()),
		"EXECUTE:":   "ok",
		"VERIFY:":    mustJSON(skill.VerifyOutput{Passed: true}),
	})}
	sl2 := newTestPrepSubLoop(t, repo, st, rec2, 3)
	if out, err := sl2.Run(context.Background(), examTask("tp-cross")); err != nil || out.Status != "done" {
		t.Fatalf("run2 want done, got %s (%v)", out.Status, err)
	}
	if len(rec2.tpCalls) != 1 {
		t.Fatalf("want 1 test-prep call in run2, got %d", len(rec2.tpCalls))
	}
	if !strings.Contains(rec2.tpCalls[0], `"criteria"`) || !strings.Contains(rec2.tpCalls[0], "考卷标准一") {
		t.Fatalf("跨 run 首轮应读回上轮考卷: %s", rec2.tpCalls[0])
	}
}

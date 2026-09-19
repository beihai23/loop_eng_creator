package loop

// M2 争议路由的接线测试：
//   - requirement → needs-human-decision 挂起（与 test-prep 启用与否无关）；
//   - exam → 争议包回灌下一轮 test-prep（首考无包，指控轮之后有包）；
//   - work / 未分类 → 现行重试路径零变化（不带争议包）；
//   - 纯函数：examDisputeOf 只提 exam 条目、requirementDisputeComment 可读、
//     verifyFailComment 展示归因段。
// verify 侧的 validClasses 过滤在 internal/verify 包测试。

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

// disputeVerifyClient 按 VERIFY 调用序数返回预设payload（第一轮驳回带归因、
// 第二轮通过的时序测试用），其余前缀转发内层 FakeClient。
type disputeVerifyClient struct {
	inner    *model.FakeClient
	verifies []string
	calls    int
}

func (c *disputeVerifyClient) verifyPayload() string {
	i := c.calls
	if i >= len(c.verifies) {
		i = len(c.verifies) - 1
	}
	return c.verifies[i]
}

func (c *disputeVerifyClient) dispatch(prompt string) (string, error) {
	if strings.HasPrefix(prompt, "VERIFY:") {
		p := c.verifyPayload()
		c.calls++
		return p, nil
	}
	out, _, err := c.inner.Call(context.Background(), prompt)
	return out, err
}

func (c *disputeVerifyClient) Call(_ context.Context, prompt string) (string, model.Usage, error) {
	out, err := c.dispatch(prompt)
	return out, model.Usage{}, err
}

func (c *disputeVerifyClient) CallIn(_ context.Context, _ string, prompt string) (string, model.Usage, error) {
	out, err := c.dispatch(prompt)
	return out, model.Usage{}, err
}

func (c *disputeVerifyClient) Exec(_ context.Context, wt string, prompt string) (string, model.Usage, error) {
	return c.inner.Exec(context.Background(), wt, prompt)
}

// verifyRejectJSON 构造带归因分类的驳回输出。
func verifyRejectJSON(reason string, classes []skill.FailureClass) string {
	return mustJSON(skill.VerifyOutput{Passed: false, Reason: reason, FailingCriteria: []string{"标准甲"}, FailureClasses: classes})
}

// tpDisputeTpl 在 tpExamTpl 基础上渲染争议包字段（生产模板的对应条件块在
// embed test-prep.md；单测用裸模板直接断言字段到达）。
const tpDisputeTpl = "TEST-PREP: exam[{{.PriorExam}}] crit[{{.AcceptanceCriteria}}] dispute[{{.DisputePacket}}]"

func TestSubLoopRequirementDisputeParks(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()
	// legacy（无 test-prep）也要能路由——requirement 争议与出题权分离正交。
	fake := model.NewFake(map[string]string{
		"PLAN:":    validPlanJSON(),
		"EXECUTE:": "ok",
	})
	vc := &disputeVerifyClient{inner: fake, verifies: []string{
		verifyRejectJSON("标准甲与需求正文矛盾", []skill.FailureClass{{Criterion: "标准甲", Class: "requirement", Evidence: "需求要求 A 同时要求非 A，任何实现都无法满足"}}),
	}}
	sl := &SubLoop{
		Repo: repo, Store: st, Budget: budget.New(100000, 1000000, 3),
		Execute:    fake,
		Plan:       mkSkill[skill.PlanInput, skill.PlanOutput]("PLAN:", fake),
		VerifyLLM:  verify.LLM{Skill: mkSkill[skill.VerifyInput, skill.VerifyOutput]("VERIFY:", vc)},
		Tier3Human: true,
		Channel:    channel.NewLocal(t.TempDir()),
	}
	out, err := sl.Run(context.Background(), examTask("dp-req"))
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "needs-human-decision" {
		t.Fatalf("want needs-human-decision, got %s (%s)", out.Status, out.Detail)
	}
	if !strings.Contains(out.Detail, "需求本身的问题") || !strings.Contains(out.Detail, "标准甲") || !strings.Contains(out.Detail, "任何实现都无法满足") {
		t.Fatalf("战报应含裁决入口（标准+证据）: %s", out.Detail)
	}
}

func TestSubLoopExamDisputeFeedsTestPrep(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()
	rec := &examRecordingClient{inner: model.NewFake(map[string]string{
		"PLAN:":      validPlanJSON(),
		"TEST-PREP:": mustJSON(examFakeOutput()),
		"EXECUTE:":   "ok",
	})}
	vc := &disputeVerifyClient{inner: rec.inner, verifies: []string{
		verifyRejectJSON("考卷判据结构性不可满足", []skill.FailureClass{{Criterion: "标准甲", Class: "exam", Evidence: "要求 diff 附运行输出，而该输出不进 diff"}}),
		mustJSON(skill.VerifyOutput{Passed: true}),
	}}
	tp := mkSkill[skill.TestPrepInput, skill.TestPrepOutput](tpDisputeTpl, rec)
	sl := &SubLoop{
		Repo: repo, Store: st, Budget: budget.New(100000, 1000000, 3),
		Execute:          vc,
		Plan:             mkSkill[skill.PlanInput, skill.PlanOutput](planExamTpl, rec),
		TestPrep:         &tp,
		TestPrepModelRef: "claude/exam",
		VerifyLLM:        verify.LLM{Skill: mkSkill[skill.VerifyInput, skill.VerifyOutput]("VERIFY:", vc)},
		Tier3Human:       true,
		Channel:          channel.NewLocal(t.TempDir()),
	}
	out, err := sl.Run(context.Background(), examTask("dp-exam"))
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "done" {
		t.Fatalf("want done, got %s (%s)", out.Status, out.Detail)
	}
	if len(rec.tpCalls) != 2 {
		t.Fatalf("want 2 test-prep calls, got %d", len(rec.tpCalls))
	}
	// 首考盲（无争议包）；被指控后的下一轮考卷输入带争议包（知情修订）。
	if strings.Contains(rec.tpCalls[0], "归因指控") {
		t.Fatalf("attempt 1 不应带争议包: %s", rec.tpCalls[0])
	}
	if !strings.Contains(rec.tpCalls[1], "归因指控") || !strings.Contains(rec.tpCalls[1], "不进 diff") {
		t.Fatalf("attempt 2 应带 exam 争议包（含证据）: %s", rec.tpCalls[1])
	}
}

func TestSubLoopWorkClassKeepsLegacyRetry(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()
	rec := &examRecordingClient{inner: model.NewFake(map[string]string{
		"PLAN:":      validPlanJSON(),
		"TEST-PREP:": mustJSON(examFakeOutput()),
		"EXECUTE:":   "ok",
	})}
	vc := &disputeVerifyClient{inner: rec.inner, verifies: []string{
		verifyRejectJSON("实现没做完", []skill.FailureClass{{Criterion: "标准甲", Class: "work", Evidence: "diff 缺少对应改动"}}),
	}}
	tp := mkSkill[skill.TestPrepInput, skill.TestPrepOutput](tpDisputeTpl, rec)
	sl := &SubLoop{
		Repo: repo, Store: st, Budget: budget.New(100000, 1000000, 2),
		Execute:    vc,
		Plan:       mkSkill[skill.PlanInput, skill.PlanOutput](planExamTpl, rec),
		TestPrep:   &tp,
		VerifyLLM:  verify.LLM{Skill: mkSkill[skill.VerifyInput, skill.VerifyOutput]("VERIFY:", vc)},
		Tier3Human: true,
		Channel:    channel.NewLocal(t.TempDir()),
	}
	out, _ := sl.Run(context.Background(), examTask("dp-work"))
	if out.Status != "blocked" {
		t.Fatalf("want blocked (现行重试), got %s", out.Status)
	}
	// work 类不触发争议：两轮考卷输入都不带争议包。
	for i, c := range rec.tpCalls {
		if strings.Contains(c, "归因指控") {
			t.Fatalf("attempt %d 的考卷输入不应带争议包（work 类）: %s", i+1, c)
		}
	}
}

func TestExamDisputeOfOnlyPicksExam(t *testing.T) {
	res := verify.VerifyResult{
		Detail: "两条不过",
		FailureClasses: []skill.FailureClass{
			{Criterion: "标准甲", Class: "work", Evidence: "w"},
			{Criterion: "标准乙", Class: "exam", Evidence: "e"},
			{Criterion: "标准丙", Class: "requirement", Evidence: "r"},
		},
	}
	got := examDisputeOf(res)
	if !strings.Contains(got, "标准乙") || !strings.Contains(got, "e") {
		t.Fatalf("争议包应只含 exam 条目: %q", got)
	}
	if strings.Contains(got, "标准甲") || strings.Contains(got, "标准丙") {
		t.Fatalf("争议包不应混入 work/requirement 条目: %q", got)
	}
	if examDisputeOf(verify.VerifyResult{}) != "" {
		t.Fatal("无分类应返回空串（正常出题）")
	}
}

func TestVerifyFailCommentShowsClasses(t *testing.T) {
	res := verify.VerifyResult{
		Detail:          "没做完",
		FailingCriteria: []string{"标准甲"},
		FailureClasses:  []skill.FailureClass{{Criterion: "标准甲", Class: "exam", Evidence: "不可判定"}},
	}
	c := verifyFailComment(channel.Task{AcceptanceCriteria: []string{"标准甲"}}, 1, res)
	if !strings.Contains(c, "### 归因分类") || !strings.Contains(c, "exam") || !strings.Contains(c, "不可判定") {
		t.Fatalf("驳回评论应展示归因段: %s", c)
	}
	// 无分类时不出现空归因段（旧输出形状不变）。
	c2 := verifyFailComment(channel.Task{AcceptanceCriteria: []string{"标准甲"}}, 1, verify.VerifyResult{Detail: "x", FailingCriteria: []string{"标准甲"}})
	if strings.Contains(c2, "归因分类") {
		t.Fatalf("无分类不应渲染归因段: %s", c2)
	}
}

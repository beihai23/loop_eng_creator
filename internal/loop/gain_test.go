package loop

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"loop-eng/internal/budget"
	"loop-eng/internal/channel"
	"loop-eng/internal/model"
	"loop-eng/internal/skill"
	"loop-eng/internal/state"
	"loop-eng/internal/verify"
)

// ---- 重试增益门槛（zero-gain gate）回归测试 ----
//
// 钉死 DoD：第二次同签名失败即升级（无第三次同构 attempt）+ trace zero_gain=true；
// 不同签名正常重试到 done；blocked 战报含 help 三字段（stuck_at/tried/need_from_human）。

// TestZeroGainOscillationLike71 复刻 #71 振荡形态（run_6ca678c5c8c42fbf）：verify 的
// 驳回理由每轮在「execute 返回 4 个值…3 个值」与「…2 个值…3 个值」间轮替——只是计数
// 不同。failureSignature 把计数剥掉后两轮归一为同一签名，故 attempt 2 的 verify 驳回即
// 零增益 → 升级 blocked，不再做第 3 轮同构 attempt。
func TestZeroGainOscillationLike71(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()

	osc := &oscillationVerifyClient{}
	fake := model.NewFake(map[string]string{
		"PLAN:":    validPlanJSON(), // 无 VerifyScript → 落 tier-2（LLM），由 osc 拍板
		"EXECUTE:": "ok",
	})
	sl := &SubLoop{
		Repo: repo, Store: st, Budget: budget.New(100000, 1000000, 3),
		Execute:    fake,
		Plan:       mkSkill[skill.PlanInput, skill.PlanOutput]("PLAN:", fake),
		VerifyLLM:  verify.LLM{Skill: mkSkill[skill.VerifyInput, skill.VerifyOutput]("VERIFY:", osc)},
		Tier3Human: true,
		Channel:    channel.NewLocal(t.TempDir()),
	}

	out, err := sl.Run(context.Background(), channel.Task{Ref: "71", Description: "振荡任务", AcceptanceCriteria: []string{"签名对齐"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "blocked" {
		t.Fatalf("振荡零增益应升级 blocked, got %s (%s)", out.Status, out.Detail)
	}
	// blocked 战报含「零增益」+ help 结构化字段。
	for _, want := range []string{"零增益", "stuck_at", "tried", "need_from_human"} {
		if !strings.Contains(out.Detail, want) {
			t.Fatalf("blocked detail 缺 %q:\n%s", want, out.Detail)
		}
	}

	steps := singleRunSteps(t, st)
	// 只跑了 2 轮 plan + 2 轮 verify（attempt 2 的 verify 驳回即升级，无第 3 轮）。
	if n := countStepsByRole(steps, "plan"); n != 2 {
		t.Fatalf("振荡零增益应只跑 2 轮 plan（无第 3 轮同构 attempt）, got %d", n)
	}
	if n := countStepsByRole(steps, "verify"); n != 2 {
		t.Fatalf("振荡零增益应只跑 2 轮 verify, got %d", n)
	}
	// trace 必有一条 role=retry-gate 且 zero_gain=true 的 step（判定依据可审计）。
	if !hasZeroGainGate(steps) {
		t.Fatalf("缺 role=retry-gate zero_gain=true 的 trace step: %+v", steps)
	}
}

// TestZeroGainVerbatimLikeB154ff82 复刻 run_b154ff8213e7851a：plan 连续返回逐字相同的
// "claude -p: context canceled"（非致命错误，走重试路径）。第二次同签名失败即零增益升级
// blocked，无第 3 次空转 plan 调用。
func TestZeroGainVerbatimLikeB154ff82(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()

	planClient := &alwaysFailPlanClient{err: errors.New("claude -p: context canceled")}
	fake := model.NewFake(map[string]string{ // execute/verify 不会被调用（plan 先失败）
		"EXECUTE:": "ok",
		"VERIFY:":  mustJSON(skill.VerifyOutput{Passed: true}),
	})
	sl := &SubLoop{
		Repo: repo, Store: st, Budget: budget.New(100000, 1000000, 3),
		Execute:    fake,
		Plan:       mkSkill[skill.PlanInput, skill.PlanOutput]("PLAN:", planClient),
		VerifyLLM:  verify.LLM{Skill: mkSkill[skill.VerifyInput, skill.VerifyOutput]("VERIFY:", fake)},
		Tier3Human: true,
		Channel:    channel.NewLocal(t.TempDir()),
	}

	out, err := sl.Run(context.Background(), channel.Task{Ref: "b154", Description: "逐字同错任务", AcceptanceCriteria: []string{"c"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "blocked" {
		t.Fatalf("逐字同错零增益应升级 blocked, got %s (%s)", out.Status, out.Detail)
	}
	for _, want := range []string{"stuck_at", "tried", "need_from_human"} {
		if !strings.Contains(out.Detail, want) {
			t.Fatalf("blocked detail 缺 help 字段 %q:\n%s", want, out.Detail)
		}
	}
	// 只调了 2 次 plan（attempt 2 的 plan 失败即零增益升级，无第 3 次）。
	if planClient.calls != 2 {
		t.Fatalf("逐字同错零增益应只调 2 次 plan（无第 3 次空转）, got %d", planClient.calls)
	}
	steps := singleRunSteps(t, st)
	if !hasZeroGainGate(steps) {
		t.Fatalf("缺 role=retry-gate zero_gain=true 的 trace step: %+v", steps)
	}
}

// TestDifferentSignatureRetriesNormal 钉死 #46 形态：verify 驳回理由的「符号」每轮不同
// （undefined: Foo → undefined: Bar → Passed），failureSignature 保留符号差异 → 三轮签名
// 各不相同 → 门控不误伤，正常重试到 done。
func TestDifferentSignatureRetriesNormally(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()

	changed := &changingVerifyClient{}
	fake := model.NewFake(map[string]string{
		"PLAN:":    validPlanJSON(),
		"EXECUTE:": "ok",
	})
	sl := &SubLoop{
		Repo: repo, Store: st, Budget: budget.New(100000, 1000000, 3),
		Execute:    fake,
		Plan:       mkSkill[skill.PlanInput, skill.PlanOutput]("PLAN:", fake),
		VerifyLLM:  verify.LLM{Skill: mkSkill[skill.VerifyInput, skill.VerifyOutput]("VERIFY:", changed)},
		Tier3Human: true,
		Channel:    channel.NewLocal(t.TempDir()),
	}

	out, err := sl.Run(context.Background(), channel.Task{Ref: "46", Description: "编译错误→修复→过", AcceptanceCriteria: []string{"c"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "done" {
		t.Fatalf("不同签名应正常重试到 done, got %s (%s)", out.Status, out.Detail)
	}
	// 跑了 3 轮 plan（签名不同，门控不误伤）。
	steps := singleRunSteps(t, st)
	if n := countStepsByRole(steps, "plan"); n != 3 {
		t.Fatalf("不同签名应跑 3 轮 plan（门控不误伤）, got %d", n)
	}
	// 全程无 zero-gain 升级（每次签名都不同）。
	if hasZeroGainGate(steps) {
		t.Fatalf("不同签名不应触发 zero-gain 升级: %+v", steps)
	}
}

// TestEscalateBlockedDetailHasHelpFields 钉死 escalateZeroGain 路径的 blocked detail 逐字
// 含 stuck_at / tried / need_from_human 三标记（help skill 结构化输出接入）。
func TestEscalateBlockedDetailHasHelpFields(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()
	taskID, err := st.InsertTask(state.TaskRow{IssueRef: "Z", Description: "零增益任务"})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	sl := &SubLoop{
		Repo: repo, Store: st, Budget: budget.New(100000, 1000000, 3),
		Channel: channel.NewLocal(t.TempDir()),
	}
	task := channel.Task{Ref: "Z", Description: "零增益任务"}
	out := sl.escalateZeroGain(context.Background(), taskID, task, 2,
		"execute 返回 4 个值，plan 冻结 3 个值", "归一签名", "")
	if out.Status != "blocked" {
		t.Fatalf("escalateZeroGain 应产 blocked, got %s", out.Status)
	}
	for _, marker := range []string{"stuck_at", "tried", "need_from_human"} {
		if !strings.Contains(out.Detail, marker) {
			t.Fatalf("escalateZeroGain detail 缺 %q:\n%s", marker, out.Detail)
		}
	}
}

// ---- 测试桩 ----

// oscillationVerifyClient 复刻 #71 振荡：verify 理由每轮在两个仅计数不同的形态间轮替。
type oscillationVerifyClient struct {
	calls int
}

func (c *oscillationVerifyClient) Call(_ context.Context, _ string) (string, model.Usage, error) {
	reasons := []string{
		"签名不匹配：execute 返回 4 个值，plan 冻结 3 个值",
		"签名不匹配：execute 返回 2 个值，plan 冻结 3 个值",
	}
	r := reasons[c.calls%len(reasons)]
	c.calls++
	return mustJSON(skill.VerifyOutput{Passed: false, Reason: r}), model.Usage{}, nil
}

// alwaysFailPlanClient 复刻 run_b154ff82：plan 每次返回同一逐字错误（非致命，走重试路径）。
type alwaysFailPlanClient struct {
	calls int
	err   error
}

func (c *alwaysFailPlanClient) Call(_ context.Context, _ string) (string, model.Usage, error) {
	c.calls++
	return "", model.Usage{}, c.err
}

// changingVerifyClient 复刻 #46：verify 理由的符号每轮不同（Foo→Bar→过），签名各异。
type changingVerifyClient struct {
	calls int
}

func (c *changingVerifyClient) Call(_ context.Context, _ string) (string, model.Usage, error) {
	c.calls++
	switch c.calls {
	case 1:
		return mustJSON(skill.VerifyOutput{Passed: false, Reason: "undefined: Foo"}), model.Usage{}, nil
	case 2:
		return mustJSON(skill.VerifyOutput{Passed: false, Reason: "undefined: Bar"}), model.Usage{}, nil
	default:
		return mustJSON(skill.VerifyOutput{Passed: true}), model.Usage{}, nil
	}
}

// ---- 步骤断言助手（同包，复用 subloop_test 的 Replay 模式）----

// singleRunSteps 返回「单任务单 run」的 Replay steps（Run 自行 InsertTask + StartRun）。
func singleRunSteps(t *testing.T, st *state.Store) []state.StepRow {
	t.Helper()
	statuses, err := st.ListStatuses()
	if err != nil || len(statuses) != 1 {
		t.Fatalf("want 1 task status, got %d (err %v)", len(statuses), err)
	}
	runs, err := st.RunsOfTask(statuses[0].ID)
	if err != nil || len(runs) != 1 {
		t.Fatalf("want 1 run, got %d (err %v)", len(runs), err)
	}
	steps, err := st.Replay(runs[0].ID)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	return steps
}

// countStepsByRole 数给定 role 的 step 条数。
func countStepsByRole(steps []state.StepRow, role string) int {
	var n int
	for _, s := range steps {
		if s.Role == role {
			n++
		}
	}
	return n
}

// hasZeroGainGate 报告 trace 中是否存在一条 role=retry-gate 且 zero_gain=true 的 step。
func hasZeroGainGate(steps []state.StepRow) bool {
	for _, s := range steps {
		if s.Role != "retry-gate" {
			continue
		}
		var tr retryGateTrace
		if json.Unmarshal([]byte(s.OutputJSON), &tr) == nil && tr.ZeroGain {
			return true
		}
	}
	return false
}

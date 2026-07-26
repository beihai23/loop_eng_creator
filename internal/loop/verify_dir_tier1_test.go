package loop

// verify_dir_tier1_test.go 钉死两个 #81/#86 事故修复：
//  1. tier-2 verify 的模型调用 cwd = attempt worktree（agentic verify 的
//     ground-check 必须站在改动真实发生的树里，#81 假驳回的病根）；
//  2. 预算类 verify 错误（budget.Client 拒付 tier-2）立即 blocked，不进
//     重试循环空烧 plan+execute（#86/#87 实战教训）。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"loop-eng/internal/budget"
	"loop-eng/internal/channel"
	"loop-eng/internal/model"
	"loop-eng/internal/skill"
	"loop-eng/internal/state"
	"loop-eng/internal/verify"
)

// TestTier1VerifyRunsInWorktree：plan/execute/verify 三角色的模型调用目录
// 全部是同一棵 attempt worktree——verify 不再站在主仓库根做 ground-check。
func TestTier1VerifyRunsInWorktree(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()

	// dirRecordingClient（plan_workdir_tier1_test.go）实现 Client/DirClient/Executer，
	// CallIn 记录目录——plan 与 verify 的技能调用都会经过它。
	rec := &dirRecordingClient{inner: model.NewFake(map[string]string{
		"PLAN:":    validPlanJSON(),
		"EXECUTE:": "ok",
		"VERIFY:":  mustJSON(skill.VerifyOutput{Passed: true}),
	})}
	sl := &SubLoop{
		Repo: repo, Store: st, Budget: budget.New(100000, 1000000, 3),
		Execute:    rec,
		Plan:       mkScenePlanSkill(rec),
		VerifyLLM:  mkSceneVerifySkill(rec),
		Tier3Human: true,
		Channel:    channel.NewLocal(t.TempDir()),
	}
	out, err := sl.Run(context.Background(), channel.Task{Ref: "96", Description: "d", AcceptanceCriteria: []string{"c"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "done" {
		t.Fatalf("want done, got %s (%s)", out.Status, out.Detail)
	}
	// 两次 DirClient 调用（plan + verify）的目录都必须等于 execute 的 worktree。
	if len(rec.planDirs) != 2 || len(rec.execDirs) != 1 {
		t.Fatalf("want 2 dir-bound skill calls + 1 exec, got %d/%d", len(rec.planDirs), len(rec.execDirs))
	}
	for i, d := range rec.planDirs {
		if d != rec.execDirs[0] {
			t.Fatalf("skill call %d dir=%q, want worktree %q", i, d, rec.execDirs[0])
		}
	}
}

// TestTier1TiersForInjectsWorktree：tiersFor 把 wt 注入 LLM 层（Dir 字段），
// Tier 接口签名不变、链结构不变。
func TestTier1TiersForInjectsWorktree(t *testing.T) {
	sl := &SubLoop{}
	llm := verify.LLM{}
	tiers := sl.tiersFor("/wt-x", skill.PlanOutput{}, llm)
	got, ok := tiers[0].(verify.LLM)
	if !ok {
		t.Fatalf("tier[0] must be verify.LLM, got %T", tiers[0])
	}
	if got.Dir != "/wt-x" {
		t.Fatalf("tiersFor must inject wt into LLM.Dir, got %q", got.Dir)
	}
}

// TestTier1VerifyBudgetErrorBlocksImmediately：verify 的 budget.Client 拒付
// （per-task 超限）→ SubLoop 立即 blocked（detail 含 "budget:"），plan 只跑
// 一轮——预算错误不进重试循环。
func TestTier1VerifyBudgetErrorBlocksImmediately(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()

	rec := &sceneRecorder{inner: model.NewFake(map[string]string{
		"PLAN:":    validPlanJSON(),
		"EXECUTE:": "ok",
	})}
	// verify 的 Model 套 budget.Client：Enf(perCall=100, perTask=1) → BeforeCall
	// 即拒（spent 0 + estimate 100 > perTask 1），err 含 budget.ErrPerTask。
	verifyModel := &budget.Client{
		Base: model.NewFake(map[string]string{"VERIFY:": mustJSON(skill.VerifyOutput{Passed: true})}),
		Enf:  budget.New(100, 1, 3),
	}
	vs := skill.Skill[skill.VerifyInput, skill.VerifyOutput]{
		Name: "verify", PromptTmpl: "VERIFY:",
		ParseJSON: func(b []byte) (skill.VerifyOutput, error) {
			var o skill.VerifyOutput
			return o, json.Unmarshal(b, &o)
		},
		Model: verifyModel,
	}
	sl := &SubLoop{
		Repo: repo, Store: st, Budget: budget.New(100000, 1000000, 3),
		Execute:    rec,
		Plan:       mkScenePlanSkill(rec),
		VerifyLLM:  verify.LLM{Skill: vs},
		Tier3Human: true,
		Channel:    channel.NewLocal(t.TempDir()),
	}
	out, _ := sl.Run(context.Background(), channel.Task{Ref: "97", Description: "d", AcceptanceCriteria: []string{"c"}})
	if out.Status != "blocked" {
		t.Fatalf("budget error must block, got %s", out.Status)
	}
	if !strings.Contains(out.Detail, "budget:") {
		t.Fatalf("blocked detail must name the budget cause, got %q", out.Detail)
	}
	if len(rec.planPrompts) != 1 {
		t.Fatalf("budget error must NOT retry plan+execute, plan calls = %d", len(rec.planPrompts))
	}
}

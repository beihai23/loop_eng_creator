package loop

// verify_workdir_tier1_test.go 钉死 #81 修复：tier-2 verify 的模型调用 cwd = attempt
// worktree。生产装配里 verify 的 Skill.Model 恒为 *budget.Client（包真 agent），而它
// 原本只实现 Call、不是 DirClient → RunIn 会静默退化为 Call（cwd=守护进程根）。所以
// 光给 LLM 设 Dir 不够：budget.Client 必须实现 DirClient（CallIn 透传 dir）。本测试
// 用 budget-wrapped Model 钉这条链，避免「裸 DirClient fake 假绿灯」。

import (
	"context"
	"testing"

	"loop-eng/internal/budget"
	"loop-eng/internal/model"
	"loop-eng/internal/skill"
	"loop-eng/internal/verify"
)

// 复用 plan_workdir_tier1_test.go 的 dirRecordingClient（实现 Client/DirClient/Executer，
// 记录 CallIn 收到的目录）与 subloop_test.go 的 mkSkill / mustJSON。

// TestTier1VerifyDirBindsWorktree：Dir!="" 时，budget-wrapped verify Model 的 CallIn
// 收到 Dir（=生产链 budget.Client.CallIn→agentClient.CallIn→adapter cmd.Dir）。
func TestTier1VerifyDirBindsWorktree(t *testing.T) {
	fake := &dirRecordingClient{inner: model.NewFake(map[string]string{
		"VERIFY:": mustJSON(skill.VerifyOutput{Passed: true}),
	})}
	bc := &budget.Client{Base: fake, Enf: budget.New(100000, 1000000, 3)}
	llm := verify.LLM{
		Skill: mkSkill[skill.VerifyInput, skill.VerifyOutput]("VERIFY:", bc),
		Dir:   "/wt/attempts",
	}
	if _, err := llm.Check(context.Background(), "diff", []string{"c"}, ""); err != nil {
		t.Fatal(err)
	}
	if len(fake.planDirs) != 1 || fake.planDirs[0] != "/wt/attempts" {
		t.Fatalf("verify model call must bind worktree via budget.Client.CallIn, got planDirs=%v", fake.planDirs)
	}
}

// TestTier1VerifyDirEmptyFallsBack：Dir="" → RunIn 退化为 Call，不触发 CallIn（旧装配不变）。
func TestTier1VerifyDirEmptyFallsBack(t *testing.T) {
	fake := &dirRecordingClient{inner: model.NewFake(map[string]string{
		"VERIFY:": mustJSON(skill.VerifyOutput{Passed: true}),
	})}
	bc := &budget.Client{Base: fake, Enf: budget.New(100000, 1000000, 3)}
	llm := verify.LLM{Skill: mkSkill[skill.VerifyInput, skill.VerifyOutput]("VERIFY:", bc)} // Dir 空
	if _, err := llm.Check(context.Background(), "diff", []string{"c"}, ""); err != nil {
		t.Fatal(err)
	}
	if len(fake.planDirs) != 0 {
		t.Fatalf("empty Dir must fall back to Call (no CallIn), got planDirs=%v", fake.planDirs)
	}
}

// TestTier1VerifyDirBudgetPreCheck：CallIn 与 Call 同走预算前置闸——perTask<PerCall(=estimate)
// → BeforeCall 即拒、不调 Base（planDirs 不变）。
func TestTier1VerifyDirBudgetPreCheck(t *testing.T) {
	fake := &dirRecordingClient{inner: model.NewFake(map[string]string{
		"VERIFY:": mustJSON(skill.VerifyOutput{Passed: true}),
	})}
	tight := &budget.Client{Base: fake, Enf: budget.New(100, 1, 3)} // estimate=PerCall=100 > perTask=1
	if _, _, err := tight.CallIn(context.Background(), "/wt/x", "VERIFY:"); err == nil {
		t.Fatal("budget.Client.CallIn must run BeforeCall pre-check (over-budget rejects without calling Base)")
	}
	if len(fake.planDirs) != 0 {
		t.Fatalf("over-budget CallIn must not reach Base, got planDirs=%v", fake.planDirs)
	}
}

// TestTier1VerifyDirTiersForInjects：tiersFor 把 wt 注入 LLM.Dir；无 VerifyScript 时
// tiers[0]=llm。Tier 接口签名 / 链结构不变。
func TestTier1VerifyDirTiersForInjects(t *testing.T) {
	sl := &SubLoop{}
	tiers := sl.tiersFor("/wt-x", skill.PlanOutput{}, verify.LLM{})
	got, ok := tiers[0].(verify.LLM)
	if !ok {
		t.Fatalf("tier[0] must be verify.LLM, got %T", tiers[0])
	}
	if got.Dir != "/wt-x" {
		t.Fatalf("tiersFor must inject wt into LLM.Dir, got %q", got.Dir)
	}
}

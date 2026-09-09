package loop

// handoff_test.go —— 移交包组装/提取 + park 即 commit 的验收测试。

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"loop-eng/internal/budget"
	"loop-eng/internal/channel"
	"loop-eng/internal/model"
	"loop-eng/internal/skill"
	"loop-eng/internal/state"
	"loop-eng/internal/verify"
)

// TestBuildHandoffWiresAllFields 核验组装映射：轮次/预算、现场、环境提取、风险、
// 标准修订、token 透明度。
func TestBuildHandoffWiresAllFields(t *testing.T) {
	sl := &SubLoop{Budget: budget.New(1000, 10000, 3)}
	planOut := skill.PlanOutput{
		Plan:          []skill.PlanStep{{Step: "s"}},
		Risks:         []string{"并发路径未覆盖"},
		CriteriaNotes: "删掉了不可脚本化的第 3 条",
	}
	ho := sl.buildHandoff(2, "/wt", "loop/task_x-r2", planOut, true,
		"实现完成。\n### 环境与复现\n跑 make test", "- verify-FAIL — 缺错误处理", 100, 50)
	if ho.Attempt != 2 || ho.MaxRetries != 3 {
		t.Fatalf("attempt/maxRetries 接线错误: %d/%d", ho.Attempt, ho.MaxRetries)
	}
	if ho.Worktree != "/wt" || ho.Branch != "loop/task_x-r2" {
		t.Fatalf("现场接线错误: %+v", ho)
	}
	if !strings.Contains(ho.EnvNotes, "make test") {
		t.Fatalf("EnvNotes 应提取自 execOut: %q", ho.EnvNotes)
	}
	if len(ho.Risks) != 1 || ho.Risks[0] != "并发路径未覆盖" {
		t.Fatalf("Risks 接线错误: %v", ho.Risks)
	}
	if ho.CriteriaNotes != planOut.CriteriaNotes {
		t.Fatalf("criteriaRevised 时应带修订理由: %q", ho.CriteriaNotes)
	}
	if ho.TokensIn != 100 || ho.TokensOut != 50 {
		t.Fatalf("token 接线错误: %d/%d", ho.TokensIn, ho.TokensOut)
	}
	if !strings.Contains(ho.RunHistory, "verify-FAIL") {
		t.Fatalf("RunHistory 接线错误: %q", ho.RunHistory)
	}
	// 未修订时不带 CriteriaNotes（人审评论里不应出现空审计线索）。
	ho2 := sl.buildHandoff(1, "/wt", "b", planOut, false, "", "", 0, 0)
	if ho2.CriteriaNotes != "" {
		t.Fatalf("未修订不应带 CriteriaNotes: %q", ho2.CriteriaNotes)
	}
}

// TestExtractEnvNotes 核验宽松提取：命中 ###/## 级标题、同级截断、深层标题不截断、
// 无段返回空、#tag 非标题。
func TestExtractEnvNotes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"三级标题段", "干完了。\n### 环境与复现\n跑 make test\n注意 go1.25", "跑 make test\n注意 go1.25"},
		{"二级标题段", "## 环境与复现\n装了 foo", "装了 foo"},
		{"同级标题截断", "### 环境与复现\n内容A\n### 下一段\n内容B", "内容A"},
		{"更高级标题截断", "### 环境与复现\n内容A\n## 另一章\n内容B", "内容A"},
		{"深层标题不截断", "### 环境与复现\n内容A\n#### 细节\n内容B", "内容A\n#### 细节\n内容B"},
		{"无段返回空", "只有自报正文，没有环境段", ""},
		{"井号标签非标题", "#tag 不是标题\n### 环境与复现\n内容", "内容"},
	}
	for _, c := range cases {
		if got := extractEnvNotes(c.in); got != c.want {
			t.Errorf("%s: extractEnvNotes=%q want %q", c.name, got, c.want)
		}
	}
}

// TestSubLoopParkCommitsAndRecordsBranch 核验 0a：NeedsHuman park 前先 commit 分支
// 并落 land_branch——loop:accept 拦截据此落地人审过的提交，无需 worktree 存活。
func TestSubLoopParkCommitsAndRecordsBranch(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()
	fake := model.NewFake(map[string]string{
		"PLAN:":    validPlanJSON(),
		"EXECUTE:": "ok",
		"VERIFY:":  mustJSON(skill.VerifyOutput{Passed: true}),
	})
	sl := &SubLoop{
		Repo: repo, Store: st, Budget: budget.New(100000, 1000000, 3),
		Execute:    fake,
		Plan:       mkSkill[skill.PlanInput, skill.PlanOutput]("PLAN:", fake),
		VerifyLLM:  verify.LLM{Skill: mkSkill[skill.VerifyInput, skill.VerifyOutput]("VERIFY:", fake)},
		Tier3Human: true,
		HumanTier:  needsHumanTier{},
		Channel:    channel.NewLocal(t.TempDir()),
	}
	task := channel.Task{Ref: "77", Description: "d", AcceptanceCriteria: []string{"c"}}
	taskID, err := st.InsertTask(state.TaskRow{IssueRef: task.Ref, Description: task.Description, Criteria: task.AcceptanceCriteria})
	if err != nil {
		t.Fatal(err)
	}
	sl.PreinsertedTaskID = taskID
	out, err := sl.Run(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "needs-review" {
		t.Fatalf("want needs-review, got %s (%s)", out.Status, out.Detail)
	}
	if out.Branch == "" || out.Worktree == "" {
		t.Fatalf("park 必须携带已 commit 的分支与 worktree（accept 落地的输入），got branch=%q wt=%q", out.Branch, out.Worktree)
	}
	row, err := st.GetTask(taskID)
	if err != nil {
		t.Fatal(err)
	}
	if row.LandBranch != out.Branch {
		t.Fatalf("land_branch=%q want %q（accept 拦截按它找分支）", row.LandBranch, out.Branch)
	}
	// 分支真实存在于主仓库（对象库里有人审过的提交）。
	got, _ := exec.Command("git", "-C", repo, "branch", "--list", out.Branch).CombinedOutput()
	if strings.TrimSpace(string(got)) == "" {
		t.Fatalf("parked 分支 %s 必须已建在主仓库", out.Branch)
	}
}

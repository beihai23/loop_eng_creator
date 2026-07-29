package loop

// runhistory_tier1_test.go 钉死「run history 回灌」（见 runhistory.go）：
//   - BuildRunHistory 从 DB 构建有界摘要（每条已终结 run 一行：outcome + 驳回理由）；
//   - 跨 run：上一次 run 的 verify 驳回经 SQLite 进下一次 run 的 plan prompt——
//     战报评论只写不读后，这是机器跨 run 记忆的替代通道；
//   - collectIssueComments 滤除 daemon 自发评论（bot 标记 + 存量前缀），只留人反馈。

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

// seedFinishedRun 在 st 里造一轮已终结的 run（独立 task 行、同 issue_ref——模拟
// run-once 每跑一次的形态）：附一次 verify 驳回 step，以 outcome 收尾。
func seedFinishedRun(t *testing.T, st *state.Store, ref, verifyDetail, outcome string) {
	t.Helper()
	tid, err := st.InsertTask(state.TaskRow{IssueRef: ref, Description: "d", TaskType: "feat"})
	if err != nil {
		t.Fatalf("InsertTask: %v", err)
	}
	rid, err := st.StartRun(tid)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	vt, _ := json.Marshal(verifyTrace{Passed: false, Detail: verifyDetail})
	if err := st.AppendStep(state.StepRow{RunID: rid, Seq: 13, Role: "verify", Status: "fail", OutputJSON: string(vt)}); err != nil {
		t.Fatalf("AppendStep: %v", err)
	}
	if err := st.EndRun(rid, outcome); err != nil {
		t.Fatalf("EndRun: %v", err)
	}
}

// TestBuildRunHistory：摘要含 outcome + 驳回理由（截断前行）；nil store / 未知
// ref 返回空串（历史是增强信号，不是必需品）。
func TestBuildRunHistory(t *testing.T) {
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()
	seedFinishedRun(t, st, "#9", "缺 healthcheck endpoint", "blocked")

	got := BuildRunHistory(st, "#9")
	if !strings.Contains(got, "- blocked — 缺 healthcheck endpoint") {
		t.Fatalf("摘要缺 blocked 行: %q", got)
	}
	if BuildRunHistory(nil, "#9") != "" || BuildRunHistory(st, "#unknown") != "" {
		t.Fatal("nil store / 未知 ref 应返回空串")
	}
}

// TestSubLoopFeedsRunHistoryToPlan 端到端：上一次 run 的 verify 驳回（DB）进下一次
// run 的 plan prompt——战报评论只写不读后，跨 run 记忆改由这条 DB 通道承载。
func TestSubLoopFeedsRunHistoryToPlan(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()
	// 上一轮的失败史（run-once 形态：每次 Run 新建 task 行，靠 issue_ref 关联）。
	seedFinishedRun(t, st, "77", "verify 驳回：缺 healthcheck endpoint", "blocked")

	fake := model.NewFake(map[string]string{
		"PLAN:":    validPlanJSON(),
		"EXECUTE:": "ok",
		"VERIFY:":  mustJSON(skill.VerifyOutput{Passed: true}),
	})
	rec := &recorder{Client: fake}

	sl := &SubLoop{
		Repo: repo, Store: st, Budget: budget.New(100000, 1000000, 3),
		Execute: fake,
		Plan: skill.Skill[skill.PlanInput, skill.PlanOutput]{
			Name:       "plan",
			PromptTmpl: "PLAN:\n摘要: {{.RunHistory}}\n任务: {{.Task}}",
			ParseJSON: func(b []byte) (skill.PlanOutput, error) {
				var o skill.PlanOutput
				return o, json.Unmarshal(b, &o)
			},
			Model: rec,
		},
		VerifyLLM:  verify.LLM{Skill: mkSkill[skill.VerifyInput, skill.VerifyOutput]("VERIFY:", fake)},
		Tier3Human: true,
		Channel:    &fakeCommentChan{replies: map[string][]channel.Reply{}},
	}
	out, err := sl.Run(context.Background(), channel.Task{Ref: "77", Description: "d", AcceptanceCriteria: []string{"c"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "done" {
		t.Fatalf("want done, got %s (%s)", out.Status, out.Detail)
	}
	if len(rec.got) == 0 {
		t.Fatal("plan 未被调用")
	}
	if !strings.Contains(rec.got[0], "blocked — verify 驳回：缺 healthcheck endpoint") {
		t.Fatalf("plan prompt 缺失 DB 构建的历轮 run 摘要:\n%s", rec.got[0])
	}
}

// TestCollectIssueCommentsFiltersBot：daemon 自发评论（新标记 + 存量前缀）被滤除，
// 人写的评论（包括引用战报内容的）保留——issue 线程只剩「人说的话」进 prompt。
func TestCollectIssueCommentsFiltersBot(t *testing.T) {
	sl := &SubLoop{Channel: &fakeCommentChan{replies: map[string][]channel.Reply{
		"42": {
			{Body: channel.MarkBotComment("BLOCKED: retries exhausted: ...")},
			{Body: "## VERIFY-FAIL（第 1 轮 verify 驳回）\n\n### 失败理由\n旧战报（无标记的存量评论）"},
			{Body: "NEEDS-INFO: 分诊判断任务信息不足，暂不开工。"},
			{Body: "人回复一：标准写错了，按这个改"},
			{Body: "我看了 VERIFY-FAIL 的理由，其实应该……"},
		},
	}}}
	got := sl.collectIssueComments(context.Background(), "42")
	for _, bot := range []string{"BLOCKED: retries exhausted", "旧战报", "NEEDS-INFO"} {
		if strings.Contains(got, bot) {
			t.Errorf("daemon 自发评论未被滤除: %q in %q", bot, got)
		}
	}
	for _, human := range []string{"人回复一：标准写错了", "我看了 VERIFY-FAIL 的理由"} {
		if !strings.Contains(got, human) {
			t.Errorf("人反馈丢失: %q not in %q", human, got)
		}
	}
}

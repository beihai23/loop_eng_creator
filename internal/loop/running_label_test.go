package loop

import (
	"context"
	"testing"

	"loop-eng/internal/budget"
	"loop-eng/internal/channel"
	"loop-eng/internal/model"
	"loop-eng/internal/skill"
	"loop-eng/internal/state"
	"loop-eng/internal/verify"
)

// recordingStatusCh wraps channel.Local and records the status of every
// UpdateStatus call in order, so tests can assert the running mark lands at
// Run entry (before any terminal writeback).
type recordingStatusCh struct {
	*channel.Local
	statuses []string
}

func (r *recordingStatusCh) UpdateStatus(ctx context.Context, ref, status string) error {
	r.statuses = append(r.statuses, status)
	return r.Local.UpdateStatus(ctx, ref, status)
}

// TestRunningLabelOnDispatch 钉死 running 打标落点：SubLoop.Run 入口（任务
// →running）必须先打 loop:running，再谈任何终态写回——第一次 UpdateStatus 是
// "running"，终态（done）在其后。这样即便任务跑很久，channel 上也始终可见
// 「正在处理」。
func TestRunningLabelOnDispatch(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()
	fake := model.NewFake(map[string]string{
		"PLAN:":    validPlanJSON(),
		"EXECUTE:": "ok",
		"VERIFY:":  mustJSON(skill.VerifyOutput{Passed: true}),
	})
	ch := &recordingStatusCh{Local: channel.NewLocal(t.TempDir())}
	sl := &SubLoop{
		Repo: repo, Store: st, Budget: budget.New(100000, 1000000, 3),
		Execute:    fake,
		Plan:       mkSkill[skill.PlanInput, skill.PlanOutput]("PLAN:", fake),
		VerifyLLM:  verify.LLM{Skill: mkSkill[skill.VerifyInput, skill.VerifyOutput]("VERIFY:", fake)},
		Tier3Human: true,
		Channel:    ch,
	}
	out, err := sl.Run(context.Background(), channel.Task{Ref: "11", Description: "d", AcceptanceCriteria: []string{"c"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "done" {
		t.Fatalf("want done, got %s (%s)", out.Status, out.Detail)
	}
	if len(ch.statuses) == 0 || ch.statuses[0] != "running" {
		t.Fatalf("first UpdateStatus must be \"running\" (dispatch mark), got %v", ch.statuses)
	}
	if ch.statuses[len(ch.statuses)-1] != "done" {
		t.Fatalf("last UpdateStatus must be the terminal status \"done\", got %v", ch.statuses)
	}
}

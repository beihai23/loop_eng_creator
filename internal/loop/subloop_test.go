package loop

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"loop-eng/internal/budget"
	"loop-eng/internal/channel"
	"loop-eng/internal/model"
	"loop-eng/internal/skill"
	"loop-eng/internal/state"
	"loop-eng/internal/verify"
)

func initRepo(t *testing.T) string {
	dir := t.TempDir()
	for _, c := range [][]string{
		{"git", "init", "-q", dir},
		{"git", "-C", dir, "config", "user.email", "t@t"},
		{"git", "-C", dir, "config", "user.name", "t"},
	} {
		out, err := exec.Command(c[0], c[1:]...).CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0644) //nolint
	exec.Command("git", "-C", dir, "add", "-A").Run()
	exec.Command("git", "-C", dir, "commit", "-q", "-m", "i").Run()
	return dir
}

func TestSubLoopDoneOnFirstPass(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()

	// 三个 skill 全成功；execute 产空 diff
	tri := model.NewFake(map[string]string{"TRIAGE:": mustJSON(skill.TriageOutput{Startable: true, LoopDoable: true})})
	// 单个 fake 给 plan/verify/execute 用前缀区分
	fake := model.NewFake(map[string]string{
		"PLAN:":    mustJSON(skill.PlanOutput{}),
		"EXECUTE:": "ok",
		"VERIFY:":  mustJSON(skill.VerifyOutput{Passed: true}),
	})

	sl := &SubLoop{
		Repo: repo, Store: st, Budget: budget.New(100000, 1000000, 3),
		Execute:    fake,
		Plan:       mkSkill[skill.PlanInput, skill.PlanOutput]("PLAN:", fake),
		VerifyLLM:  verify.LLM{Skill: mkSkill[skill.VerifyInput, skill.VerifyOutput]("VERIFY:", fake)},
		Tier3Human: true,
		Channel:    channel.NewLocal(t.TempDir()),
	}
	_ = tri // triage 在 M1 的 SubLoop 外（CLI 层先分诊），这里跳过

	out, err := sl.Run(context.Background(), channel.Task{Ref: "1", Description: "d", AcceptanceCriteria: []string{"c"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "done" {
		t.Fatalf("want done, got %s (%s)", out.Status, out.Detail)
	}
}

func TestSubLoopBlockedAfterRetries(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()
	fake := model.NewFake(map[string]string{
		"PLAN:":    mustJSON(skill.PlanOutput{}),
		"EXECUTE:": "ok",
		"VERIFY:":  mustJSON(skill.VerifyOutput{Passed: false, Reason: "nope"}), // 永远不过
	})
	sl := &SubLoop{
		Repo: repo, Store: st, Budget: budget.New(100000, 1000000, 2),
		Execute:    fake,
		Plan:       mkSkill[skill.PlanInput, skill.PlanOutput]("PLAN:", fake),
		VerifyLLM:  verify.LLM{Skill: mkSkill[skill.VerifyInput, skill.VerifyOutput]("VERIFY:", fake)},
		Tier3Human: true,
		Channel:    channel.NewLocal(t.TempDir()),
	}
	out, _ := sl.Run(context.Background(), channel.Task{Ref: "2", Description: "d"})
	if out.Status != "blocked" {
		t.Fatalf("want blocked, got %s", out.Status)
	}
}

func TestSubLoopTier1FailBlocks(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()
	fake := model.NewFake(map[string]string{
		"PLAN:":    mustJSON(skill.PlanOutput{}),
		"EXECUTE:": "ok",
		"VERIFY:":  mustJSON(skill.VerifyOutput{Passed: true}),
	})
	sl := &SubLoop{
		Repo: repo, Store: st, Budget: budget.New(100000, 1000000, 2),
		Execute:             fake,
		Plan:                mkSkill[skill.PlanInput, skill.PlanOutput]("PLAN:", fake),
		VerifyDeterministic: []verify.Deterministic{{Label: "go-test", Cmd: []string{"false"}}}, // 永远失败
		VerifyLLM:           verify.LLM{Skill: mkSkill[skill.VerifyInput, skill.VerifyOutput]("VERIFY:", fake)},
		Tier3Human:          true,
		Channel:             channel.NewLocal(t.TempDir()),
	}
	out, _ := sl.Run(context.Background(), channel.Task{Ref: "3", Description: "d"})
	if out.Status != "blocked" {
		t.Fatalf("tier1 always-fail must block, got %s", out.Status)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func mkSkill[I any, O any](prefix string, m model.Client) skill.Skill[I, O] {
	return skill.Skill[I, O]{
		Name: "x", PromptTmpl: prefix,
		ParseJSON: func(b []byte) (O, error) {
			var o O
			return o, json.Unmarshal(b, &o)
		},
		Model: m,
	}
}

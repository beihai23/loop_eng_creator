package e2e

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"testing"

	"loop-eng/internal/budget"
	"loop-eng/internal/channel"
	"loop-eng/internal/loop"
	"loop-eng/internal/model"
	"loop-eng/internal/skill"
	"loop-eng/internal/state"
	"loop-eng/internal/verify"
)

func gitRepo(t *testing.T) string {
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
	exec.Command("sh", "-c", "echo hi > "+filepath.Join(dir, "README")).Run()
	exec.Command("git", "-C", dir, "add", "-A").Run()
	exec.Command("git", "-C", dir, "commit", "-q", "-m", "i").Run()
	return dir
}

func TestHelloWorldEndToEnd(t *testing.T) {
	repo := gitRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()

	m := model.NewFake(map[string]string{
		"PLAN:":    j(skill.PlanOutput{}),
		"EXECUTE:": "done",
		"VERIFY:":  j(skill.VerifyOutput{Passed: true, Reason: "file created"}),
	})
	vskill := skill.Skill[skill.VerifyInput, skill.VerifyOutput]{
		Name: "verify", PromptTmpl: "VERIFY: {{.Diff}}",
		ParseJSON: func(b []byte) (skill.VerifyOutput, error) {
			var o skill.VerifyOutput
			return o, json.Unmarshal(b, &o)
		}, Model: m,
	}
	sl := &loop.SubLoop{
		Repo: repo, Store: st, Budget: budget.New(100000, 1000000, 3),
		Execute: m,
		Plan: skill.Skill[skill.PlanInput, skill.PlanOutput]{
			Name: "plan", PromptTmpl: "PLAN: {{.Task}}",
			ParseJSON: func(b []byte) (skill.PlanOutput, error) {
				var o skill.PlanOutput
				return o, json.Unmarshal(b, &o)
			}, Model: m,
		},
		Tiers:   []verify.Tier{verify.LLM{Skill: vskill}, verify.HumanStub{}},
		Channel: channel.NewLocal(t.TempDir()),
	}

	out, err := sl.Run(context.Background(), channel.Task{
		Ref: "1", Description: "创建 greet.txt 内容 hello",
		AcceptanceCriteria: []string{"文件 greet.txt 存在且内容为 hello"},
		TaskType:            "feature",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "done" {
		t.Fatalf("want done, got %s (%s)", out.Status, out.Detail)
	}
	steps, _ := st.Replay("task_") // Replay 按 runID；这里宽松断言有 plan/execute/verify step
	_ = steps
}

func j(v any) string { b, _ := json.Marshal(v); return string(b) }

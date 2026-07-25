package loop

// contract_tier1_test.go 钉死「plan 冻结合同的双向可见性」（contract.go）——#71 三轮
// blocked（plan 测试与 execute 实现签名 4v3→3v2→2v3 振荡）的根治：
//  - execute 侧：本轮 plan 冻结的签名 + tier-1 验收脚本直达 execute prompt；
//  - plan 侧：上一轮冻结的合同回灌下一轮 plan（run 内 + 跨 run），默认保持稳定。

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
)

const contractSigMarker = "parseClaudeResult(raw string) (out string, usage Usage, ok bool)"
const contractScriptMarker = "CONTRACT-SCRIPT-MARKER-p5"

// contractPlanJSON：plan 冻结了签名（steps）+ 验收脚本（verify_script，body 带
// marker；run=true 使 tier-1 恒过，把裁决留给 tier-2）。
func contractPlanJSON() string {
	return mustJSON(skill.PlanOutput{
		Plan: []skill.PlanStep{{
			Step: "新增 " + contractSigMarker, Files: []string{"internal/model/claude.go"},
			Expected: "解析 envelope 成功",
		}},
		VerifyScript: &skill.PlanVerifyScript{
			Label: "p5", File: "p5_contract_check.sh",
			Body: "// " + contractScriptMarker + "\npackage model\n",
			Run:  []string{"true"},
		},
	})
}

func mkContractPlanSkill(m model.Client) skill.Skill[skill.PlanInput, skill.PlanOutput] {
	return skill.Skill[skill.PlanInput, skill.PlanOutput]{
		Name: "plan", PromptTmpl: "PLAN: {{.PriorPlanContract}}",
		ParseJSON: func(b []byte) (skill.PlanOutput, error) {
			var o skill.PlanOutput
			return o, json.Unmarshal(b, &o)
		},
		Model: m,
	}
}

// TestTier1ContractVisibleToExecuteAndNextPlan：
//  - attempt 1 的 execute prompt 已含本轮 plan 冻结的签名与验收脚本（不等驳回）；
//  - attempt 2 的 plan prompt 含 attempt 1 冻结的合同（run 内回灌）。
func TestTier1ContractVisibleToExecuteAndNextPlan(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()

	rec := &sceneRecorder{inner: model.NewFake(map[string]string{
		"PLAN:":    contractPlanJSON(),
		"EXECUTE:": "ok",
	})}
	sl := &SubLoop{
		Repo: repo, Store: st, Budget: budget.New(100000, 1000000, 3),
		Execute:    rec,
		Plan:       mkContractPlanSkill(rec),
		VerifyLLM:  mkSceneVerifySkill(&flipVerifyClient{}),
		Tier3Human: true,
		Channel:    channel.NewLocal(t.TempDir()),
	}
	out, err := sl.Run(context.Background(), channel.Task{Ref: "90", Description: "d", AcceptanceCriteria: []string{"c"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "done" {
		t.Fatalf("want done, got %s (%s)", out.Status, out.Detail)
	}
	if len(rec.planPrompts) != 2 || len(rec.execPrompts) != 2 {
		t.Fatalf("want 2 plan + 2 execute calls, got plan=%d exec=%d", len(rec.planPrompts), len(rec.execPrompts))
	}
	// execute 侧：第一轮就能看到本轮合同（签名 + 验收脚本体）
	if !strings.Contains(rec.execPrompts[0], contractSigMarker) {
		t.Error("attempt 1 execute prompt must contain plan's frozen signature (contract section)")
	}
	if !strings.Contains(rec.execPrompts[0], contractScriptMarker) {
		t.Error("attempt 1 execute prompt must contain the tier-1 verify script body")
	}
	// plan 侧：attempt 1 无既往合同；attempt 2 带 attempt 1 的合同
	if strings.Contains(rec.planPrompts[0], contractSigMarker) {
		t.Error("attempt 1 plan prompt must NOT contain any prior contract")
	}
	if !strings.Contains(rec.planPrompts[1], contractSigMarker) {
		t.Error("attempt 2 plan prompt must contain attempt 1's frozen contract")
	}
}

// TestTier1ContractFedBackAcrossRuns：run 1 末轮被驳回 blocked 后，run 2（同一
// issue ref、新 task 行）的 plan prompt 经 SQLite 拿到 run 1 冻结的合同。
func TestTier1ContractFedBackAcrossRuns(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()

	mk := func(verifyOut string) (*SubLoop, *sceneRecorder) {
		rec := &sceneRecorder{inner: model.NewFake(map[string]string{
			"PLAN:":    contractPlanJSON(),
			"EXECUTE:": "ok",
		})}
		sl := &SubLoop{
			Repo: repo, Store: st, Budget: budget.New(100000, 1000000, 1),
			Execute:    rec,
			Plan:       mkContractPlanSkill(rec),
			VerifyLLM:  mkSceneVerifySkill(staticClient{verifyOut}),
			Tier3Human: true,
			Channel:    channel.NewLocal(t.TempDir()),
		}
		return sl, rec
	}

	sl1, _ := mk(mustJSON(skill.VerifyOutput{Passed: false, Reason: "impl mismatch"}))
	out1, err := sl1.Run(context.Background(), channel.Task{Ref: "91", Description: "d", AcceptanceCriteria: []string{"c"}})
	if err != nil {
		t.Fatal(err)
	}
	if out1.Status != "blocked" {
		t.Fatalf("run 1: want blocked, got %s", out1.Status)
	}

	sl2, rec2 := mk(mustJSON(skill.VerifyOutput{Passed: true}))
	out2, err := sl2.Run(context.Background(), channel.Task{Ref: "91", Description: "d", AcceptanceCriteria: []string{"c"}})
	if err != nil {
		t.Fatal(err)
	}
	if out2.Status != "done" {
		t.Fatalf("run 2: want done, got %s (%s)", out2.Status, out2.Detail)
	}
	if len(rec2.planPrompts) == 0 || !strings.Contains(rec2.planPrompts[0], contractSigMarker) {
		t.Fatal("run 2 plan prompt must contain run 1's frozen contract (cross-run via store)")
	}
}

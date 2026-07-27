package loop

// verify_tokens_tier1_test.go 钉死「verify step 落真实 token」（修 verify 行恒为 0）：
//   - base 路径：FakeClient verify skill 返回非零 usage → verify step 行 tokens_in/out 非零。
//   - override 路径（agent_hints.verify）：tier-2 换 hint agent（固定 usage{11,22}，经
//     budget.Client 透传）→ verify step tokens 仍是真值（usage 旁路随 LLM 值拷贝到 override）。
// Tier.Check 签名不变（冻结契约）：LLM 加 *model.Usage 旁路，不改 Check 签名。

import (
    "context"
    "errors"
    "testing"

    "loop-eng/internal/budget"
    "loop-eng/internal/channel"
    "loop-eng/internal/model"
    "loop-eng/internal/skill"
    "loop-eng/internal/state"
    "loop-eng/internal/verify"
)

func TestTier1VerifyTokensBase(t *testing.T) {
    repo := initRepo(t)
    st, _ := state.Open(t.TempDir() + "/s.db")
    defer st.Close()
    taskID, _ := st.InsertTask(state.TaskRow{IssueRef: "200", Description: "d", Source: "test"})
    fake := model.NewFake(map[string]string{
        "PLAN:":    validPlanJSON(),
        "EXECUTE:": "ok",
        "VERIFY:":  mustJSON(skill.VerifyOutput{Passed: true}),
    })
    sl := &SubLoop{
        Repo:              repo,
        Store:             st,
        Budget:            budget.New(100000, 1000000, 3),
        Execute:           &defaultExec{inner: fake},
        Plan:              mkSkill[skill.PlanInput, skill.PlanOutput]("PLAN:", fake),
        VerifyLLM:         verify.LLM{Skill: mkSkill[skill.VerifyInput, skill.VerifyOutput]("VERIFY:", fake)},
        Tier3Human:        true,
        Channel:           channel.NewLocal(t.TempDir()),
        PreinsertedTaskID: taskID,
    }
    out, err := sl.Run(context.Background(), channel.Task{Ref: "200", Description: "d", AcceptanceCriteria: []string{"c"}})
    if err != nil || out.Status != "done" {
        t.Fatalf("want done, got %s (%v) %s", out.Status, err, out.Detail)
    }
    vs := stepsOf(t, st, taskID, "verify")
    if len(vs) != 1 {
        t.Fatalf("want 1 verify step, got %d (%+v)", len(vs), vs)
    }
    if vs[0].TokensIn == 0 || vs[0].TokensOut == 0 {
        t.Fatalf("verify step must land real usage, got TokensIn=%d TokensOut=%d", vs[0].TokensIn, vs[0].TokensOut)
    }
}

func TestTier1VerifyTokensOverride(t *testing.T) {
    repo := initRepo(t)
    st, _ := state.Open(t.TempDir() + "/s.db")
    defer st.Close()
    taskID, _ := st.InsertTask(state.TaskRow{IssueRef: "201", Description: "d", Source: "test"})
    hintPlan := mustJSON(skill.PlanOutput{Plan: validPlanSteps(), AgentHints: &skill.AgentHints{Verify: "codex"}})
    fake := model.NewFake(map[string]string{"PLAN:": hintPlan, "EXECUTE:": "ok"})
    codex := &fakeAgent{provider: "codex", out: mustJSON(skill.VerifyOutput{Passed: true})}
    sl := &SubLoop{
        Repo:              repo,
        Store:             st,
        Budget:            budget.New(100000, 1000000, 3),
        Execute:           &defaultExec{inner: fake},
        Plan:              mkSkill[skill.PlanInput, skill.PlanOutput]("PLAN:", fake),
        VerifyLLM:         verify.LLM{Skill: mkSkill[skill.VerifyInput, skill.VerifyOutput]("VERIFY:", fake)},
        Tier3Human:        true,
        Channel:           channel.NewLocal(t.TempDir()),
        PreinsertedTaskID: taskID,
        AgentForRole: func(role, provider string) (model.Agent, error) {
            if role == "verify" && provider == "codex" {
                return codex, nil
            }
            return nil, errors.New("unexpected factory call: " + role + "/" + provider)
        },
    }
    out, err := sl.Run(context.Background(), channel.Task{Ref: "201", Description: "d", AcceptanceCriteria: []string{"c"}})
    if err != nil || out.Status != "done" {
        t.Fatalf("want done, got %s (%v) %s", out.Status, err, out.Detail)
    }
    vs := stepsOf(t, st, taskID, "verify")
    if len(vs) != 1 || vs[0].ModelRef != "codex" {
        t.Fatalf("verify override step must run on codex, got %+v", vs)
    }
    if vs[0].TokensIn != 11 || vs[0].TokensOut != 22 {
        t.Fatalf("verify override step must land codex usage (11,22), got TokensIn=%d TokensOut=%d", vs[0].TokensIn, vs[0].TokensOut)
    }
}

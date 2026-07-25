package loop

// agent_hint_tier1_test.go 钉死 #71-B（步骤级 agent override）：
//  - plan 产出 agent_hints → 该 phase 用 hint 的 agent，steps.model_ref 反映真实
//    运行的 provider（不是角色默认）；
//  - 选择优先级 step hint → task（已烘进 role agent/label）→ role → 默认；
//  - hint 不可用（未知 provider）→ 回落角色配置，不崩；AgentForRole=nil（旧装配）
//    → hint 被忽略。

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

// fakeAgent 实现 model.Agent，记录 Run 次数并返回罐头输出。
type fakeAgent struct {
	provider string
	out      string
	runs     int
}

func (f *fakeAgent) Provider() string { return f.provider }
func (f *fakeAgent) Run(context.Context, model.AgentRequest) (model.AgentResult, error) {
	f.runs++
	return f.AgentResult(f.out), nil
}
func (f *fakeAgent) Check(context.Context) error { return nil }

// AgentResult 的小助手：让 fakeAgent.Run 的返回易读。
func (f *fakeAgent) AgentResult(out string) model.AgentResult {
	return model.AgentResult{Out: out, Usage: model.Usage{TokensIn: 11, TokensOut: 22}}
}

// defaultExec 记录默认 execute 被调用的次数（override 生效时它必须为 0）。
type defaultExec struct {
	inner *model.FakeClient
	runs  int
}

func (d *defaultExec) Exec(ctx context.Context, wt, p string) (string, model.Usage, error) {
	d.runs++
	return d.inner.Exec(ctx, wt, p)
}

func stepsOf(t *testing.T, st *state.Store, taskID, role string) []state.StepRow {
	t.Helper()
	steps, err := st.StepsOfTask(taskID)
	if err != nil {
		t.Fatal(err)
	}
	var out []state.StepRow
	for _, s := range steps {
		if s.Role == role {
			out = append(out, s)
		}
	}
	return out
}

// TestTier1StepAgentOverrideExecute：plan hint execute=codex → execute 用 codex
// agent（默认 executer 零调用），step 的 model_ref=codex 且 tokens 落真值。
func TestTier1StepAgentOverrideExecute(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()
	taskID, _ := st.InsertTask(state.TaskRow{IssueRef: "95", Description: "d", Source: "test"})

	hintPlan := mustJSON(skill.PlanOutput{Plan: validPlanSteps(), AgentHints: &skill.AgentHints{Execute: "codex"}})
	fake := model.NewFake(map[string]string{"PLAN:": hintPlan, "EXECUTE:": "ok", "VERIFY:": mustJSON(skill.VerifyOutput{Passed: true})})
	def := &defaultExec{inner: fake}
	codex := &fakeAgent{provider: "codex", out: "ok"}

	sl := &SubLoop{
		Repo: repo, Store: st, Budget: budget.New(100000, 1000000, 3),
		Execute:           def,
		Plan:              mkSkill[skill.PlanInput, skill.PlanOutput]("PLAN:", fake),
		VerifyLLM:         verify.LLM{Skill: mkSkill[skill.VerifyInput, skill.VerifyOutput]("VERIFY:", fake)},
		Tier3Human:        true,
		Channel:           channel.NewLocal(t.TempDir()),
		PreinsertedTaskID: taskID,
		ExecuteModelRef:   "claude", // role 默认；task 级无 override
		AgentForRole: func(role, provider string) (model.Agent, error) {
			if role == "execute" && provider == "codex" {
				return codex, nil
			}
			return nil, errors.New("unexpected factory call: " + role + "/" + provider)
		},
	}
	out, err := sl.Run(context.Background(), channel.Task{Ref: "95", Description: "d", AcceptanceCriteria: []string{"c"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "done" {
		t.Fatalf("want done, got %s (%s)", out.Status, out.Detail)
	}
	if codex.runs != 1 || def.runs != 0 {
		t.Fatalf("execute must run on the hinted agent: codex.runs=%d default.runs=%d", codex.runs, def.runs)
	}
	execSteps := stepsOf(t, st, taskID, "execute")
	if len(execSteps) != 1 || execSteps[0].ModelRef != "codex" {
		t.Fatalf("execute step model_ref must be the hinted provider, got %+v", execSteps)
	}
	if execSteps[0].TokensIn != 11 || execSteps[0].TokensOut != 22 {
		t.Fatalf("execute step tokens must be the real usage, got %+v", execSteps[0])
	}
}

// TestTier1StepAgentOverridePriority：step hint 压过 task 级 label；无 hint 时
// task/role 的 label 原样保留（优先级 step → task → role → 默认）。
func TestTier1StepAgentOverridePriority(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()

	run := func(ref string, hints *skill.AgentHints, taskLabel string) []state.StepRow {
		taskID, _ := st.InsertTask(state.TaskRow{IssueRef: ref, Description: "d", Source: "test"})
		po := skill.PlanOutput{Plan: validPlanSteps(), AgentHints: hints}
		fake := model.NewFake(map[string]string{"PLAN:": mustJSON(po), "EXECUTE:": "ok", "VERIFY:": mustJSON(skill.VerifyOutput{Passed: true})})
		claudeA := &fakeAgent{provider: "claude", out: "ok"}
		sl := &SubLoop{
			Repo: repo, Store: st, Budget: budget.New(100000, 1000000, 3),
			Execute:           &defaultExec{inner: fake},
			Plan:              mkSkill[skill.PlanInput, skill.PlanOutput]("PLAN:", fake),
			VerifyLLM:         verify.LLM{Skill: mkSkill[skill.VerifyInput, skill.VerifyOutput]("VERIFY:", fake)},
			Tier3Human:        true,
			Channel:           channel.NewLocal(t.TempDir()),
			PreinsertedTaskID: taskID,
			ExecuteModelRef:   taskLabel, // 模拟 task 级 override 已烘进 role label
			AgentForRole: func(role, provider string) (model.Agent, error) {
				return claudeA, nil
			},
		}
		if out, err := sl.Run(context.Background(), channel.Task{Ref: ref, Description: "d"}); err != nil || out.Status != "done" {
			t.Fatalf("ref=%s: want done, got %s (%v)", ref, out.Status, err)
		}
		return stepsOf(t, st, taskID, "execute")
	}

	// step hint "claude" 压过 task 级 label "codex"
	steps := run("96a", &skill.AgentHints{Execute: "claude"}, "codex")
	if steps[0].ModelRef != "claude" {
		t.Fatalf("step hint must beat task label, got model_ref=%q", steps[0].ModelRef)
	}
	// 无 hint → task 级 label 保留
	steps = run("96b", nil, "codex")
	if steps[0].ModelRef != "codex" {
		t.Fatalf("no hint: task label must stand, got model_ref=%q", steps[0].ModelRef)
	}
}

// TestTier1StepAgentOverrideFallback：hint 的 provider 工厂构建失败 → 回落角色
// 配置（默认 executer 顶上），不崩、不改 model_ref。AgentForRole=nil 时 hint 被忽略。
func TestTier1StepAgentOverrideFallback(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()

	hintPlan := mustJSON(skill.PlanOutput{Plan: validPlanSteps(), AgentHints: &skill.AgentHints{Execute: "no-such-provider"}})

	// 情形 1：工厂报错 → 回落默认 executer
	taskID, _ := st.InsertTask(state.TaskRow{IssueRef: "97a", Description: "d", Source: "test"})
	fake := model.NewFake(map[string]string{"PLAN:": hintPlan, "EXECUTE:": "ok", "VERIFY:": mustJSON(skill.VerifyOutput{Passed: true})})
	def := &defaultExec{inner: fake}
	sl := &SubLoop{
		Repo: repo, Store: st, Budget: budget.New(100000, 1000000, 3),
		Execute:           def,
		Plan:              mkSkill[skill.PlanInput, skill.PlanOutput]("PLAN:", fake),
		VerifyLLM:         verify.LLM{Skill: mkSkill[skill.VerifyInput, skill.VerifyOutput]("VERIFY:", fake)},
		Tier3Human:        true,
		Channel:           channel.NewLocal(t.TempDir()),
		PreinsertedTaskID: taskID,
		ExecuteModelRef:   "claude",
		AgentForRole:      func(role, provider string) (model.Agent, error) { return nil, errors.New("unknown provider") },
	}
	if out, err := sl.Run(context.Background(), channel.Task{Ref: "97a", Description: "d"}); err != nil || out.Status != "done" {
		t.Fatalf("fallback: want done, got %s (%v)", out.Status, err)
	}
	if def.runs != 1 {
		t.Fatalf("unusable hint must fall back to role default, default.runs=%d", def.runs)
	}
	if got := stepsOf(t, st, taskID, "execute")[0].ModelRef; got != "claude" {
		t.Fatalf("fallback: model_ref must stay role default, got %q", got)
	}

	// 情形 2：AgentForRole=nil（旧装配）→ hint 忽略
	taskID2, _ := st.InsertTask(state.TaskRow{IssueRef: "97b", Description: "d", Source: "test"})
	def2 := &defaultExec{inner: fake}
	sl2 := &SubLoop{
		Repo: repo, Store: st, Budget: budget.New(100000, 1000000, 3),
		Execute:           def2,
		Plan:              mkSkill[skill.PlanInput, skill.PlanOutput]("PLAN:", fake),
		VerifyLLM:         verify.LLM{Skill: mkSkill[skill.VerifyInput, skill.VerifyOutput]("VERIFY:", fake)},
		Tier3Human:        true,
		Channel:           channel.NewLocal(t.TempDir()),
		PreinsertedTaskID: taskID2,
	}
	if out, err := sl2.Run(context.Background(), channel.Task{Ref: "97b", Description: "d"}); err != nil || out.Status != "done" {
		t.Fatalf("nil factory: want done, got %s (%v)", out.Status, err)
	}
	if def2.runs != 1 {
		t.Fatalf("nil factory: hint must be ignored, default.runs=%d", def2.runs)
	}
}

// TestTier1StepAgentOverrideVerify：plan hint verify=codex → tier-2 用 codex
// agent（且保留预算装饰器路径不崩），verify step 的 model_ref=codex。
func TestTier1StepAgentOverrideVerify(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()
	taskID, _ := st.InsertTask(state.TaskRow{IssueRef: "98", Description: "d", Source: "test"})

	hintPlan := mustJSON(skill.PlanOutput{Plan: validPlanSteps(), AgentHints: &skill.AgentHints{Verify: "codex"}})
	fake := model.NewFake(map[string]string{"PLAN:": hintPlan, "EXECUTE:": "ok"})
	def := &defaultExec{inner: fake}
	codex := &fakeAgent{provider: "codex", out: mustJSON(skill.VerifyOutput{Passed: true})}

	sl := &SubLoop{
		Repo: repo, Store: st, Budget: budget.New(100000, 1000000, 3),
		Execute:           def,
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
	out, err := sl.Run(context.Background(), channel.Task{Ref: "98", Description: "d", AcceptanceCriteria: []string{"c"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "done" {
		t.Fatalf("want done, got %s (%s)", out.Status, out.Detail)
	}
	if codex.runs != 1 {
		t.Fatalf("verify must run on the hinted agent, codex.runs=%d", codex.runs)
	}
	if got := stepsOf(t, st, taskID, "verify")[0].ModelRef; got != "codex" {
		t.Fatalf("verify step model_ref must be the hinted provider, got %q", got)
	}
}

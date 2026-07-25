package loop

// scene_tier1_test.go 钉死「失败现场（rejected diff）回灌」（见 scene.go）：
//  - run 内：attempt N 被驳回的 diff 进 attempt N+1 的 plan/execute prompt；
//  - 跨 run：上一轮 run 的 execute diff 经 SQLite（按 issue_ref 跨 task 行）进下一次
//    run 的 plan prompt——判决（战报）之外，现场也能传到；
//  - blocked 末轮保留现场 worktree 供人排查（GC 按 TTL 回收，见 gc_tier1_test.go）。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"loop-eng/internal/budget"
	"loop-eng/internal/channel"
	"loop-eng/internal/model"
	"loop-eng/internal/skill"
	"loop-eng/internal/state"
	"loop-eng/internal/verify"
)

const sceneMarker = "SCENE-MARKER-7f3a9b"

// sceneRecorder 同时实现 Client / DirClient / Executer：记录 plan/execute 各自
// 收到的完整 prompt；Exec 时在 worktree 里写一个带 marker 的文件（产出真实 diff）。
type sceneRecorder struct {
	inner       *model.FakeClient
	planPrompts []string
	execPrompts []string
}

func (r *sceneRecorder) Call(ctx context.Context, p string) (string, model.Usage, error) {
	return r.inner.Call(ctx, p)
}

func (r *sceneRecorder) CallIn(ctx context.Context, dir, p string) (string, model.Usage, error) {
	r.planPrompts = append(r.planPrompts, p)
	return r.inner.Call(ctx, p)
}

func (r *sceneRecorder) Exec(ctx context.Context, wt, p string) (string, model.Usage, error) {
	r.execPrompts = append(r.execPrompts, p)
	_ = os.WriteFile(filepath.Join(wt, "scene.go"), []byte("package x\n// "+sceneMarker+"\n"), 0644)
	return r.inner.Exec(ctx, wt, p)
}

// staticClient 永远返回同一段输出（verify 恒过/恒拒用）。
type staticClient struct{ out string }

func (s staticClient) Call(context.Context, string) (string, model.Usage, error) {
	return s.out, model.Usage{}, nil
}

// flipVerifyClient 第一次拒绝、其后全过（run 内重试的现场回灌用）。
type flipVerifyClient struct{ calls int }

func (f *flipVerifyClient) Call(context.Context, string) (string, model.Usage, error) {
	f.calls++
	if f.calls == 1 {
		return mustJSON(skill.VerifyOutput{Passed: false, Reason: "reject-round-1"}), model.Usage{}, nil
	}
	return mustJSON(skill.VerifyOutput{Passed: true}), model.Usage{}, nil
}

// mkScenePlanSkill 构造把 RejectedDiff 渲染进 prompt 的 plan skill（mkSkill 的常量
// 模板渲染不出字段，这里用真模板）。
func mkScenePlanSkill(m model.Client) skill.Skill[skill.PlanInput, skill.PlanOutput] {
	return skill.Skill[skill.PlanInput, skill.PlanOutput]{
		Name: "plan", PromptTmpl: "PLAN: {{.RejectedDiff}}",
		ParseJSON: func(b []byte) (skill.PlanOutput, error) {
			var o skill.PlanOutput
			return o, json.Unmarshal(b, &o)
		},
		Model: m,
	}
}

func mkSceneVerifySkill(m model.Client) verify.LLM {
	s := skill.Skill[skill.VerifyInput, skill.VerifyOutput]{
		Name: "verify", PromptTmpl: "VERIFY:",
		ParseJSON: func(b []byte) (skill.VerifyOutput, error) {
			var o skill.VerifyOutput
			return o, json.Unmarshal(b, &o)
		},
		Model: m,
	}
	return verify.LLM{Skill: s}
}

// TestTier1SceneFedBackWithinRun：attempt 1 被驳回 → attempt 2 的 plan 与 execute
// prompt 都带上 attempt 1 的现场 diff（含 marker）与驳回理由；attempt 1 的 prompt 没有。
func TestTier1SceneFedBackWithinRun(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()

	rec := &sceneRecorder{inner: model.NewFake(map[string]string{
		"PLAN:":    validPlanJSON(),
		"EXECUTE:": "ok",
	})}
	sl := &SubLoop{
		Repo: repo, Store: st, Budget: budget.New(100000, 1000000, 3),
		Execute:    rec,
		Plan:       mkScenePlanSkill(rec),
		VerifyLLM:  mkSceneVerifySkill(&flipVerifyClient{}),
		Tier3Human: true,
		Channel:    channel.NewLocal(t.TempDir()),
	}
	out, err := sl.Run(context.Background(), channel.Task{Ref: "80", Description: "d", AcceptanceCriteria: []string{"c"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "done" {
		t.Fatalf("want done, got %s (%s)", out.Status, out.Detail)
	}
	if len(rec.planPrompts) != 2 || len(rec.execPrompts) != 2 {
		t.Fatalf("want 2 plan + 2 execute calls, got plan=%d exec=%d", len(rec.planPrompts), len(rec.execPrompts))
	}
	if strings.Contains(rec.planPrompts[0], sceneMarker) || strings.Contains(rec.execPrompts[0], sceneMarker) {
		t.Fatal("attempt 1 prompts must NOT contain any prior scene")
	}
	if !strings.Contains(rec.planPrompts[1], sceneMarker) {
		t.Fatal("attempt 2 plan prompt must contain the rejected diff (scene), got none")
	}
	if !strings.Contains(rec.execPrompts[1], sceneMarker) {
		t.Fatal("attempt 2 execute prompt must contain the rejected diff (scene), got none")
	}
	if !strings.Contains(rec.execPrompts[1], "reject-round-1") {
		t.Fatal("attempt 2 execute prompt must carry the rejection reason alongside the scene")
	}
}

// TestTier1SceneFedBackAcrossRuns：run 1 末轮被驳回 → blocked 且现场 worktree 保留；
// run 2（同一 issue ref，run-once 形态 = 新 task 行）的 plan prompt 经 SQLite 拿到
// run 1 的 execute diff（跨 task 行按 issue_ref 关联）。
func TestTier1SceneFedBackAcrossRuns(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()

	// ---- run 1：单 attempt（MaxRetries=1），verify 恒拒 → blocked + 现场保留 ----
	rec1 := &sceneRecorder{inner: model.NewFake(map[string]string{
		"PLAN:":    validPlanJSON(),
		"EXECUTE:": "ok",
	})}
	sl1 := &SubLoop{
		Repo: repo, Store: st, Budget: budget.New(100000, 1000000, 1),
		Execute:    rec1,
		Plan:       mkScenePlanSkill(rec1),
		VerifyLLM:  mkSceneVerifySkill(staticClient{mustJSON(skill.VerifyOutput{Passed: false, Reason: "still-wrong"})}),
		Tier3Human: true,
		Channel:    channel.NewLocal(t.TempDir()),
	}
	out1, err := sl1.Run(context.Background(), channel.Task{Ref: "81", Description: "d", AcceptanceCriteria: []string{"c"}})
	if err != nil {
		t.Fatal(err)
	}
	if out1.Status != "blocked" {
		t.Fatalf("run 1: want blocked, got %s (%s)", out1.Status, out1.Detail)
	}
	if !strings.Contains(out1.Detail, ".loop/worktrees") {
		t.Fatalf("blocked detail must name the preserved scene worktree, got: %s", out1.Detail)
	}
	kept, gerr := filepath.Glob(filepath.Join(repo, ".loop", "worktrees", "*"))
	if gerr != nil || len(kept) != 1 {
		t.Fatalf("final attempt's scene worktree must be preserved (exactly 1), got %v (%v)", kept, gerr)
	}
	if _, serr := os.Stat(filepath.Join(kept[0], "scene.go")); serr != nil {
		t.Fatalf("preserved scene worktree should hold the rejected code: %v", serr)
	}

	// ---- run 2：同一 issue ref、新 task 行；plan prompt 必须带上 run 1 的现场 ----
	rec2 := &sceneRecorder{inner: model.NewFake(map[string]string{
		"PLAN:":    validPlanJSON(),
		"EXECUTE:": "ok",
	})}
	sl2 := &SubLoop{
		Repo: repo, Store: st, Budget: budget.New(100000, 1000000, 3),
		Execute:    rec2,
		Plan:       mkScenePlanSkill(rec2),
		VerifyLLM:  mkSceneVerifySkill(staticClient{mustJSON(skill.VerifyOutput{Passed: true})}),
		Tier3Human: true,
		Channel:    channel.NewLocal(t.TempDir()),
	}
	out2, err := sl2.Run(context.Background(), channel.Task{Ref: "81", Description: "d", AcceptanceCriteria: []string{"c"}})
	if err != nil {
		t.Fatal(err)
	}
	if out2.Status != "done" {
		t.Fatalf("run 2: want done, got %s (%s)", out2.Status, out2.Detail)
	}
	if len(rec2.planPrompts) == 0 || !strings.Contains(rec2.planPrompts[0], sceneMarker) {
		t.Fatal("run 2 plan prompt must contain run 1's rejected diff (cross-run scene via store)")
	}
}

package loop

// plan_workdir_tier1_test.go 钉死「plan 在 attempt 的 worktree 里跑」（spec §8.9）：
// worktree 在 attempt 开头创建，plan/execute/verify 共用；plan 阶段失败同样即建即弃，
// 不留 worktree 目录与 loop/* 分支残渣。

import (
	"context"
	"os"
	"os/exec"
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

// dirRecordingClient 同时实现 Client / DirClient / Executer：记录 plan（CallIn）
// 与 execute（Exec）各自拿到的目录，供测试断言两者是同一棵 worktree。
type dirRecordingClient struct {
	inner    *model.FakeClient
	planDirs []string
	execDirs []string
}

func (d *dirRecordingClient) Call(ctx context.Context, prompt string) (string, model.Usage, error) {
	return d.inner.Call(ctx, prompt)
}

func (d *dirRecordingClient) CallIn(ctx context.Context, dir, prompt string) (string, model.Usage, error) {
	d.planDirs = append(d.planDirs, dir)
	return d.inner.Call(ctx, prompt)
}

func (d *dirRecordingClient) Exec(ctx context.Context, wt, prompt string) (string, model.Usage, error) {
	d.execDirs = append(d.execDirs, wt)
	return d.inner.Exec(ctx, wt, prompt)
}

// TestTier1PlanRunsInAttemptWorktree：plan 的模型调用拿到 attempt 的 worktree 目录
// （经 DirClient.CallIn），且与 execute 拿到的是同一棵；done 后该树保留并作为
// Outcome.Worktree 交给调用方。
func TestTier1PlanRunsInAttemptWorktree(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()

	rec := &dirRecordingClient{inner: model.NewFake(map[string]string{
		"PLAN:":    validPlanJSON(),
		"EXECUTE:": "ok",
		"VERIFY:":  mustJSON(skill.VerifyOutput{Passed: true}),
	})}
	sl := &SubLoop{
		Repo: repo, Store: st, Budget: budget.New(100000, 1000000, 3),
		Execute:    rec,
		Plan:       mkSkill[skill.PlanInput, skill.PlanOutput]("PLAN:", rec),
		VerifyLLM:  verify.LLM{Skill: mkSkill[skill.VerifyInput, skill.VerifyOutput]("VERIFY:", rec)},
		Tier3Human: true,
		Channel:    channel.NewLocal(t.TempDir()),
	}
	out, err := sl.Run(context.Background(), channel.Task{Ref: "70", Description: "d", AcceptanceCriteria: []string{"c"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "done" {
		t.Fatalf("want done, got %s (%s)", out.Status, out.Detail)
	}
	if len(rec.planDirs) != 1 || len(rec.execDirs) != 1 {
		t.Fatalf("want 1 plan + 1 execute call, got plan=%d exec=%d", len(rec.planDirs), len(rec.execDirs))
	}
	if rec.planDirs[0] == "" {
		t.Fatal("plan must run inside the attempt worktree (RunIn dir), got empty dir")
	}
	if rec.planDirs[0] != rec.execDirs[0] {
		t.Fatalf("plan and execute must share the attempt worktree: plan=%q exec=%q", rec.planDirs[0], rec.execDirs[0])
	}
	if rec.planDirs[0] != out.Worktree {
		t.Fatalf("plan worktree should be the preserved done worktree: plan=%q out=%q", rec.planDirs[0], out.Worktree)
	}
	if !strings.Contains(filepath.ToSlash(rec.planDirs[0]), ".loop/worktrees/") {
		t.Fatalf("plan dir should live under .loop/worktrees, got %q", rec.planDirs[0])
	}
	if fi, serr := os.Stat(rec.planDirs[0]); serr != nil || !fi.IsDir() {
		t.Fatalf("attempt worktree should exist on disk: %v", serr)
	}
}

// TestTier1PlanFailureDiscardsWorktree：plan 每轮都失败（fake 无 PLAN: 前缀 → 调用
// 报错）→ blocked；每轮 attempt 创建的 worktree 必须随即丢弃——.loop/worktrees 下
// 不留目录、git 里不留 loop/* 分支。
func TestTier1PlanFailureDiscardsWorktree(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()

	fake := model.NewFake(map[string]string{
		"EXECUTE:": "ok",
		"VERIFY:":  mustJSON(skill.VerifyOutput{Passed: true}),
	})
	sl := &SubLoop{
		Repo: repo, Store: st, Budget: budget.New(100000, 1000000, 2),
		Execute:    fake,
		Plan:       mkSkill[skill.PlanInput, skill.PlanOutput]("PLAN:", fake),
		VerifyLLM:  verify.LLM{Skill: mkSkill[skill.VerifyInput, skill.VerifyOutput]("VERIFY:", fake)},
		Tier3Human: true,
		Channel:    channel.NewLocal(t.TempDir()),
	}
	out, _ := sl.Run(context.Background(), channel.Task{Ref: "71", Description: "d"})
	if out.Status != "blocked" {
		t.Fatalf("want blocked, got %s (%s)", out.Status, out.Detail)
	}
	left, gerr := filepath.Glob(filepath.Join(repo, ".loop", "worktrees", "*"))
	if gerr != nil {
		t.Fatal(gerr)
	}
	if len(left) != 0 {
		t.Fatalf("failed attempts must discard their worktrees, left: %v", left)
	}
	branches, berr := exec.Command("git", "-C", repo, "branch", "--list", "loop/*").Output()
	if berr != nil {
		t.Fatal(berr)
	}
	if s := strings.TrimSpace(string(branches)); s != "" {
		t.Fatalf("failed attempts must delete loop/* branches, left: %q", s)
	}
}

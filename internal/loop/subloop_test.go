package loop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"loop-eng/internal/budget"
	"loop-eng/internal/channel"
	"loop-eng/internal/isolation"
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
	// plan 产出一个永远失败的 tier-1 验收脚本（run=false）→ tier-1 挂、短路 → blocked。
	// tier-1 完全来自 plan，无静态 config 列表。
	fake := model.NewFake(map[string]string{
		"PLAN:":    mustJSON(skill.PlanOutput{VerifyScript: &skill.PlanVerifyScript{Label: "go-test", Run: []string{"false"}}}),
		"EXECUTE:": "ok",
		"VERIFY:":  mustJSON(skill.VerifyOutput{Passed: true}), // tier-2 本会过，但 tier-1 先短路
	})
	sl := &SubLoop{
		Repo: repo, Store: st, Budget: budget.New(100000, 1000000, 2),
		Execute:    fake,
		Plan:       mkSkill[skill.PlanInput, skill.PlanOutput]("PLAN:", fake),
		VerifyLLM:  verify.LLM{Skill: mkSkill[skill.VerifyInput, skill.VerifyOutput]("VERIFY:", fake)},
		Tier3Human: true,
		Channel:    channel.NewLocal(t.TempDir()),
	}
	out, _ := sl.Run(context.Background(), channel.Task{Ref: "3", Description: "d"})
	if out.Status != "blocked" {
		t.Fatalf("plan tier-1 always-fail must block, got %s", out.Status)
	}
}

// TestSubLoopPlanScriptRunsTier1 钉死「plan 产出脚本 → tier-1 执行」：plan 产出一个
// 会通过的 tier-1 验收脚本（在 worktree 写一个 sentinel 文件后 exit 0）。tier-2 也过
// ⇒ done，且 sentinel 文件存在 + 逐 tier 行里有 tier=1 已过 ⇒ tier-1 脚本确实在 worktree
// 里跑了（不是被跳过）。tier-1 完全来自 plan，无静态/兜底列表。
func TestSubLoopPlanScriptRunsTier1(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()
	script := &skill.PlanVerifyScript{
		Label: "sentinel",
		File:  "verify_tier1.sh",
		Body:  "#!/bin/sh\necho ran > tier1-ran\n",
		Run:   []string{"sh", "verify_tier1.sh"},
	}
	fake := model.NewFake(map[string]string{
		"PLAN:":    mustJSON(skill.PlanOutput{VerifyScript: script}),
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
	out, err := sl.Run(context.Background(), channel.Task{Ref: "60", Description: "d", AcceptanceCriteria: []string{"c"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "done" {
		t.Fatalf("want done, got %s (%s)", out.Status, out.Detail)
	}
	// sentinel 文件存在 ⇒ tier-1 脚本确在 worktree 里跑过
	got, rerr := os.ReadFile(filepath.Join(out.Worktree, "tier1-ran"))
	if rerr != nil {
		t.Fatalf("tier-1 sentinel not written — script did not run: %v", rerr)
	}
	if strings.TrimSpace(string(got)) != "ran" {
		t.Fatalf("sentinel content = %q, want %q", got, "ran")
	}
	// 逐 tier 行里必有 tier=1 且过——对称地证明 tier-1 在场（与下面的 skip 测试对照）
	t1 := false
	for _, v := range verificationsOf(t, st) {
		if v.Tier == 1 && v.Passed {
			t1 = true
		}
	}
	if !t1 {
		t.Fatalf("expected a passing tier=1 verification row, got %+v", verificationsOf(t, st))
	}
}

// TestSubLoopNoPlanScriptSkipsTier1 钉死「plan 不产出 → 跳过 tier-1 落 tier-2」：plan
// VerifyScript=nil ⇒ Deterministic tier 不挂，链 = [LLM, HumanStub]。逐 tier 行恰好 2 条
// （产脚本时是 3 条）+ done ⇒ deterministic tier-1 没跑、LLM（tier-2）拍板通过。
func TestSubLoopNoPlanScriptSkipsTier1(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()
	fake := model.NewFake(map[string]string{
		"PLAN:":    mustJSON(skill.PlanOutput{}), // 无 VerifyScript —— 不可脚本化
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
	out, err := sl.Run(context.Background(), channel.Task{Ref: "61", Description: "d", AcceptanceCriteria: []string{"c"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "done" {
		t.Fatalf("want done, got %s (%s)", out.Status, out.Detail)
	}
	vs := verificationsOf(t, st)
	// plan 没产脚本 ⇒ tiersFor 不挂 Deterministic，链 = [LLM, HumanStub] ⇒ 恰好 2 条逐
	// tier 行（产脚本时会是 3 条）。结合 done（LLM 必过）证明 deterministic tier-1 缺席。
	if len(vs) != 2 {
		t.Fatalf("want exactly 2 verification rows (LLM + human-stub; no deterministic tier-1), got %d: %+v", len(vs), vs)
	}
}

// TestSubLoopInvalidPlanScriptSkipsTier1：plan 产出了一个非法脚本（缺运行命令）⇒ SubLoop
// 丢弃它、tier-1 缺席、落 tier-2（不制造假绿灯、也不整任务失败）。验证链第一条须是 tier=2。
func TestSubLoopInvalidPlanScriptSkipsTier1(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()
	fake := model.NewFake(map[string]string{
		"PLAN:":    mustJSON(skill.PlanOutput{VerifyScript: &skill.PlanVerifyScript{Label: "bad"}}), // 无 Run
		"EXECUTE:": "ok",
		"VERIFY:":  mustJSON(skill.VerifyOutput{Passed: true}),
	})
	var buf bytes.Buffer
	sl := &SubLoop{
		Repo: repo, Store: st, Budget: budget.New(100000, 1000000, 3),
		Execute:    fake,
		Plan:       mkSkill[skill.PlanInput, skill.PlanOutput]("PLAN:", fake),
		VerifyLLM:  verify.LLM{Skill: mkSkill[skill.VerifyInput, skill.VerifyOutput]("VERIFY:", fake)},
		Tier3Human: true,
		Channel:    channel.NewLocal(t.TempDir()),
		Log:        log.New(&buf, "", log.Lmsgprefix),
	}
	out, err := sl.Run(context.Background(), channel.Task{Ref: "62", Description: "d", AcceptanceCriteria: []string{"c"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "done" {
		t.Fatalf("invalid plan script must fall to tier-2 (done), got %s (%s)", out.Status, out.Detail)
	}
	if !strings.Contains(buf.String(), "verify_script invalid") {
		t.Fatalf("expected an invalid-script log line, got:\n%s", buf.String())
	}
	// 非法脚本被丢 ⇒ 同样不挂 Deterministic，链 = [LLM, HumanStub] ⇒ 2 条逐 tier 行。
	if len(verificationsOf(t, st)) != 2 {
		t.Fatalf("invalid plan script must be dropped → 2 verification rows (no deterministic tier-1), got %+v", verificationsOf(t, st))
	}
}

// TestTiersForPlanDriven 是 tiersFor 的直接单测——权威地钉死「tier-1 完全来自 plan」：
// plan 产出合法脚本 ⇒ 链首是 verify.Deterministic；未产出 / 产出非法 ⇒ 没有 Deterministic，
// LLM 成为链首（落 tier-2）。无任何静态/兜底来源。tier 编号是位置序号（Chain 里 i+1），
// 故这里按类型断言，而不是按 tier 号。
func TestTiersForPlanDriven(t *testing.T) {
	sl := &SubLoop{Tier3Human: true}

	// plan 产出合法脚本（纯命令）→ Deterministic 在链首。
	with := skill.PlanOutput{VerifyScript: &skill.PlanVerifyScript{Label: "go-test", Run: []string{"go", "test", "./..."}}}
	tiers := sl.tiersFor("/wt", with)
	if len(tiers) != 3 {
		t.Fatalf("valid script: want 3 tiers (det+llm+human), got %d", len(tiers))
	}
	d, ok := tiers[0].(verify.Deterministic)
	if !ok {
		t.Fatalf("valid script: tier[0] must be verify.Deterministic, got %T", tiers[0])
	}
	if d.Dir != "/wt" || len(d.Cmd) != 3 || d.Cmd[0] != "go" {
		t.Fatalf("deterministic tier not wired from plan script: %+v", d)
	}

	// plan 产出带 body 的脚本 → body/file 透传到 Deterministic。
	withBody := skill.PlanOutput{VerifyScript: &skill.PlanVerifyScript{File: "v.sh", Body: "echo ok", Run: []string{"sh", "v.sh"}}}
	tiers2 := sl.tiersFor("/wt", withBody)
	d2, _ := tiers2[0].(verify.Deterministic)
	if d2.ScriptFile != "v.sh" || d2.ScriptBody != "echo ok" {
		t.Fatalf("script body/file not passed through: %+v", d2)
	}

	// plan 未产出 → 无 Deterministic，LLM 是链首。
	none := skill.PlanOutput{}
	tiers3 := sl.tiersFor("/wt", none)
	if _, ok := tiers3[0].(verify.Deterministic); ok {
		t.Fatal("no script: tier[0] must NOT be Deterministic (tier-1 absent)")
	}
	if _, ok := tiers3[0].(verify.LLM); !ok {
		t.Fatalf("no script: tier[0] must be verify.LLM (fall to tier-2), got %T", tiers3[0])
	}

	// plan 产出非法（缺 Run）→ 同样无 Deterministic。
	invalid := skill.PlanOutput{VerifyScript: &skill.PlanVerifyScript{Label: "bad"}}
	tiers4 := sl.tiersFor("/wt", invalid)
	if _, ok := tiers4[0].(verify.Deterministic); ok {
		t.Fatal("invalid script: tier[0] must NOT be Deterministic (dropped to tier-2)")
	}
}

// verificationsOf returns the per-tier verification rows for the (single) run of
// the (single) task these subloop tests insert. Run inserts the task itself, so
// its status row is the one and only task; that task has exactly one run here.
func verificationsOf(t *testing.T, st *state.Store) []state.VerificationRow {
	t.Helper()
	statuses, err := st.ListStatuses()
	if err != nil || len(statuses) != 1 {
		t.Fatalf("want exactly 1 task status, got %d (err %v)", len(statuses), err)
	}
	runs, err := st.RunsOfTask(statuses[0].ID)
	if err != nil || len(runs) != 1 {
		t.Fatalf("want 1 run, got %d (err %v)", len(runs), err)
	}
	vs, err := st.VerificationsByRun(runs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	return vs
}

// TestSubLoopWritesBudgetLedger closes the M1 deferral: every budget check in
// SubLoop.Run must durably append a row to budget_ledger (spec §8.8). A
// first-pass done run exercises the retry brake (loop entry) and the per-call
// token brake (plan + execute BeforeCall), so the ledger must hold ≥1 row.
func TestSubLoopWritesBudgetLedger(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()
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
	out, err := sl.Run(context.Background(), channel.Task{Ref: "1", Description: "d", AcceptanceCriteria: []string{"c"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "done" {
		t.Fatalf("want done, got %s (%s)", out.Status, out.Detail)
	}
	// Run inserts the task itself; recover its id, then the run id (budget_ledger
	// 现按 run_id 落库——spec §4.1/§8.8，修 replay 交错 bug）来 scope 本次读取。
	statuses, err := st.ListStatuses()
	if err != nil || len(statuses) != 1 {
		t.Fatalf("want exactly 1 task status, got %d (err %v)", len(statuses), err)
	}
	runs, err := st.RunsOfTask(statuses[0].ID)
	if err != nil || len(runs) != 1 {
		t.Fatalf("want 1 run for the done task, got %d (err %v)", len(runs), err)
	}
	rows, err := st.BudgetLedger(runs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) < 1 {
		t.Fatalf("budget_ledger must have ≥1 row for a done run, got 0")
	}
	// plan + execute both run before a done verify ⇒ at least one tokens row.
	hasTokens := false
	for _, r := range rows {
		if r.Kind == "tokens" {
			hasTokens = true
		}
	}
	if !hasTokens {
		t.Fatalf("budget_ledger missing a tokens check row: %+v", rows)
	}
}

// TestSubLoopWritesStatusDone closes the M2 deferral (§7.2d status mark): a
// done outcome must mark the ticket via Channel.UpdateStatus — for Local that
// means a <root>/status/<ref> file whose content is the lowercase "done".
func TestSubLoopWritesStatusDone(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()
	fake := model.NewFake(map[string]string{
		"PLAN:":    mustJSON(skill.PlanOutput{}),
		"EXECUTE:": "ok",
		"VERIFY:":  mustJSON(skill.VerifyOutput{Passed: true}),
	})
	root := t.TempDir()
	sl := &SubLoop{
		Repo: repo, Store: st, Budget: budget.New(100000, 1000000, 3),
		Execute:    fake,
		Plan:       mkSkill[skill.PlanInput, skill.PlanOutput]("PLAN:", fake),
		VerifyLLM:  verify.LLM{Skill: mkSkill[skill.VerifyInput, skill.VerifyOutput]("VERIFY:", fake)},
		Tier3Human: true,
		Channel:    channel.NewLocal(root),
	}
	out, err := sl.Run(context.Background(), channel.Task{Ref: "7", Description: "d", AcceptanceCriteria: []string{"c"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "done" {
		t.Fatalf("want done, got %s (%s)", out.Status, out.Detail)
	}
	got, rerr := os.ReadFile(filepath.Join(root, "status", "7"))
	if rerr != nil {
		t.Fatalf("status/<ref> not written for done: %v", rerr)
	}
	if string(got) != "done" {
		t.Fatalf("status file content = %q, want %q", string(got), "done")
	}
}

// TestSubLoopWritesStatusBlocked: plan 产出 tier-1 脚本（run=false 永失败）→ blocked，
// 且 blocked 结局必须把工单态标成 "blocked"——Local 即 status/<ref> == "blocked"。
func TestSubLoopWritesStatusBlocked(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()
	fake := model.NewFake(map[string]string{
		"PLAN:":    mustJSON(skill.PlanOutput{VerifyScript: &skill.PlanVerifyScript{Label: "go-test", Run: []string{"false"}}}), // tier-1 永失败
		"EXECUTE:": "ok",
		"VERIFY:":  mustJSON(skill.VerifyOutput{Passed: true}),
	})
	root := t.TempDir()
	sl := &SubLoop{
		Repo: repo, Store: st, Budget: budget.New(100000, 1000000, 2),
		Execute:    fake,
		Plan:       mkSkill[skill.PlanInput, skill.PlanOutput]("PLAN:", fake),
		VerifyLLM:  verify.LLM{Skill: mkSkill[skill.VerifyInput, skill.VerifyOutput]("VERIFY:", fake)},
		Tier3Human: true,
		Channel:    channel.NewLocal(root),
	}
	out, _ := sl.Run(context.Background(), channel.Task{Ref: "8", Description: "d"})
	if out.Status != "blocked" {
		t.Fatalf("tier1 always-fail must block, got %s", out.Status)
	}
	got, rerr := os.ReadFile(filepath.Join(root, "status", "8"))
	if rerr != nil {
		t.Fatalf("status/<ref> not written for blocked: %v", rerr)
	}
	if string(got) != "blocked" {
		t.Fatalf("status file content = %q, want %q", string(got), "blocked")
	}
}

// TestSubLoopUpdateStatusErrorSurfaced: when Channel.UpdateStatus fails, the
// error must surface (stderr + Detail) but must NOT flip the outcome status —
// the same contract as PostComment. errStatusCh isolates the status leg by
// failing only UpdateStatus (PostComment still succeeds via the embedded Local).
func TestSubLoopUpdateStatusErrorSurfaced(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()
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
		Channel:    errStatusCh{Local: channel.NewLocal(t.TempDir())},
	}
	out, err := sl.Run(context.Background(), channel.Task{Ref: "9", Description: "d", AcceptanceCriteria: []string{"c"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "done" {
		t.Fatalf("UpdateStatus error must not flip status: want done, got %s", out.Status)
	}
	if !strings.Contains(out.Detail, "boom status") || !strings.Contains(out.Detail, "writeback partial: status") {
		t.Fatalf("UpdateStatus error must surface in Detail, got %q", out.Detail)
	}
}

// TestSubLoopNeedsHumanRoutesToNeedsReview closes the M3 tier-3 prep (spec
// §7.2c/§8.6/§10): when a tier returns NeedsHuman=true, SubLoop.Run must route
// to needs-review (park) — BEFORE the Passed check — even though Passed=false
// would otherwise feed back and retry. The NeedsHuman-emitting tier is injected
// via SubLoop.HumanTier; tier1/tier2 pass so the chain reaches tier-3.
func TestSubLoopNeedsHumanRoutesToNeedsReview(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()
	fake := model.NewFake(map[string]string{
		"PLAN:":    mustJSON(skill.PlanOutput{}),
		"EXECUTE:": "ok",
		"VERIFY:":  mustJSON(skill.VerifyOutput{Passed: true}), // tier1/2 过 → chain 走到 tier-3
	})
	root := t.TempDir()
	sl := &SubLoop{
		Repo: repo, Store: st, Budget: budget.New(100000, 1000000, 3),
		Execute:    fake,
		Plan:       mkSkill[skill.PlanInput, skill.PlanOutput]("PLAN:", fake),
		VerifyLLM:  verify.LLM{Skill: mkSkill[skill.VerifyInput, skill.VerifyOutput]("VERIFY:", fake)},
		Tier3Human: true,
		HumanTier:  needsHumanTier{}, // 注入产 NeedsHuman 的 tier-3 人审 tier
		Channel:    channel.NewLocal(root),
	}
	out, err := sl.Run(context.Background(), channel.Task{Ref: "11", Description: "d", AcceptanceCriteria: []string{"c"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "needs-review" {
		t.Fatalf("NeedsHuman must route to needs-review (before Passed), got %s (%s)", out.Status, out.Detail)
	}
	// writeback 仍触发：needs-review 的 status mark 落到 status/<ref>。
	got, rerr := os.ReadFile(filepath.Join(root, "status", "11"))
	if rerr != nil {
		t.Fatalf("status/<ref> not written for needs-review: %v", rerr)
	}
	if string(got) != "needs-review" {
		t.Fatalf("status file content = %q, want %q", string(got), "needs-review")
	}
}

// needsHumanTier is an M3 tier-3 stand-in injected via SubLoop.HumanTier: it
// emits NeedsHuman so SubLoop.Run parks the task at needs-review regardless of
// Passed. Mirrors what a real human-review tier (issue-comment-backed) will do.
type needsHumanTier struct{}

func (needsHumanTier) Check(context.Context, string, []string, string) (verify.VerifyResult, error) {
	return verify.VerifyResult{Passed: false, NeedsHuman: true, Detail: "needs human review"}, nil
}

// errStatusCh wraps *channel.Local but fails only UpdateStatus, to verify the
// status-leg writeback error surfaces in Detail without flipping the outcome
// status (same contract as PostComment). PostComment etc. stay promoted from
// the embedded Local, so this isolates the third writeback leg cleanly.
type errStatusCh struct{ *channel.Local }

func (errStatusCh) UpdateStatus(context.Context, string, string) error {
	return errors.New("boom status")
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

// fileWriteExec is a test Executer that writes a file into the worktree, so the
// done path has a non-empty diff to commit (the default FakeClient returns "ok"
// with no file changes → empty diff). Drives TestSubLoopDoneCommitsWorktree.
type fileWriteExec struct{}

func (fileWriteExec) Exec(_ context.Context, dir, _ string) (string, model.Usage, error) {
	if err := os.WriteFile(filepath.Join(dir, "landed.txt"), []byte("done work"), 0644); err != nil {
		return "", model.Usage{}, err
	}
	return "ok", model.Usage{}, nil
}

// TestSubLoopDoneCommitsWorktree guards the done-land fix: on Passed, SubLoop
// must commit the worktree's changes on its branch and expose Worktree+Branch
// in the Outcome so the caller can FF-merge to main. Previously the done path
// left the worktree uncommitted (lost #12/#14). The commit lands on the worktree
// branch only — main must NOT yet see the change (landing is the caller's job).
func TestSubLoopDoneCommitsWorktree(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()
	fake := model.NewFake(map[string]string{
		"PLAN:":   mustJSON(skill.PlanOutput{}),
		"VERIFY:": mustJSON(skill.VerifyOutput{Passed: true}),
	})
	sl := &SubLoop{
		Repo: repo, Store: st, Budget: budget.New(100000, 1000000, 3),
		Execute:    fileWriteExec{},
		Plan:       mkSkill[skill.PlanInput, skill.PlanOutput]("PLAN:", fake),
		VerifyLLM:  verify.LLM{Skill: mkSkill[skill.VerifyInput, skill.VerifyOutput]("VERIFY:", fake)},
		Tier3Human: true,
		Channel:    channel.NewLocal(t.TempDir()),
	}
	out, err := sl.Run(context.Background(), channel.Task{Ref: "20", Description: "land me", AcceptanceCriteria: []string{"c"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "done" {
		t.Fatalf("want done, got %s (%s)", out.Status, out.Detail)
	}
	if out.Branch == "" || out.Worktree == "" {
		t.Fatalf("done must expose Branch+Worktree for landing, got %+v", out)
	}
	// the worktree's branch holds the auto-land commit
	logOut, _ := execGit(repo, "--no-pager", "log", "--oneline", out.Branch)
	if !strings.Contains(logOut, "loop-eng auto-land") {
		t.Fatalf("branch %s missing auto-land commit: %s", out.Branch, logOut)
	}
	// main must NOT yet contain the new file (landing is the caller's job)
	if _, err := os.Stat(filepath.Join(repo, "landed.txt")); !os.IsNotExist(err) {
		t.Fatalf("main must not see landed.txt before caller lands: %v", err)
	}
	// the committed file is in the worktree
	got, _ := os.ReadFile(filepath.Join(out.Worktree, "landed.txt"))
	if string(got) != "done work" {
		t.Fatalf("worktree file content = %q, want %q", string(got), "done work")
	}
}

// ---- observability tests (spec §8.7) ----

// TestSubLoopPhaseLogsAsserts asserts that each phase (plan/execute/verify)
// emits a [subloop] log line at start and done with the task short ID and phase
// name. Uses an injected logger backed by a bytes.Buffer so assertions are
// against structured output, not stderr scraping.
func TestSubLoopPhaseLogsAsserts(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()
	fake := model.NewFake(map[string]string{
		"PLAN:":    mustJSON(skill.PlanOutput{}),
		"EXECUTE:": "ok",
		"VERIFY:":  mustJSON(skill.VerifyOutput{Passed: true}),
	})
	var buf bytes.Buffer
	sl := &SubLoop{
		Repo: repo, Store: st, Budget: budget.New(100000, 1000000, 3),
		Execute:    fake,
		Plan:       mkSkill[skill.PlanInput, skill.PlanOutput]("PLAN:", fake),
		VerifyLLM:  verify.LLM{Skill: mkSkill[skill.VerifyInput, skill.VerifyOutput]("VERIFY:", fake)},
		Tier3Human: true,
		Channel:    channel.NewLocal(t.TempDir()),
		Log:        log.New(&buf, "", log.Lmsgprefix),
	}

	out, err := sl.Run(context.Background(), channel.Task{Ref: "30", Description: "d", AcceptanceCriteria: []string{"c"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "done" {
		t.Fatalf("want done, got %s (%s)", out.Status, out.Detail)
	}

	logs := buf.String()
	t.Logf("SubLoop logs:\n%s", logs)

	// Each phase must have a start and a done log line.
	for _, phase := range []string{"plan", "execute", "verify"} {
		wantStart := "phase=" + phase + " start"
		if !strings.Contains(logs, wantStart) {
			t.Errorf("phase %q: missing start log (want %q in logs)", phase, wantStart)
		}
		wantDone := "phase=" + phase + " done"
		if !strings.Contains(logs, wantDone) {
			t.Errorf("phase %q: missing done log (want %q in logs)", phase, wantDone)
		}
	}
}

// TestSubLoopRetryLogs asserts retry logs with attempt number, reason, and
// backoff when verify repeatedly fails. The fake verify always rejects; after
// max retries the task blocks.
func TestSubLoopRetryLogs(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()
	fake := model.NewFake(map[string]string{
		"PLAN:":    mustJSON(skill.PlanOutput{}),
		"EXECUTE:": "ok",
		"VERIFY:":  mustJSON(skill.VerifyOutput{Passed: false, Reason: "reject reason here"}),
	})
	var buf bytes.Buffer
	sl := &SubLoop{
		Repo: repo, Store: st, Budget: budget.New(100000, 1000000, 2),
		Execute:    fake,
		Plan:       mkSkill[skill.PlanInput, skill.PlanOutput]("PLAN:", fake),
		VerifyLLM:  verify.LLM{Skill: mkSkill[skill.VerifyInput, skill.VerifyOutput]("VERIFY:", fake)},
		Tier3Human: true,
		Channel:    channel.NewLocal(t.TempDir()),
		Log:        log.New(&buf, "", log.Lmsgprefix),
	}

	out, _ := sl.Run(context.Background(), channel.Task{Ref: "31", Description: "d"})
	if out.Status != "blocked" {
		t.Fatalf("want blocked, got %s", out.Status)
	}

	logs := buf.String()
	t.Logf("Retry logs:\n%s", logs)

	// Must have retry log lines with attempt, reason, backoff.
	if !strings.Contains(logs, "retry") {
		t.Error("expected retry log lines, got none")
	}
	if !strings.Contains(logs, "attempt=") {
		t.Error("retry log missing attempt number")
	}
	if !strings.Contains(logs, "reason=") {
		t.Error("retry log missing reason")
	}
	if !strings.Contains(logs, "backoff=") {
		t.Error("retry log missing backoff")
	}
}

// TestSubLoopInFlightClearedOnDone verifies InFlight is cleared after a done
// outcome (terminal path). The Store's in_flight table must be empty after
// SubLoop.Run returns done.
func TestSubLoopInFlightClearedOnDone(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()
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

	out, err := sl.Run(context.Background(), channel.Task{Ref: "32", Description: "d", AcceptanceCriteria: []string{"c"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "done" {
		t.Fatalf("want done, got %s", out.Status)
	}

	// InFlight must be empty after terminal outcome.
	if _, ok, _ := st.InFlight(); ok {
		t.Fatal("in_flight must be empty after SubLoop.Run returns done")
	}
}

// TestSubLoopInFlightClearedOnBlocked verifies InFlight is cleared after a
// blocked outcome (retries exhausted).
func TestSubLoopInFlightClearedOnBlocked(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()
	fake := model.NewFake(map[string]string{
		"PLAN:":    mustJSON(skill.PlanOutput{}),
		"EXECUTE:": "ok",
		"VERIFY:":  mustJSON(skill.VerifyOutput{Passed: false, Reason: "nope"}),
	})
	sl := &SubLoop{
		Repo: repo, Store: st, Budget: budget.New(100000, 1000000, 2),
		Execute:    fake,
		Plan:       mkSkill[skill.PlanInput, skill.PlanOutput]("PLAN:", fake),
		VerifyLLM:  verify.LLM{Skill: mkSkill[skill.VerifyInput, skill.VerifyOutput]("VERIFY:", fake)},
		Tier3Human: true,
		Channel:    channel.NewLocal(t.TempDir()),
	}

	out, _ := sl.Run(context.Background(), channel.Task{Ref: "33", Description: "d"})
	if out.Status != "blocked" {
		t.Fatalf("want blocked, got %s", out.Status)
	}

	if _, ok, _ := st.InFlight(); ok {
		t.Fatal("in_flight must be empty after SubLoop.Run returns blocked")
	}
}

// TestSubLoopInFlightClearedOnNeedsReview verifies InFlight is cleared after a
// needs-review outcome (tier-3 human review park).
func TestSubLoopInFlightClearedOnNeedsReview(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()
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
		HumanTier:  needsHumanTier{},
		Channel:    channel.NewLocal(t.TempDir()),
	}

	out, err := sl.Run(context.Background(), channel.Task{Ref: "34", Description: "d", AcceptanceCriteria: []string{"c"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "needs-review" {
		t.Fatalf("want needs-review, got %s", out.Status)
	}

	if _, ok, _ := st.InFlight(); ok {
		t.Fatal("in_flight must be empty after SubLoop.Run returns needs-review")
	}
}

// TestCooperativeCancel 钉死协作式 cancel（spec §4.5/§7）：一个跑起来的 SubLoop
// 必须在每个 phase 边界自查 Store.CancelRequested，命中则提前以 cancelled 收尾
// （协作式，非硬杀）。测试包住 Plan skill 的 model.Client：第一次 plan 调用返回
// 后插入一条 pending cancel 命令，使 execute 前的自查命中并 bail。defer EndRun
// （Task 2）用 out.Status=cancelled 自动收尾，InFlight 必须清空。
func TestCooperativeCancel(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()
	fake := model.NewFake(map[string]string{
		"PLAN:":    mustJSON(skill.PlanOutput{}),
		"EXECUTE:": "ok",
		"VERIFY:":  mustJSON(skill.VerifyOutput{Passed: true}),
	})
	task := channel.Task{Ref: "50", Description: "d", AcceptanceCriteria: []string{"c"}}
	taskID, err := st.InsertTask(state.TaskRow{
		IssueRef: task.Ref, Description: task.Description,
		TaskType: task.TaskType, Source: "run-once", Criteria: task.AcceptanceCriteria,
	})
	if err != nil {
		t.Fatal(err)
	}
	sl := &SubLoop{
		Repo: repo, Store: st, Budget: budget.New(100000, 1000000, 3),
		Execute: fake,
		// Plan 的 model.Client 被包一层：第一次 plan 调用后插入 cancel 命令，
		// execute 前的自查即会命中。
		Plan:       mkSkill[skill.PlanInput, skill.PlanOutput]("PLAN:", &cancelOnFirstCall{inner: fake, store: st, taskID: taskID, t: t}),
		VerifyLLM:  verify.LLM{Skill: mkSkill[skill.VerifyInput, skill.VerifyOutput]("VERIFY:", fake)},
		Tier3Human: true,
		Channel:    channel.NewLocal(t.TempDir()),
	}
	sl.PreinsertedTaskID = taskID

	out, err := sl.Run(context.Background(), task)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Status != "cancelled" {
		t.Fatalf("status=%q want cancelled (detail=%s)", out.Status, out.Detail)
	}
	// 终态后 InFlight 必须清空
	if _, ok, _ := st.InFlight(); ok {
		t.Fatal("in_flight must be empty after cooperative cancel")
	}
	// run 关闭为 cancelled（defer EndRun 用 out.Status 收尾）
	runs, err := st.RunsOfTask(taskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Outcome != "cancelled" {
		t.Fatalf("runs=%+v want exactly 1 run with outcome=cancelled", runs)
	}
}

// cancelOnFirstCall 包住一个 model.Client：第一次 Call 在委托给内部 client 后
// 往 store 插一条 pending cancel 命令，使 SubLoop 下一个 phase 边界自查
// （Store.CancelRequested）bail 到 cancelled。仅用于测试，模拟 TUI 中途下发 cancel。
type cancelOnFirstCall struct {
	inner  model.Client
	store  *state.Store
	taskID string
	done   bool
	t      *testing.T
}

func (c *cancelOnFirstCall) Call(ctx context.Context, prompt string) (string, model.Usage, error) {
	out, u, err := c.inner.Call(ctx, prompt)
	if !c.done {
		c.done = true
		if e := c.store.InsertCommand(c.taskID, "cancel", ""); e != nil {
			c.t.Fatal(e)
		}
	}
	return out, u, err
}

// TestReplayNoInterleaveAfterResume 钉死 spec §4.1 的 bug：同任务两次 run，各自
// attempt=1 的 plan step seq 都是 11。修前 AppendStep 的 RunID 传的是 taskID 且无
// StartRun ⇒ RunsOfTask 空、两次 run 的 step 全压在同一个 taskID 下、Replay 交错；
// 修后每次 Run 用 StartRun 拿独立 runID 透传，每个 run 的 Replay 只剩自己那条 plan
// step（seq=11 至多一条）。构造照搬 TestSubLoopDoneOnFirstPass 的 passing SubLoop。
func TestReplayNoInterleaveAfterResume(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()
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
	task := channel.Task{Ref: "40", Description: "d", AcceptanceCriteria: []string{"c"}}
	taskID, err := st.InsertTask(state.TaskRow{
		IssueRef: task.Ref, Description: task.Description,
		TaskType: task.TaskType, Source: "run-once", Criteria: task.AcceptanceCriteria,
	})
	if err != nil {
		t.Fatal(err)
	}
	sl.PreinsertedTaskID = taskID // 两次 Run 复用同一 taskID（模拟 resume 后再 dispatch）
	ctx := context.Background()

	// run 1（first pass → done）
	out1, err := sl.Run(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	if out1.Status != "done" {
		t.Fatalf("run 1: want done, got %s (%s)", out1.Status, out1.Detail)
	}
	// done 保留 worktree + 分支 loop/<taskID>-r1；丢弃后 run 2 才能重建同名 worktree。
	if out1.Worktree != "" {
		if derr := isolation.Discard(sl.Repo, out1.Worktree); derr != nil {
			t.Fatalf("discard run-1 worktree: %v", derr)
		}
	}

	// run 2（模拟 resume 后再次 dispatch 同一 task）
	if _, err := sl.Run(ctx, task); err != nil {
		t.Fatal(err)
	}

	runs, err := st.RunsOfTask(taskID)
	if err != nil || len(runs) != 2 {
		t.Fatalf("want 2 runs, got %d (err %v)", len(runs), err)
	}
	for _, r := range runs {
		steps, err := st.Replay(r.ID)
		if err != nil {
			t.Fatalf("Replay(%s): %v", r.ID, err)
		}
		// 每个 run 的 plan step（seq=11）至多一条；两次 run 不交错 ⇒ 不应出现两条 seq=11。
		var planSeq11 int
		for _, s := range steps {
			if s.Seq == 11 && s.Role == "plan" {
				planSeq11++
			}
		}
		if planSeq11 > 1 {
			t.Fatalf("run %s: got %d plan seq=11 steps (interleaved), want ≤1", r.ID, planSeq11)
		}
	}
}

// ---- reopen/反馈：每次运行都把 issue 评论喂给 plan（修 reopen 反馈丢失 bug）----

// fakeCommentChan：channel.Channel 桩，ListReplies 返回预设评论。local 通道不返回
// 评论，故用它证明 SubLoop 把评论收集进 plan 的 BattleReport。
type fakeCommentChan struct{ replies map[string][]channel.Reply }

func (f *fakeCommentChan) ListNewTasks(context.Context) ([]channel.Task, error) { return nil, nil }
func (f *fakeCommentChan) ListReplies(_ context.Context, _ []string, _ time.Time) (map[string][]channel.Reply, error) {
	return f.replies, nil
}
func (f *fakeCommentChan) PostComment(context.Context, string, string) error  { return nil }
func (f *fakeCommentChan) UpdateStatus(context.Context, string, string) error { return nil }
func (f *fakeCommentChan) CloseIssue(context.Context, string) error           { return nil }
func (f *fakeCommentChan) GetTaskStates(context.Context, []string) (map[string]channel.TaskState, error) {
	return nil, nil
}

// recorder：包一层 model.Client，记录每次被调用的 prompt，以便断言 plan 真收到了什么。
type recorder struct {
	model.Client
	got []string
}

func (r *recorder) Call(ctx context.Context, prompt string) (string, model.Usage, error) {
	r.got = append(r.got, prompt)
	return r.Client.Call(ctx, prompt)
}

// TestCollectIssueComments：收集器把 ref 的全部评论拉下来拼接（trim 首尾空白）；nil channel → ""。
func TestCollectIssueComments(t *testing.T) {
	sl := &SubLoop{Channel: &fakeCommentChan{replies: map[string][]channel.Reply{
		"42": {{Body: "第一轮反馈"}, {Body: "  第二轮反馈  "}},
	}}}
	got := sl.collectIssueComments(context.Background(), "42")
	for _, want := range []string{"第一轮反馈", "第二轮反馈"} {
		if !strings.Contains(got, want) {
			t.Fatalf("评论 %q 丢失: %q", want, got)
		}
	}
	if (&SubLoop{}).collectIssueComments(context.Background(), "x") != "" {
		t.Fatal("nil channel 应返回空")
	}
}

// TestSubLoopFeedsIssueCommentsToPlan：reopen 后人在 issue 写的反馈，plan 必须能看到
// —— 核心修复：之前 reopen 走 reconcile 只翻状态、不读评论，反馈丢失，plan 盲跑。
func TestSubLoopFeedsIssueCommentsToPlan(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()

	fake := model.NewFake(map[string]string{
		"PLAN:":    mustJSON(skill.PlanOutput{}),
		"EXECUTE:": "ok",
		"VERIFY:":  mustJSON(skill.VerifyOutput{Passed: true}),
	})
	rec := &recorder{Client: fake}

	ch := &fakeCommentChan{replies: map[string][]channel.Reply{
		"77": {{Body: "USER_FEEDBACK: plan.md 例子是瞎讲，脚本要按 plan 实现个性化"}},
	}}

	sl := &SubLoop{
		Repo: repo, Store: st, Budget: budget.New(100000, 1000000, 3),
		Execute: fake,
		Plan: skill.Skill[skill.PlanInput, skill.PlanOutput]{
			Name:       "plan",
			PromptTmpl: "PLAN:\n战报: {{.BattleReport}}\n任务: {{.Task}}",
			ParseJSON: func(b []byte) (skill.PlanOutput, error) {
				var o skill.PlanOutput
				return o, json.Unmarshal(b, &o)
			},
			Model: rec,
		},
		VerifyLLM:  verify.LLM{Skill: mkSkill[skill.VerifyInput, skill.VerifyOutput]("VERIFY:", fake)},
		Tier3Human: true,
		Channel:    ch,
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
	if !strings.Contains(rec.got[0], "USER_FEEDBACK: plan.md 例子是瞎讲") {
		t.Fatalf("plan prompt 缺失 issue 评论（reopen 反馈丢失）:\n%s", rec.got[0])
	}
}

// echoExec 写一个文件（保证非空 diff）并返回一个可识别的输出串，用于断言
// execute 的模型输出被落进了 trace（而非被 `_ = execOut` 丢弃）。
type echoExec struct{ out, body string }

func (e echoExec) Exec(_ context.Context, dir, _ string) (string, model.Usage, error) {
	if err := os.WriteFile(filepath.Join(dir, "landed.txt"), []byte(e.body), 0644); err != nil {
		return "", model.Usage{}, err
	}
	return e.out, model.Usage{}, nil
}

// TestSubLoopCapturesExecuteOutput: execute 的模型输出（execOut）+ prompt 必须落进
// trace。之前 execOut 被丢弃，导致「空 diff」时无从诊断 claude 到底返回了啥。
func TestSubLoopCapturesExecuteOutput(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()
	fake := model.NewFake(map[string]string{
		"PLAN:":   mustJSON(skill.PlanOutput{}),
		"VERIFY:": mustJSON(skill.VerifyOutput{Passed: true}),
	})
	sl := &SubLoop{
		Repo: repo, Store: st, Budget: budget.New(100000, 1000000, 3),
		Execute:    echoExec{out: "MODEL_ECHO_42", body: "done work"},
		Plan:       mkSkill[skill.PlanInput, skill.PlanOutput]("PLAN:", fake),
		VerifyLLM:  verify.LLM{Skill: mkSkill[skill.VerifyInput, skill.VerifyOutput]("VERIFY:", fake)},
		Tier3Human: true,
		Channel:    channel.NewLocal(t.TempDir()),
	}
	out, err := sl.Run(context.Background(), channel.Task{Ref: "1", Description: "d", AcceptanceCriteria: []string{"c"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "done" {
		t.Fatalf("want done, got %s (%s)", out.Status, out.Detail)
	}
	statuses, _ := st.ListStatuses()
	runs, _ := st.RunsOfTask(statuses[0].ID)
	steps, _ := st.Replay(runs[0].ID)
	var execStep state.StepRow
	found := false
	for _, s := range steps {
		if s.Role == "execute" {
			execStep, found = s, true
			break
		}
	}
	if !found {
		t.Fatal("no execute step recorded")
	}
	if !strings.Contains(execStep.OutputJSON, "MODEL_ECHO_42") {
		t.Fatalf("execute OutputJSON 缺失模型输出（execOut 被丢弃了？）: %s", execStep.OutputJSON)
	}
	if !strings.Contains(execStep.OutputJSON, "landed.txt") {
		t.Fatalf("execute OutputJSON 缺失 diff: %s", execStep.OutputJSON)
	}
	if !strings.Contains(execStep.InputJSON, "EXECUTE:") {
		t.Fatalf("execute InputJSON 缺失 prompt: %s", execStep.InputJSON)
	}
}

// captureExec 记录 execute 收到的 prompt（不写文件 → 空 diff），用于断言 issue 评论
// 也喂给了 execute（不只是 plan）。
type captureExec struct{ got string }

func (c *captureExec) Exec(_ context.Context, _ string, prompt string) (string, model.Usage, error) {
	c.got = prompt
	return "ok", model.Usage{}, nil
}

// TestSubLoopFeedsIssueCommentsToExecute: issue 评论必须喂给 EXECUTE，否则 execute 只
// 对照验收标准、看不见反馈，对「已实现但需按反馈精修」的任务反复空 diff（#20 即此）。
func TestSubLoopFeedsIssueCommentsToExecute(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()
	fake := model.NewFake(map[string]string{
		"PLAN:":   mustJSON(skill.PlanOutput{}),
		"VERIFY:": mustJSON(skill.VerifyOutput{Passed: true}),
	})
	exec := &captureExec{}
	sl := &SubLoop{
		Repo: repo, Store: st, Budget: budget.New(100000, 1000000, 3),
		Execute:    exec,
		Plan:       mkSkill[skill.PlanInput, skill.PlanOutput]("PLAN:", fake),
		VerifyLLM:  verify.LLM{Skill: mkSkill[skill.VerifyInput, skill.VerifyOutput]("VERIFY:", fake)},
		Tier3Human: true,
		Channel: &fakeCommentChan{replies: map[string][]channel.Reply{
			"9": {{Body: "EXEC_FEEDBACK: plan.md 例子要按实现个性化"}},
		}},
	}
	out, err := sl.Run(context.Background(), channel.Task{Ref: "9", Description: "d", AcceptanceCriteria: []string{"c"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "done" {
		t.Fatalf("want done, got %s (%s)", out.Status, out.Detail)
	}
	if !strings.Contains(exec.got, "EXEC_FEEDBACK: plan.md 例子要按实现个性化") {
		t.Fatalf("execute prompt 缺失 issue 评论（反馈只给了 plan 没给 execute）:\n%s", exec.got)
	}
}

// TestVerifyFailComment：verify 驳回评论正文必须同时含「失败理由」+「改进建议」，
// 且签名 (channel.Task, int, verify.VerifyResult) 的调用与定义对齐（修历轮编译失败）。
// 三条分支：FailingCriteria 非空 → 针对性修正；空 → 回退验收标准；皆空 → 兜底建议。
func TestVerifyFailComment(t *testing.T) {
	task := channel.Task{Ref: "5", AcceptanceCriteria: []string{"通过 go test", "diff 非空"}}

	// 1) FailingCriteria 非空：理由来自 Detail，建议逐条来自未满足标准。
	got := verifyFailComment(task, 2, verify.VerifyResult{
		Passed: false, Detail: "tier-2: tests do not cover X",
		FailingCriteria: []string{"通过 go test", "覆盖边界"},
	})
	for _, want := range []string{"VERIFY-FAIL", "第 2 轮", "失败理由", "tier-2: tests do not cover X",
		"改进建议", "针对未满足标准修正：通过 go test", "针对未满足标准修正：覆盖边界"} {
		if !strings.Contains(got, want) {
			t.Fatalf("FailingCriteria 分支缺失 %q:\n%s", want, got)
		}
	}

	// 2) FailingCriteria 空、验收标准非空（tier-1 这类驳回）：建议回退到验收标准复核。
	got = verifyFailComment(task, 1, verify.VerifyResult{Passed: false, Detail: "go-test: exit 1"})
	for _, want := range []string{"失败理由", "go-test: exit 1", "改进建议",
		"复核验收标准是否满足：通过 go test", "复核验收标准是否满足：diff 非空"} {
		if !strings.Contains(got, want) {
			t.Fatalf("验收标准回退分支缺失 %q:\n%s", want, got)
		}
	}

	// 3) 皆空（无标准、无 FailingCriteria、Detail 也空）：理由兜底 + 兜底建议，不留空区。
	got = verifyFailComment(channel.Task{Ref: "6"}, 3, verify.VerifyResult{Passed: false})
	for _, want := range []string{"失败理由", "verify 驳回但未给出理由", "改进建议", "对照上面的失败理由"} {
		if !strings.Contains(got, want) {
			t.Fatalf("兜底分支缺失 %q:\n%s", want, got)
		}
	}
}

// TestSubLoopPostsVerifyFailCommentOnReject：verify 不过时 SubLoop 必须把「失败理由 +
// 改进建议」作为评论发到 issue（outbox/<ref>.md）。落笔后持久可见、下一轮可读回。
func TestSubLoopPostsVerifyFailCommentOnReject(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()
	fake := model.NewFake(map[string]string{
		"PLAN:":    mustJSON(skill.PlanOutput{}),
		"EXECUTE:": "ok",
		"VERIFY:":  mustJSON(skill.VerifyOutput{Passed: false, Reason: "nope", FailingCriteria: []string{"criterion-A"}}), // 永远不过
	})
	root := t.TempDir()
	sl := &SubLoop{
		Repo: repo, Store: st, Budget: budget.New(100000, 1000000, 2),
		Execute:    fake,
		Plan:       mkSkill[skill.PlanInput, skill.PlanOutput]("PLAN:", fake),
		VerifyLLM:  verify.LLM{Skill: mkSkill[skill.VerifyInput, skill.VerifyOutput]("VERIFY:", fake)},
		Tier3Human: true,
		Channel:    channel.NewLocal(root),
	}
	out, _ := sl.Run(context.Background(), channel.Task{Ref: "7", Description: "d", AcceptanceCriteria: []string{"criterion-A"}})
	if out.Status != "blocked" {
		t.Fatalf("want blocked, got %s", out.Status)
	}
	body, err := os.ReadFile(filepath.Join(root, "outbox", "7.md"))
	if err != nil {
		t.Fatalf("outbox/7.md 未写入（verify-fail 评论未发）: %v", err)
	}
	// 每轮驳回各发一条 VERIFY-FAIL（maxRetries=2 → 2 条），含理由 + 建议。
	got := string(body)
	for _, want := range []string{"VERIFY-FAIL", "失败理由", "nope", "改进建议", "针对未满足标准修正：criterion-A"} {
		if !strings.Contains(got, want) {
			t.Fatalf("verify-fail 评论缺失 %q:\n%s", want, got)
		}
	}
	if c := strings.Count(got, "VERIFY-FAIL"); c != 2 {
		t.Fatalf("想见 2 条 verify-fail 评论（每轮一条），实际 %d 条", c)
	}
}

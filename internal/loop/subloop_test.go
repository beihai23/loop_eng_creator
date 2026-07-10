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
	// Run inserts the task itself; recover its id to scope the ledger read.
	statuses, err := st.ListStatuses()
	if err != nil || len(statuses) != 1 {
		t.Fatalf("want exactly 1 task status, got %d (err %v)", len(statuses), err)
	}
	rows, err := st.BudgetLedger(statuses[0].ID)
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

// TestSubLoopWritesStatusBlocked: tier1 always-fail → blocked, and the blocked
// outcome must mark the ticket status — for Local, status/<ref> == "blocked".
func TestSubLoopWritesStatusBlocked(t *testing.T) {
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
		Repo: repo, Store: st, Budget: budget.New(100000, 1000000, 2),
		Execute:             fake,
		Plan:                mkSkill[skill.PlanInput, skill.PlanOutput]("PLAN:", fake),
		VerifyDeterministic: []verify.Deterministic{{Label: "go-test", Cmd: []string{"false"}}}, // tier1 永失败
		VerifyLLM:           verify.LLM{Skill: mkSkill[skill.VerifyInput, skill.VerifyOutput]("VERIFY:", fake)},
		Tier3Human:          true,
		Channel:             channel.NewLocal(root),
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
		Execute:   fileWriteExec{},
		Plan:      mkSkill[skill.PlanInput, skill.PlanOutput]("PLAN:", fake),
		VerifyLLM: verify.LLM{Skill: mkSkill[skill.VerifyInput, skill.VerifyOutput]("VERIFY:", fake)},
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
		Execute:  fake,
		Plan:     mkSkill[skill.PlanInput, skill.PlanOutput]("PLAN:", fake),
		VerifyLLM: verify.LLM{Skill: mkSkill[skill.VerifyInput, skill.VerifyOutput]("VERIFY:", fake)},
		Tier3Human: true,
		Channel:   channel.NewLocal(t.TempDir()),
		Log:       log.New(&buf, "", log.Lmsgprefix),
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
		Execute:  fake,
		Plan:     mkSkill[skill.PlanInput, skill.PlanOutput]("PLAN:", fake),
		VerifyLLM: verify.LLM{Skill: mkSkill[skill.VerifyInput, skill.VerifyOutput]("VERIFY:", fake)},
		Tier3Human: true,
		Channel:   channel.NewLocal(t.TempDir()),
		Log:       log.New(&buf, "", log.Lmsgprefix),
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

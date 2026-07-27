package cli

import (
	"context"
	"testing"

	"loop-eng/internal/budget"
	"loop-eng/internal/config"
	"loop-eng/internal/state"
)

// TestTriageAccountsTokens 钉死 triage 记账（spec §8.8）：daemon 派发点的 runTriage 把
// triage skill 的 Model 经 budget.Client 装饰（按次新鲜 Enforcer，不复用守护级），token 进
// steps(role=triage,tokens_in/out) 且 budget_ledger 有 tokens 行，挂在 triage 自己的 run 上
// （outcome=triage，与 task run 的 done/blocked 分离、不影响 SubLoop 的 run 计数）。
func TestTriageAccountsTokens(t *testing.T) {
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()
	cfg := &config.Config{
		Budget: config.Budget{PerCallTokens: 100000, PerTaskTokens: 1000000, MaxRetries: 3},
	}
	// buildModels(fake) 返回裸 triage skill（Model 未装饰），正是 runTriage 的输入契约。
	_, _, _, triageSkill, _ := buildModels(cfg, "fake", budget.New(100000, 1000000, 3))

	tid, err := st.InsertTask(state.TaskRow{IssueRef: "T", Description: "triage 任务"})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	out, err := runTriage(context.Background(), st, triageSkill, cfg, state.TaskRow{
		ID: tid, IssueRef: "T", Description: "triage 任务",
	})
	if err != nil {
		t.Fatalf("runTriage: %v (out=%+v)", err, out)
	}

	// triage 开了自己的 run（独立于 SubLoop 的 task run）。
	runs, err := st.RunsOfTask(tid)
	if err != nil || len(runs) != 1 {
		t.Fatalf("want 1 triage run, got %d (err %v)", len(runs), err)
	}
	if runs[0].Outcome != "triage" {
		t.Fatalf("triage run outcome 应为 triage, got %q", runs[0].Outcome)
	}

	// 落了一行 role=triage step 且带真实 token。
	steps, err := st.Replay(runs[0].ID)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	var triTokensIn int
	var hasTriageStep bool
	for _, s := range steps {
		if s.Role == "triage" {
			hasTriageStep = true
			triTokensIn = s.TokensIn
		}
	}
	if !hasTriageStep {
		t.Fatalf("缺 role=triage step: %+v", steps)
	}
	if triTokensIn == 0 {
		t.Fatalf("triage step tokens_in 应 >0, got 0")
	}

	// budget_ledger 有 tokens 行。
	rows, err := st.BudgetLedger(runs[0].ID)
	if err != nil {
		t.Fatalf("budget ledger: %v", err)
	}
	var hasTokens bool
	for _, r := range rows {
		if r.Kind == "tokens" {
			hasTokens = true
		}
	}
	if !hasTokens {
		t.Fatalf("budget_ledger 缺 triage 的 tokens 行: %+v", rows)
	}
}

// TestTriageBudgetGateBlocksCall 钉死 triage 闸门（spec §8.8）：紧预算下（PerCall 顶过
// PerTask）BeforeCall 在模型运行前拒付——triage 返回 error、不被静默放行。
func TestTriageBudgetGateBlocksCall(t *testing.T) {
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()
	// PerCall(1000) > PerTask(50)：BeforeCall 因 spent+PerCall > PerTask 拒付。
	cfg := &config.Config{
		Budget: config.Budget{PerCallTokens: 1000, PerTaskTokens: 50, MaxRetries: 3},
	}
	_, _, _, triageSkill, _ := buildModels(cfg, "fake", budget.New(1000, 50, 3))

	tid, err := st.InsertTask(state.TaskRow{IssueRef: "G", Description: "紧预算 triage"})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	out, err := runTriage(context.Background(), st, triageSkill, cfg, state.TaskRow{
		ID: tid, IssueRef: "G", Description: "紧预算 triage",
	})
	if err == nil {
		t.Fatalf("紧预算下 triage 应被闸住返回 error, got out=%+v", out)
	}
	// 被闸的 triage run outcome=error，且仍落了 role=triage step（status=fail）。
	runs, _ := st.RunsOfTask(tid)
	if len(runs) != 1 || runs[0].Outcome != "error" {
		t.Fatalf("want 1 triage run outcome=error, got %+v", runs)
	}
	steps, _ := st.Replay(runs[0].ID)
	var hasFailStep bool
	for _, s := range steps {
		if s.Role == "triage" && s.Status == "fail" {
			hasFailStep = true
		}
	}
	if !hasFailStep {
		t.Fatalf("紧预算 triage 应落 status=fail 的 triage step: %+v", steps)
	}
}

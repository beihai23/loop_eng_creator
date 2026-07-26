package loop

import (
	"context"
	"encoding/json"
	"testing"

	"loop-eng/internal/budget"
	"loop-eng/internal/channel"
	"loop-eng/internal/model"
	"loop-eng/internal/skill"
	"loop-eng/internal/state"
)

// tier1CountingHelpClient 是 help 模型桩：计数 Call 并回合法 HelpOutput JSON。
type tier1CountingHelpClient struct{ calls int }

func (c *tier1CountingHelpClient) Call(_ context.Context, _ string) (string, model.Usage, error) {
	c.calls++
	return mustJSON(skill.HelpOutput{}), model.Usage{TokensIn: 7, TokensOut: 9}, nil
}

func tier1ParseHelp(b []byte) (skill.HelpOutput, error) {
	var o skill.HelpOutput
	return o, json.Unmarshal(b, &o)
}

// TestTier1HelpBudget 钉死本任务给 help 角色加的 §8.8 契约：
//   - 记账：help 模型调用经 budget.Client，token 进 steps.tokens_in/out 且 budget_ledger 有 tokens 行；
//   - 闸门：per-task 预算容不下时 BeforeCall 在模型运行前拒付（base 未被调用）、help 优雅回退。
func TestTier1HelpBudget(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()

	// (1) 记账——宽裕预算，help 真跑并被落账。
	tid, err := st.InsertTask(state.TaskRow{IssueRef: "H", Description: "h"})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	runID, err := st.StartRun(tid)
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	base := &tier1CountingHelpClient{}
	bz := budget.New(100000, 1000000, 3)
	sl := &SubLoop{
		Repo: repo, Store: st, Budget: bz, Channel: channel.NewLocal(t.TempDir()),
		Help: skill.Skill[skill.HelpInput, skill.HelpOutput]{
			Name: "help", PromptTmpl: "HELP: {{.Task}}", ParseJSON: tier1ParseHelp,
			Model: &budget.Client{Base: base, Enf: bz},
		},
	}
	_ = sl.helpOutput(context.Background(), channel.Task{Ref: "H", Description: "h"}, 2, "卡住", runID)
	if base.calls != 1 {
		t.Fatalf("accounting: help base 应被调用 1 次, got %d", base.calls)
	}
	var helpIn int
	for _, s := range mustReplay(t, st, runID) {
		if s.Role == "help" {
			helpIn = s.TokensIn
		}
	}
	if helpIn == 0 {
		t.Fatalf("accounting: 想要 help step tokens_in>0, got 0")
	}
	if !ledgerHasTokens(t, st, runID) {
		t.Fatalf("accounting: budget_ledger 缺 help 的 tokens 行")
	}

	// (2) 闸门——PerCall(100) 会把 spent 顶过 PerTask(50)：BeforeCall 拒付。
	tid2, _ := st.InsertTask(state.TaskRow{IssueRef: "H2", Description: "h2"})
	runID2, _ := st.StartRun(tid2)
	gated := &tier1CountingHelpClient{}
	bzGate := budget.New(100, 50, 3)
	sl2 := &SubLoop{
		Repo: repo, Store: st, Budget: bzGate, Channel: channel.NewLocal(t.TempDir()),
		Help: skill.Skill[skill.HelpInput, skill.HelpOutput]{
			Name: "help", PromptTmpl: "HELP: {{.Task}}", ParseJSON: tier1ParseHelp,
			Model: &budget.Client{Base: gated, Enf: bzGate},
		},
	}
	_ = sl2.helpOutput(context.Background(), channel.Task{Ref: "H2", Description: "h2"}, 2, "卡住", runID2)
	if gated.calls != 0 {
		t.Fatalf("gate: help 模型必须被闸住(不调用), got %d calls", gated.calls)
	}
}

func mustReplay(t *testing.T, st *state.Store, runID string) []state.StepRow {
	t.Helper()
	steps, err := st.Replay(runID)
	if err != nil {
		t.Fatalf("replay %s: %v", runID, err)
	}
	return steps
}

func ledgerHasTokens(t *testing.T, st *state.Store, runID string) bool {
	t.Helper()
	rows, err := st.BudgetLedger(runID)
	if err != nil {
		t.Fatalf("budget ledger %s: %v", runID, err)
	}
	for _, r := range rows {
		if r.Kind == "tokens" {
			return true
		}
	}
	return false
}

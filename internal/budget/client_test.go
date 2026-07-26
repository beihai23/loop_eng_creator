package budget

import (
	"context"
	"errors"
	"testing"

	"loop-eng/internal/model"
)

// recordingClient is a model.Client stub that flips `called` when Call runs,
// so tests can prove the decorator does NOT invoke Base when its pre-check
// rejects.
type recordingClient struct {
	called bool
}

func (r *recordingClient) Call(_ context.Context, _ string) (string, model.Usage, error) {
	r.called = true
	return "x", model.Usage{TokensIn: 1, TokensOut: 1}, nil
}

// TestClientDecoratorBudgetsEveryCall proves the budget.Client decorator
// accrues every successful Call into Enforcer.spent (via Record) and primes the
// next Estimate from real usage — closing the spec §8.8 gap where verify/triage/
// help used to bypass the budget. The estimate BeforeCall checks now tracks the
// role's last real usage × safety (first call uses the role floor), NOT a
// constant, so it can actually exceed PerCall.
func TestClientDecoratorBudgetsEveryCall(t *testing.T) {
	base := model.NewFake(map[string]string{
		"ping": "pong", // usage = TokensIn:len("ping"), TokensOut:len("pong")
	})
	enf := New(100000, 1000000, 3) // PerCall admits the verify floor (20000)
	c := &Client{Base: base, Enf: enf, Role: "verify"}

	before := enf.Spent()
	if before != 0 {
		t.Fatalf("fresh enforcer spent=%d, want 0", before)
	}

	out1, u1, err := c.Call(context.Background(), "ping")
	if err != nil || out1 != "pong" {
		t.Fatalf("first call: out=%q usage=%+v err=%v", out1, u1, err)
	}
	mid := enf.Spent()
	want1 := u1.TokensIn + u1.TokensOut
	if want1 == 0 {
		t.Fatalf("fake returned zero usage — test cannot assert accrual")
	}
	if mid != want1 {
		t.Fatalf("after call 1: spent=%d want=%d", mid, want1)
	}
	// Estimate now tracks real usage: after call 1 the verify estimate is the
	// observed usage × safety (not a constant), so the brake reflects reality.
	if got, want := enf.Estimate("verify"), want1*estimateSafety; got != want {
		t.Fatalf("after call 1: Estimate(verify)=%d want=%d (real %d × safety %d)", got, want, want1, estimateSafety)
	}

	out2, u2, err := c.Call(context.Background(), "ping")
	if err != nil || out2 != "pong" {
		t.Fatalf("second call: %v %v", out2, err)
	}
	after := enf.Spent()
	want2 := u2.TokensIn + u2.TokensOut
	if after != mid+want2 {
		t.Fatalf("after call 2: spent=%d want=%d (mid=%d + u2=%d)", after, mid+want2, mid, want2)
	}
}

// TestClientDecoratorBlocksOnBudgetExceeded proves the decorator's Estimate
// tracks real usage: after a role's recorded usage pushes its next estimate
// past PerCall, the Call is rejected with ErrPerCall WITHOUT invoking Base.
// This replaces the old "estimate == PerCall" tautology (an estimate that
// equaled the cap could never exceed it) — the estimate now reflects reality.
func TestClientDecoratorBlocksOnBudgetExceeded(t *testing.T) {
	base := &recordingClient{}
	enf := New(100000, 1_000_000_000, 3)
	// Prime the role's last usage past PerCall so the next Estimate (× safety)
	// exceeds the cap — the estimate now tracks REAL usage, not a constant.
	enf.Record("verify", model.Usage{TokensIn: 150000, TokensOut: 50000}) // 200000 → est 400000
	c := &Client{Base: base, Enf: enf, Role: "verify"}

	_, _, err := c.Call(context.Background(), "ping")
	if !errors.Is(err, ErrPerCall) {
		t.Fatalf("want ErrPerCall when estimate (%d) exceeds PerCall, got %v", enf.Estimate("verify"), err)
	}
	if base.called {
		t.Fatal("Base must NOT be called when the pre-check rejects")
	}
}

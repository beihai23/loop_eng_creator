package budget

import (
	"context"
	"errors"
	"testing"

	"loop-eng/internal/model"
)

// TestClientDecoratorBudgetsEveryCall proves the budget.Client decorator
// closes the Task 12 verify-budget gap: every Call passes through
// BeforeCall + AfterCall and accrues into Enforcer.spent, so a verify Call
// can no longer silently bypass the budget (spec §8.8).
func TestClientDecoratorBudgetsEveryCall(t *testing.T) {
	base := model.NewFake(map[string]string{
		"ping": "pong", // usage = TokensIn:len("ping"), TokensOut:len("pong")
	})
	enf := New(1000, 10000, 3)
	c := &Client{Base: base, Enf: enf}

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

// TestClientDecoratorBlocksOnBudgetExceeded proves the decorator enforces
// BeforeCall: when the conservative PerCall estimate would blow the per-task
// cap, the Call is rejected WITHOUT invoking Base.
func TestClientDecoratorBlocksOnBudgetExceeded(t *testing.T) {
	base := model.NewFake(map[string]string{"ping": "pong"})
	// PerCall(1000) estimate vs PerTask(100): PerCall > PerTask gap ⇒ first
	// BeforeCall fails because spent+PerCall(1000) > PerTask(100).
	enf := New(1000, 100, 3)
	c := &Client{Base: base, Enf: enf}

	_, _, err := c.Call(context.Background(), "ping")
	if !errors.Is(err, ErrPerTask) {
		t.Fatalf("want ErrPerTask from decorator pre-check, got %v", err)
	}
	if enf.Spent() != 0 {
		t.Fatalf("rejected call must not accrue: spent=%d", enf.Spent())
	}
}

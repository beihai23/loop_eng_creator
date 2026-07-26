package budget

import (
	"errors"
	"testing"

	"loop-eng/internal/model"
)

func TestPerCallCapSemantics(t *testing.T) {
	e := New(100, 1000, 3)
	// 单次调用估算超过 per_call 即拒
	if err := e.BeforeCall(150); !errors.Is(err, ErrPerCall) {
		t.Fatalf("want ErrPerCall, got %v", err)
	}
}

func TestPerTaskCap(t *testing.T) {
	e := New(1000, 100, 3)
	_ = e.AfterCall(model.Usage{TokensIn: 60, TokensOut: 30}) // 90 spent
	if err := e.BeforeCall(50); !errors.Is(err, ErrPerTask) {
		t.Fatalf("want ErrPerTask (90+50>100), got %v", err)
	}
}

func TestRetry(t *testing.T) {
	e := New(1000, 1000, 3)
	if !e.ShouldRetry(1) || !e.ShouldRetry(3) || e.ShouldRetry(4) {
		t.Fatal("retry boundary wrong")
	}
}

// TestHeadroomReservation pins the #86 fix: SubLoop calls BeforeCall(PerCall) so
// the per-task pre-check reserves a full PerCall of headroom under PerTask —
// tripping BEFORE a call that would overshoot, not after. With the old fixed
// estimate (1000), this gate only fired once cumulative spend had already blown
// past the cap (the #86 incident: spent=266844, per_task=200000).
func TestHeadroomReservation(t *testing.T) {
	const perCall, perTask = 20000, 200000
	e := New(perCall, perTask, 3)
	// Spend up to exactly the headroom line: spent+PerCall == PerTask still passes.
	_ = e.AfterCall(model.Usage{TokensOut: perTask - perCall}) // spent = 180000
	if err := e.BeforeCall(perCall); err != nil {
		t.Fatalf("spent+PerCall==PerTask should pass: %v", err)
	}
	// One more token of spend → spent+PerCall > PerTask → reject BEFORE the call.
	_ = e.AfterCall(model.Usage{TokensOut: 1}) // spent = 180001
	if err := e.BeforeCall(perCall); !errors.Is(err, ErrPerTask) {
		t.Fatalf("want ErrPerTask once spent+PerCall>PerTask, got %v", err)
	}
}

// TestAfterCallEnforcesPerCall pins the #98 post-call contract: AfterCall now
// returns an error — a single call whose real usage (TokensIn+TokensOut)
// strictly exceeds PerCall is a violation. ==PerCall is NOT a violation (strict
// >), matching BeforeCall's estimate>PerCall boundary. spent still accumulates.
func TestAfterCallEnforcesPerCall(t *testing.T) {
	e := New(100, 1000, 3)
	// 用量 == PerCall（100）→ 不违规
	if err := e.AfterCall(model.Usage{TokensIn: 60, TokensOut: 40}); err != nil {
		t.Fatalf("usage==PerCall must pass (strict >), got %v", err)
	}
	// 用量 > PerCall（110）→ ErrPerCall（post-call 执法）
	if err := e.AfterCall(model.Usage{TokensIn: 60, TokensOut: 50}); !errors.Is(err, ErrPerCall) {
		t.Fatalf("usage>PerCall want ErrPerCall, got %v", err)
	}
}

package budget

import (
	"errors"
	"testing"

	"loop-eng/internal/model"
)

// TestEnforcePerCall pins #103's post-call per-call brake: after a Call's REAL
// usage is known, a single call exceeding PerCall is rejected. The pre-call
// BeforeCall gates on a predictive Estimate (last usage × safety / floor) and
// can miss a FIRST overshoot — a pathological call far above the role's norm
// sails through pre-call and only inflates the Estimate for the next call. This
// post-call check catches that first overshoot on its actual usage.
func TestEnforcePerCall(t *testing.T) {
	e := New(100, 1000000, 3) // PerCall=100, PerTask=large
	// over ceiling → ErrPerCall (names the role + the real usage).
	if err := e.EnforcePerCall(model.Usage{TokensIn: 60, TokensOut: 50}, "execute"); !errors.Is(err, ErrPerCall) {
		t.Fatalf("usage 110 > PerCall 100 must reject; got %v", err)
	}
	// at/under ceiling → pass.
	if err := e.EnforcePerCall(model.Usage{TokensIn: 30, TokensOut: 20}, "execute"); err != nil {
		t.Fatalf("usage 50 <= PerCall 100 must pass; got %v", err)
	}
	if err := e.EnforcePerCall(model.Usage{TokensIn: 40, TokensOut: 60}, "verify"); err != nil {
		t.Fatalf("usage 100 == PerCall 100 must pass (not strictly greater); got %v", err)
	}
}

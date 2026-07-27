package budget

import (
	"errors"
	"fmt"

	"loop-eng/internal/model"
)

var (
	ErrPerCall = errors.New("budget: per-call token cap exceeded")
	ErrPerTask = errors.New("budget: per-task token cap exceeded")
)

type Enforcer struct {
	PerCall, PerTask, MaxRetries int
	spent                        int
	// last remembers each role's most recent real per-call token total so the
	// next Estimate(role) reflects observed reality instead of a constant. This
	// is the mechanism that makes the per-call brake real (spec §8.8): the
	// estimate BeforeCall checks now tracks actual usage × a safety factor.
	last map[string]int
}

func New(perCall, perTask, maxRetries int) *Enforcer {
	return &Enforcer{PerCall: perCall, PerTask: perTask, MaxRetries: maxRetries, last: map[string]int{}}
}

// estimateSafety multiplies a role's last observed usage to derive its next-call
// estimate. The failure direction is "block" (spec §8.8 — a brake errs toward
// refusing), so the factor is ≥1: an underestimate would let an over-budget call
// through, so we round up. This is part of the brake mechanism itself, NOT a
// user-facing knob (the three brakes' existence is unconfigurable per spec §8.8).
const estimateSafety = 2

// roleFloor is the conservative first-call estimate per role — used before any
// real usage has been observed (Estimate has no history yet). Each floor is a
// rough lower bound on a single LLM call's tokens for that role so the brake is
// meaningful on the very first call instead of admitting a constant 1000.
// defaultFloor is the fallback for any role not listed here. Like
// estimateSafety, these are internal brake constants, not config knobs.
var roleFloor = map[string]int{
	"plan":    20000,
	"execute": 80000,
	"verify":  20000,
	"triage":  15000,
	"help":    15000,
}

const defaultFloor = 20000

// Estimate returns the pre-call token estimate for a role: its last observed
// per-call usage × estimateSafety when history exists, else its roleFloor
// (defaultFloor for an unknown role). This replaces the old constant 1000 /
// "estimate == PerCall" tautology — the per-call brake now checks an estimate
// that tracks reality and can actually exceed PerCall.
func (e *Enforcer) Estimate(role string) int {
	if total, ok := e.last[role]; ok {
		return total * estimateSafety
	}
	if floor, ok := roleFloor[role]; ok {
		return floor
	}
	return defaultFloor
}

// Record accounts a Call's real usage for a role: it accrues the tokens into
// spent (the per-task tally, same as AfterCall) AND remembers the role's latest
// per-call total so the next Estimate(role) reflects reality. Only a non-zero
// total updates last[role] — a zero-usage Call must not poison the estimate.
func (e *Enforcer) Record(role string, u model.Usage) {
	total := u.TokensIn + u.TokensOut
	e.spent += total
	if total > 0 {
		if e.last == nil {
			e.last = map[string]int{}
		}
		e.last[role] = total
	}
}

// EnforcePerCall is the post-call per-call brake: after a Call's REAL usage is
// known, reject if THIS single call exceeded PerCall. The pre-call BeforeCall
// gates on a predictive Estimate (last usage × safety / floor), which can miss a
// FIRST overshoot — a single pathological call far above the role's norm sails
// through pre-call and only inflates the Estimate for the NEXT call. This closes
// that gap: a single call whose real usage > PerCall is caught and the loop
// aborts (blocked) instead of continuing to burn budget. nil when the call is
// within the per-call ceiling (the common case). Call after Record so the usage
// is accrued to the per-task tally regardless.
func (e *Enforcer) EnforcePerCall(u model.Usage, role string) error {
	used := u.TokensIn + u.TokensOut
	if used > e.PerCall {
		return fmt.Errorf("%w: single %s call used=%d per_call=%d", ErrPerCall, role, used, e.PerCall)
	}
	return nil
}

// BeforeCall: estimate 是单次调用的估算 token；超过 PerCall 即拒；累计+estimate 超 PerTask 也拒。
func (e *Enforcer) BeforeCall(estimate int) error {
	if estimate > e.PerCall {
		return fmt.Errorf("%w: estimate=%d per_call=%d", ErrPerCall, estimate, e.PerCall)
	}
	if e.spent+estimate > e.PerTask {
		return fmt.Errorf("%w: spent=%d estimate=%d per_task=%d", ErrPerTask, e.spent, estimate, e.PerTask)
	}
	return nil
}

func (e *Enforcer) AfterCall(u model.Usage) {
	e.spent += u.TokensIn + u.TokensOut
}

// Spent returns the running total of tokens accounted via AfterCall. Exposed
// for the budget.Client decorator test (carry-forward from Task 12: prove the
// decorator accumulates spend on every verify Call) and for run reports.
func (e *Enforcer) Spent() int { return e.spent }

// ShouldRetry: attempt 是当前第几次执行（1 起）；attempt <= MaxRetries 才重试。
func (e *Enforcer) ShouldRetry(attempt int) bool {
	return attempt <= e.MaxRetries
}

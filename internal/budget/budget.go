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
}

func New(perCall, perTask, maxRetries int) *Enforcer {
	return &Enforcer{PerCall: perCall, PerTask: perTask, MaxRetries: maxRetries}
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

// AfterCall accounts the real usage of a completed call. The running tally
// (spent) is always incremented — PerTask headroom still relies on it via the
// next BeforeCall. Post-call enforcement (#98): if a single call's real usage
// (TokensIn+TokensOut) strictly exceeds PerCall, it returns ErrPerCall so the
// loop can abort instead of continuing to burn the budget on subsequent calls.
// `==` PerCall is NOT a violation (strict >), mirroring BeforeCall's
// `estimate > PerCall` boundary. PerTask is NOT re-checked here — that remains
// BeforeCall's headroom responsibility (separation of concerns).
func (e *Enforcer) AfterCall(u model.Usage) error {
	e.spent += u.TokensIn + u.TokensOut
	if used := u.TokensIn + u.TokensOut; used > e.PerCall {
		return fmt.Errorf("%w: used=%d per_call=%d", ErrPerCall, used, e.PerCall)
	}
	return nil
}

// Spent returns the running total of tokens accounted via AfterCall. Exposed
// for the budget.Client decorator test (carry-forward from Task 12: prove the
// decorator accumulates spend on every verify Call) and for run reports.
func (e *Enforcer) Spent() int { return e.spent }

// ShouldRetry: attempt 是当前第几次执行（1 起）；attempt <= MaxRetries 才重试。
func (e *Enforcer) ShouldRetry(attempt int) bool {
	return attempt <= e.MaxRetries
}

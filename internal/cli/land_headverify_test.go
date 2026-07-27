package cli

import (
	"strings"
	"testing"
	"time"
)

// TestVerifyPRHeadConverges pins the happy paths: an immediate match passes with
// no sleep, and a transient GitHub head-sync lag (stale on the first poll, then
// converged) passes after one sleep. createPR relies on this to confirm the PR
// it just opened will be merged at the pushed tip.
func TestVerifyPRHeadConverges(t *testing.T) {
	tip := "abc123def456"
	// immediate convergence — no sleep.
	var slept int
	if err := verifyPRHead(tip, func() (string, error) { return tip, nil }, func(time.Duration) { slept++ }, 5); err != nil {
		t.Fatalf("immediate match must pass: %v", err)
	}
	if slept != 0 {
		t.Fatalf("immediate match must not sleep, got %d", slept)
	}
	// lags once (sync lag), then converges — one sleep.
	calls := 0
	if err := verifyPRHead(tip, func() (string, error) {
		calls++
		if calls == 1 {
			return "stale0000000", nil // first poll: stale head
		}
		return tip, nil
	}, func(time.Duration) {}, 5); err != nil {
		t.Fatalf("must converge after one lag: %v", err)
	}
}

// TestVerifyPRHeadStallErrors pins the guard: a persistent stall (head never
// converges to the tip) returns an error naming both SHAs, and sleeps once per
// retry (attempts-1 sleeps). This is the signal createPR logs so the operator
// knows to verify head==tip before a merge that would miss commits (#97/#115).
func TestVerifyPRHeadStallErrors(t *testing.T) {
	tip := "abc123def456"
	var slept int
	err := verifyPRHead(tip, func() (string, error) { return "stale0000000", nil }, func(time.Duration) { slept++ }, 3)
	if err == nil {
		t.Fatal("persistent stall must error")
	}
	if !strings.Contains(err.Error(), "abc123") || !strings.Contains(err.Error(), "stale0") {
		t.Fatalf("error must name both SHAs; got: %v", err)
	}
	if slept != 2 { // 3 attempts → sleeps after attempt 1 and 2
		t.Fatalf("want 2 sleeps (attempts-1), got %d", slept)
	}
}

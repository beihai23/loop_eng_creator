package channel

import "testing"

// TestGhPermanent pins ghExec's retry classification (H4): auth/not-found are
// permanent and short-circuit (no 2s/4s backoff burn), while rate-limit 403 /
// 5xx / network / TLS-timeout remain retryable. 403 is deliberately NOT
// permanent — GitHub rate-limit 403s are transient.
func TestGhPermanent(t *testing.T) {
	permanent := []string{
		"HTTP 401: Bad credentials",
		"HTTP 404: Not Found",
		"authentication required; run gh auth login",
		"could not find the issue",
	}
	for _, s := range permanent {
		if !ghPermanent(s) {
			t.Errorf("ghPermanent(%q) = false, want true (permanent)", s)
		}
	}
	retryable := []string{
		"HTTP 403: rate limit exceeded",
		"HTTP 502: Bad Gateway",
		"HTTP 503: Service Unavailable",
		"dial tcp: i/o timeout",
		"TLS handshake timeout",
		"connection refused",
	}
	for _, s := range retryable {
		if ghPermanent(s) {
			t.Errorf("ghPermanent(%q) = true, want false (retryable)", s)
		}
	}
}

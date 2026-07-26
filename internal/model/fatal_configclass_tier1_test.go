package model

import "testing"

// TestTier1FatalConfigClass pins the config/env-class fatal signals added for the
// agent-smoke scenario fix (task #87): codex trusted-dir + codex quota
// (429/usage-limit/rate-limit) and opencode/kimi/kilo insufficient-balance must
// all classify as FATAL (retry cannot heal them -> ErrClaudeFatal short-circuit),
// while a 5xx gateway timeout must NOT (it must still ride the retry loop).
func TestTier1FatalConfigClass(t *testing.T) {
	cases := []struct {
		name   string
		fatal  func(string, string) bool
		stdout string
		want   bool
	}{
		{"codex trusted-dir", isFatalCodexError, "Not inside a trusted directory and --skip-git-repo-check was not specified", true},
		{"codex usage-limit", isFatalCodexError, "Error: usage limit exceeded", true},
		{"codex 429-rate-limit", isFatalCodexError, "rate limit exceeded (429)", true},
		{"codex transient-504-not-fatal", isFatalCodexError, "upstream gateway timeout (504)", false},
		{"opencode insufficient-balance", isFatalOpencodeError, "Error: Insufficient Balance", true},
		{"kimi insufficient-balance", isFatalKimiError, "insufficient balance", true},
		{"kilo insufficient-balance", isFatalKiloError, "insufficient balance", true},
	}
	for _, tc := range cases {
		if got := tc.fatal("", tc.stdout); got != tc.want {
			t.Fatalf("%s: isFatal=%v want %v (stdout=%q)", tc.name, got, tc.want, tc.stdout)
		}
	}
}

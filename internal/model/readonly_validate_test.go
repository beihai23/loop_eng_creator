package model

import (
	"strings"
	"testing"

	"loop-eng/internal/config"
)

// TestValidateProvidersReadOnly pins #101: a ReadOnly role on a provider without
// headless read-only (opencode/kimi/kilo) is rejected at config validation — it
// used to be silently ignored, so the role ran with write permission. claude and
// codex (which honor ReadOnly) are accepted.
func TestValidateProvidersReadOnly(t *testing.T) {
	// claude + codex with readonly: accepted (they honor it: plan mode / read-only sandbox).
	for _, p := range []string{"claude", "codex"} {
		cfg := &config.Config{Models: config.Models{Plan: config.ModelRef{Provider: p, ReadOnly: true}}}
		if err := ValidateProviders(cfg); err != nil {
			t.Fatalf("readonly+%s must be accepted: %v", p, err)
		}
	}
	// opencode/kimi/kilo + readonly: rejected (no headless read-only mode).
	for _, p := range []string{"opencode", "kimi", "kilo"} {
		cfg := &config.Config{Models: config.Models{Plan: config.ModelRef{Provider: p, ReadOnly: true}}}
		err := ValidateProviders(cfg)
		if err == nil {
			t.Fatalf("readonly+%s must be rejected (no headless read-only)", p)
		}
		if !strings.Contains(err.Error(), "read-only") {
			t.Fatalf("error must explain read-only unsupported: %v", err)
		}
	}
}

// TestCodexReadOnlyArgs pins #101: a ReadOnly codex role is forced into a
// read-only sandbox even if cmd tried to widen it (--sandbox workspace-write),
// mirroring how claude's plan-mode profile strips a mis-configured bypass.
func TestCodexReadOnlyArgs(t *testing.T) {
	got := codexReadOnlyArgs([]string{"--sandbox", "workspace-write", "-m", "x"})
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "--sandbox read-only") {
		t.Fatalf("must force --sandbox read-only; got %v", got)
	}
	if strings.Contains(joined, "workspace-write") {
		t.Fatalf("must strip the prior --sandbox workspace-write; got %v", got)
	}
}

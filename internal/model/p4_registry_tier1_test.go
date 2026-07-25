package model

import (
	"context"
	"strings"
	"testing"

	"loop-eng/internal/config"
)

// TestTier1P4FiveProviderRegistry verifies P4's provider-registry contract.
// It depends ONLY on the frozen NewAgent / Agent.Provider / Agent.Check
// signatures landed in #68 plus the three new P4 cases — it does not assume any
// new internal struct/field name, so it stays valid however the adapters are
// built. Covers: (1) all five keys dispatch with the right Provider(); (2) the
// default-empty key still resolves to claude (out-of-box unchanged); (3) Check
// fails for a missing binary on every provider (doctor contract); (4) the
// unknown-provider error names all five keys.
func TestTier1P4FiveProviderRegistry(t *testing.T) {
	want := map[string]string{
		"":         "claude",
		"claude":   "claude",
		"codex":    "codex",
		"opencode": "opencode",
		"kimi":     "kimi",
		"kilo":     "kilo",
	}
	for key, prov := range want {
		a, err := NewAgent(config.ModelRef{Provider: key, Binary: "/no/such/p4-bin-zzz"})
		if err != nil {
			t.Fatalf("NewAgent(provider=%q): %v", key, err)
		}
		if got := a.Provider(); got != prov {
			t.Fatalf("provider %q: Provider() want %q, got %q", key, prov, got)
		}
		if err := a.Check(context.Background()); err == nil {
			t.Fatalf("provider %q: Check must fail when binary is missing", key)
		}
	}
	_, err := NewAgent(config.ModelRef{Provider: "no-such-provider"})
	if err == nil {
		t.Fatal("unknown provider must return an error")
	}
	low := strings.ToLower(err.Error())
	for _, key := range []string{"claude", "codex", "opencode", "kimi", "kilo"} {
		if !strings.Contains(low, key) {
			t.Fatalf("unknown-provider error must list %q; got %q", key, err.Error())
		}
	}
}

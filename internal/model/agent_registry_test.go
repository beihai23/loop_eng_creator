package model

import (
	"strings"
	"testing"

	"loop-eng/internal/config"
)

// TestNewAgentRecognizesAllProviders asserts NewAgent dispatches every supported
// provider key — claude/codex/opencode/kimi/kilo — plus the "" default → claude
// (开箱行为不变: a default config with no provider field keeps today's `claude -p`
// behavior). An unknown provider must return an error, and the error text
// (lowercased) must list all five keys so a misconfigured role learns the valid
// set from the doctor / buildModels error alone.
func TestNewAgentRecognizesAllProviders(t *testing.T) {
	cases := map[string]string{
		"":         "claude",
		"claude":   "claude",
		"codex":    "codex",
		"opencode": "opencode",
		"kimi":     "kimi",
		"kilo":     "kilo",
	}
	for key, provider := range cases {
		a, err := NewAgent(config.ModelRef{Provider: key, Binary: "x"})
		if err != nil {
			t.Errorf("provider %q: NewAgent errored: %v", key, err)
			continue
		}
		if got := a.Provider(); got != provider {
			t.Errorf("provider %q: want %s, got %s", key, provider, got)
		}
	}

	_, err := NewAgent(config.ModelRef{Provider: "no-such-provider"})
	if err == nil {
		t.Fatal("unknown provider must return an error")
	}
	low := strings.ToLower(err.Error())
	for _, key := range []string{"claude", "codex", "opencode", "kimi", "kilo"} {
		if !strings.Contains(low, key) {
			t.Errorf("unknown-provider error must list %q; got: %v", key, err)
		}
	}
}

package model

import (
	"strings"
	"testing"

	"loop-eng/internal/config"
)

func TestTier1ProviderRegistry(t *testing.T) {
	names := RegisteredProviders()
	// registry 必须承载 NewAgent 当前派发的全部 provider——漏挂任一是回归
	// (applyTaskAgent/agentForRole 调 NewAgent，preflight/agent_tier1 测试依赖)。
	for _, want := range []string{"claude", "codex", "opencode", "kimi", "kilo"} {
		if !hasStr(names, want) {
			t.Fatalf("RegisteredProviders must include %s, got %v", want, names)
		}
	}
	a, err := NewAgent(config.ModelRef{Provider: "", Binary: "claude"})
	if err != nil || a.Provider() != "claude" {
		t.Fatalf("empty provider must resolve to claude, got %v, %v", a, err)
	}
	if _, err := NewAgent(config.ModelRef{Provider: "codx", Binary: "x"}); err == nil {
		t.Fatal("unknown provider must return error")
	}
	bad := &config.Config{Models: config.Models{
		Triage:  config.ModelRef{Binary: "claude"},
		Plan:    config.ModelRef{Binary: "claude"},
		Execute: config.ModelRef{Provider: "codx", Binary: "x"},
		Verify:  config.ModelRef{Binary: "claude"},
	}}
	err = ValidateProviders(bad)
	if err == nil {
		t.Fatal("ValidateProviders must reject unknown provider codx")
	}
	for _, want := range []string{"models.execute", "codx", "claude", "codex"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("ValidateProviders error missing %q: %v", want, err)
		}
	}
	good := &config.Config{Models: config.Models{
		Triage:  config.ModelRef{Provider: "", Binary: "claude"},
		Plan:    config.ModelRef{Provider: "claude", Binary: "claude"},
		Execute: config.ModelRef{Provider: "codex", Binary: "codex"},
		Verify:  config.ModelRef{Provider: "", Binary: "claude"},
	}}
	if err := ValidateProviders(good); err != nil {
		t.Fatalf("registered/empty providers must validate clean, got %v", err)
	}
}

func hasStr(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

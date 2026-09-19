package config

// M1 出题权分离（models.test_prep）的配置层测试：可选语义（零值=legacy）、
// round-trip、不完整配置的硬错误。

import (
	"path/filepath"
	"testing"
)

func TestTestPrepOptionalByDefault(t *testing.T) {
	p := writeFile(t, `
models:
  triage: { binary: c }
  plan: { binary: c }
  execute: { binary: c }
  verify: { binary: c }
budget: { per_call_tokens: 1, per_task_tokens: 1, max_retries: 1 }
channel: { provider: local }
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("legacy config（无 test_prep）必须可加载: %v", err)
	}
	if !cfg.Models.TestPrep.IsZero() {
		t.Fatalf("未配置时 TestPrep 应为零值: %+v", cfg.Models.TestPrep)
	}
}

func TestTestPrepRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	mk := func() ModelRef { return ModelRef{Via: "claude-p", Binary: "claude"} }
	cfg := &Config{
		Models: Models{
			Triage: mk(), Plan: mk(), Execute: mk(), Verify: mk(),
			TestPrep: ModelRef{Provider: "claude", Binary: "claude", Name: "sonnet", ReadOnly: true},
		},
		Budget: Budget{PerCallTokens: 1, PerTaskTokens: 2, MaxRetries: 3},
		Channel: Channel{
			Provider: "local",
		},
	}
	if err := Save(p, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	tp := got.Models.TestPrep
	if tp.Provider != "claude" || tp.Binary != "claude" || tp.Name != "sonnet" || !tp.ReadOnly {
		t.Fatalf("test_prep round-trip mismatch: %+v", tp)
	}
	if tp.IsZero() {
		t.Fatal("已配置的 TestPrep 不应判零")
	}
}

func TestLoadRejectsIncompleteTestPrep(t *testing.T) {
	p := writeFile(t, `
models:
  triage: { binary: c }
  plan: { binary: c }
  execute: { binary: c }
  verify: { binary: c }
  test_prep: { provider: claude, readonly: true }
budget: { per_call_tokens: 1, per_task_tokens: 1, max_retries: 1 }
channel: { provider: local }
`)
	if _, err := Load(p); err == nil {
		t.Fatal("test_prep 配置了但缺 name/binary 应硬错误（与其他角色同一门槛）")
	}
}

func TestModelRefIsZero(t *testing.T) {
	if !(ModelRef{}.IsZero()) {
		t.Fatal("零值 ModelRef 应判零")
	}
	for name, r := range map[string]ModelRef{
		"provider": {Provider: "claude"},
		"name":     {Name: "sonnet"},
		"binary":   {Binary: "claude"},
		"via":      {Via: "claude-p"},
		"cmd":      {Cmd: []string{"claude", "-p"}},
		"readonly": {ReadOnly: true},
	} {
		if r.IsZero() {
			t.Fatalf("%s 字段非零时不应判零: %+v", name, r)
		}
	}
}

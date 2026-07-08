package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadValid(t *testing.T) {
	p := writeFile(t, `
models:
  triage: { provider: anthropic, name: claude-haiku-4-5 }
budget:
  per_call_tokens: 20000
  per_task_tokens: 200000
  max_retries: 3
verify:
  deterministic:
    - { label: tests, cmd: ["pytest", "-q"] }
isolation: { worktree: true }
skills: { dir: .loop/skills }
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
	if cfg.Budget.MaxRetries != 3 {
		t.Fatalf("max_retries=%d want 3", cfg.Budget.MaxRetries)
	}
	if cfg.Verify.Deterministic[0].Cmd[0] != "pytest" {
		t.Fatalf("cmd not parsed")
	}
}

func TestLoadRejectsMissingBudget(t *testing.T) {
	p := writeFile(t, `models: { triage: { name: x } }`)
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for missing budget")
	}
}

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
  triage:  { provider: anthropic, name: claude-haiku-4-5 }
  plan:    { name: claude-haiku-4-5 }
  execute: { name: claude-haiku-4-5 }
  verify:  { name: claude-haiku-4-5 }
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

// roleConfig builds a valid config where every role is configured (name set)
// EXCEPT the one named in omit, whose name+binary are both left empty.
func roleConfig(omit string) string {
	var b strings.Builder
	b.WriteString("models:\n")
	for _, role := range []string{"triage", "plan", "execute", "verify"} {
		if role == omit {
			b.WriteString("  " + role + ": {}\n") // name 与 binary 均空
			continue
		}
		b.WriteString("  " + role + ": { name: x }\n")
	}
	b.WriteString(`
budget:
  per_call_tokens: 20000
  per_task_tokens: 200000
  max_retries: 3
`)
	return b.String()
}

// TestLoadRejectsMissingRole guards the M2-1 fix: every role (triage/plan/
// execute/verify) must set name or binary; missing one is a hard error.
// The plan case is the explicit acceptance criterion; execute/verify/triage
// are covered to confirm the loop checks all four roles.
func TestLoadRejectsMissingRole(t *testing.T) {
	for _, role := range []string{"plan", "execute", "verify", "triage"} {
		t.Run(role, func(t *testing.T) {
			p := writeFile(t, roleConfig(role))
			_, err := Load(p)
			if err == nil {
				t.Fatalf("expected error when models.%s missing", role)
			}
			// 报错必须点名缺失的 role，而非误报成 budget 等。
			if !strings.Contains(err.Error(), "models."+role) {
				t.Fatalf("error should blame models.%s, got: %v", role, err)
			}
		})
	}
}

func TestLoadParsesChannel(t *testing.T) {
	p := writeFile(t, `
models:
  triage:  { binary: claude }
  plan:    { binary: claude }
  execute: { binary: claude }
  verify:  { binary: claude }
budget:
  per_call_tokens: 20000
  per_task_tokens: 200000
  max_retries: 3
channel:
  provider: github
  repo: beihai23/loop_eng_creator
  task_label: "loop:task"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
	if cfg.Channel.Provider != "github" || cfg.Channel.Repo != "beihai23/loop_eng_creator" {
		t.Fatalf("channel not parsed: %+v", cfg.Channel)
	}
	if cfg.Channel.TaskLabel != "loop:task" {
		t.Fatalf("task_label not parsed: %q", cfg.Channel.TaskLabel)
	}
}

func TestLoadParsesDaemonPollInterval(t *testing.T) {
	p := writeFile(t, `
models:
  triage: { binary: c }
  plan: { binary: c }
  execute: { binary: c }
  verify: { binary: c }
budget: { per_call_tokens: 1, per_task_tokens: 1, max_retries: 1 }
daemon: { poll_interval: 90s }
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
	if cfg.Daemon.PollInterval != 90*time.Second {
		t.Fatalf("poll_interval = %v, want 90s", cfg.Daemon.PollInterval)
	}
}

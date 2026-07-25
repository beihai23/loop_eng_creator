package e2e

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"loop-eng/internal/config"
	"loop-eng/internal/model"
)

// The real-binary smokes below exercise the full provider path (NewAgent →
// AsClient/AsExecuter → Call/Exec) against a live coding-agent CLI — the "e2e
// 真 binary 冒烟" acceptance, one per provider (claude / codex / opencode /
// kimi / kilo).
//
// Gating (deliberate): these run ONLY when LOOP_ENG_AGENT_SMOKE=1 is set AND the
// binary is on PATH. A real claude/codex call costs tokens, takes seconds, and
// can flake on rate-limiting — letting it run on every `go test ./...` would
// endanger the all-green gate. The equivalent deterministic contract (cmd.Dir =
// worktree, retry, fatal short-circuit) is covered by internal/model/exec_test.go
// + exec_codex_test.go's fake-binary tests, which always run. So: no creds → no
// opt-in → honest t.Skip; opt in when you have a live binary+auth to smoke for real.
const smokeEnv = "LOOP_ENG_AGENT_SMOKE"

// TestClaudeBinarySmoke drives the claude provider against a real `claude` CLI:
// a trivial print-mode call must return non-empty output without error.
func TestClaudeBinarySmoke(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skipf("claude binary not on PATH: %v", err)
	}
	if os.Getenv(smokeEnv) != "1" {
		t.Skipf("smoke not opted in (set %s=1 to run the real-binary claude smoke)", smokeEnv)
	}
	a, err := model.NewAgent(config.ModelRef{Provider: "claude", Binary: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := model.AsClient(a).Call(context.Background(),
		"Reply with exactly the two characters: OK")
	if err != nil {
		t.Fatalf("claude smoke Call failed: %v", err)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatalf("claude smoke returned empty output")
	}
	t.Logf("claude smoke ok: %q", strings.TrimSpace(out))
}

// TestCodexBinarySmoke drives the codex provider against a real `codex` CLI: a
// trivial exec call must return non-empty output without error. Requires codex
// auth (CODEX_API_KEY or `codex login`).
func TestCodexBinarySmoke(t *testing.T) {
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skipf("codex binary not on PATH: %v", err)
	}
	if os.Getenv(smokeEnv) != "1" {
		t.Skipf("smoke not opted in (set %s=1 to run the real-binary codex smoke)", smokeEnv)
	}
	a, err := model.NewAgent(config.ModelRef{Provider: "codex", Binary: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := model.AsExecuter(a).Exec(context.Background(),
		t.TempDir(), "Reply with exactly the two characters: OK")
	if err != nil {
		t.Fatalf("codex smoke Exec failed: %v", err)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatalf("codex smoke returned empty output")
	}
	t.Logf("codex smoke ok: %q", strings.TrimSpace(out))
}

// TestOpencodeBinarySmoke drives the opencode provider against a real `opencode`
// CLI: a trivial `opencode run` call must return non-empty output without error.
// Requires opencode auth (`opencode auth login` or a provider key env).
func TestOpencodeBinarySmoke(t *testing.T) {
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skipf("opencode binary not on PATH: %v", err)
	}
	if os.Getenv(smokeEnv) != "1" {
		t.Skipf("smoke not opted in (set %s=1 to run the real-binary opencode smoke)", smokeEnv)
	}
	a, err := model.NewAgent(config.ModelRef{Provider: "opencode", Binary: "opencode"})
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := model.AsExecuter(a).Exec(context.Background(),
		t.TempDir(), "Reply with exactly the two characters: OK")
	if err != nil {
		t.Fatalf("opencode smoke Exec failed: %v", err)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatalf("opencode smoke returned empty output")
	}
	t.Logf("opencode smoke ok: %q", strings.TrimSpace(out))
}

// TestKimiBinarySmoke drives the kimi provider against a real `kimi` CLI: a
// trivial `kimi -p` call must return non-empty output without error. Requires
// kimi auth (MOONSHOT_API_KEY/KIMI_API_KEY or `kimi login`).
func TestKimiBinarySmoke(t *testing.T) {
	if _, err := exec.LookPath("kimi"); err != nil {
		t.Skipf("kimi binary not on PATH: %v", err)
	}
	if os.Getenv(smokeEnv) != "1" {
		t.Skipf("smoke not opted in (set %s=1 to run the real-binary kimi smoke)", smokeEnv)
	}
	a, err := model.NewAgent(config.ModelRef{Provider: "kimi", Binary: "kimi"})
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := model.AsExecuter(a).Exec(context.Background(),
		t.TempDir(), "Reply with exactly the two characters: OK")
	if err != nil {
		t.Fatalf("kimi smoke Exec failed: %v", err)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatalf("kimi smoke returned empty output")
	}
	t.Logf("kimi smoke ok: %q", strings.TrimSpace(out))
}

// TestKiloBinarySmoke drives the kilo provider against a real `kilo` CLI: a
// trivial `kilo run --auto` call must return non-empty output without error.
// Requires kilo auth (KILO_API_KEY or configured kilo provider auth).
func TestKiloBinarySmoke(t *testing.T) {
	if _, err := exec.LookPath("kilo"); err != nil {
		t.Skipf("kilo binary not on PATH: %v", err)
	}
	if os.Getenv(smokeEnv) != "1" {
		t.Skipf("smoke not opted in (set %s=1 to run the real-binary kilo smoke)", smokeEnv)
	}
	a, err := model.NewAgent(config.ModelRef{Provider: "kilo", Binary: "kilo"})
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := model.AsExecuter(a).Exec(context.Background(),
		t.TempDir(), "Reply with exactly the two characters: OK")
	if err != nil {
		t.Fatalf("kilo smoke Exec failed: %v", err)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatalf("kilo smoke returned empty output")
	}
	t.Logf("kilo smoke ok: %q", strings.TrimSpace(out))
}

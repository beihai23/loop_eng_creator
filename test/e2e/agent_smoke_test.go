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
// 真 binary 冒烟" acceptance for ≥2 providers (claude + codex).
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

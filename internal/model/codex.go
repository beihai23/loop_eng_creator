// codex.go is the codex provider implementation of Agent.
//
// codex (https://github.com/openai/codex) is OpenAI's headless coding agent. Its
// non-interactive entry point is `codex exec`; the prompt is read from stdin,
// the model selected with -m, the working directory with --cd/-C. Like claude,
// a single Run is a FRESH session (no conversation state shared between calls),
// which is what loop-eng's verification-independence assumption requires.
//
// This adapter maps the provider-neutral Agent contract onto codex's native
// flags, and absorbs codex-specific shape (prompt-via-stdin, workspace sandbox,
// auth via CODEX_API_KEY / ~/.codex/auth.json). Fatal auth/credential errors wrap
// the shared model.ErrClaudeFatal so SubLoop's fatal short-circuit fires uniformly
// across providers (the symbol survival list in the issue: codex fatal errors
// must wrap ErrClaudeFatal too).

package model

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"loop-eng/internal/config"
)

// codexAgent is the codex provider. binary/model/args come from config.ModelRef
// (Binary defaults to "codex" via binaryOf; Name → -m; Cmd → native passthrough).
type codexAgent struct {
	binary string
	model  string
	args   []string
}

// newCodexAgent builds a codexAgent from a config.ModelRef. Binary falls back to
// "codex" when unset; Name is the -m model; Cmd is appended verbatim (native
// passthrough for provider-specific flags).
func newCodexAgent(ref config.ModelRef) *codexAgent {
	return &codexAgent{
		binary: binaryOf(ref, "codex"),
		model:  ref.Name,
		args:   ref.Cmd,
	}
}

func (a *codexAgent) Provider() string { return "codex" }

// Run executes `codex exec [--cd <workdir>] [-m <model>] <args…>` with the
// prompt on stdin, cmd.Dir=workdir, retried on transient failure. Setting
// cmd.Dir is the exec_test.go-equivalent contract: the agent's edits must land
// on the isolated worktree, exactly like ClaudeClient.Exec.
func (a *codexAgent) Run(ctx context.Context, req AgentRequest) (AgentResult, error) {
	model := req.Model
	if model == "" {
		model = a.model
	}
	out, u, err := runWithRetry(ctx, "codex exec",
		func(ctx context.Context) (string, string, error) {
			return a.runOnce(ctx, req.Workdir, model, req.Prompt)
		},
		isFatalCodexError)
	return AgentResult{Out: out, Usage: u}, err
}

// runOnce builds and executes a single `codex exec` attempt. cmd.Dir is set to
// dir so the agent operates inside the worktree (and `--cd` is passed too so the
// agent's own cwd resolution agrees). Prompt goes to stdin (codex reads stdin
// when no positional prompt is given).
func (a *codexAgent) runOnce(ctx context.Context, dir, model, prompt string) (string, string, error) {
	args := []string{"exec"}
	if dir != "" {
		args = append(args, "--cd", dir)
	}
	if model != "" {
		args = append(args, "-m", model)
	}
	args = append(args, a.args...)
	cmd := exec.CommandContext(ctx, a.binary, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Stdin = strings.NewReader(prompt)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err := cmd.Run()
	return out.String(), errBuf.String(), err
}

// fatalCodexSignals are lowercased substrings that mark a non-retryable codex
// failure. Besides auth/credential (mirrors fatalClaudeSignals for claude), this
// also covers the config/env-class failures retry cannot heal: the trusted-dir
// refusal ("Not inside a trusted directory", which the smoke hit only because
// the harness used a bare tempdir instead of a git worktree — real loop usage in
// a worktree never trips it) and the account quota/rate-limit class (usage
// limit / 429 / rate limit) that an exhausted codex account emits. Hitting any
// → ErrClaudeFatal short-circuit, no 30/60/120s backoff burn.
var fatalCodexSignals = []string{
	"authentication", "unauthorized", "not authorized",
	"401", "403",
	"api key", "api_key", "apikey",
	"credential", "not logged in", "login required", "please log in",
	"invalid api key", "missing api key",
	// config/env-class: trusted-dir refusal + account quota/rate-limit. These
	// never self-heal on retry, so they are fatal alongside auth.
	"not inside a trusted directory",
	"usage limit", "usage_limit", "quota",
	"rate limit", "rate_limit", "429",
	"insufficient balance", "insufficient_balance",
}

// isFatalCodexError reports whether codex's combined output looks like a
// non-retryable failure — auth/credential OR a config/env-class error retry
// cannot heal (trusted-dir refusal, account quota/usage-limit/rate-limit/429,
// insufficient balance). ErrClaudeFatal short-circuits it either way.
func isFatalCodexError(stderr, stdout string) bool {
	s := strings.ToLower(stderr + " " + stdout)
	for _, sig := range fatalCodexSignals {
		if strings.Contains(s, sig) {
			return true
		}
	}
	return false
}

// Check is the doctor/preflight self-check: the codex binary must be on PATH and
// at least one auth source must be in place (CODEX_API_KEY env or a saved
// ~/.codex/auth.json from `codex login`). Returns a descriptive error otherwise.
func (a *codexAgent) Check(ctx context.Context) error {
	if err := checkBinary(a.binary); err != nil {
		return err
	}
	if os.Getenv("CODEX_API_KEY") != "" {
		return nil
	}
	if home, err := os.UserHomeDir(); err == nil {
		if _, statErr := os.Stat(filepath.Join(home, ".codex", "auth.json")); statErr == nil {
			return nil
		}
	}
	return fmt.Errorf("codex: no auth — set CODEX_API_KEY or run `codex login`")
}

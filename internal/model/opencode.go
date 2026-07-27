// opencode.go is the opencode provider implementation of Agent.
//
// opencode (https://opencode.ai) is a terminal coding agent with a non-interactive
// `opencode run` entry point. Unlike codex (which reads the prompt from stdin),
// opencode takes the prompt as a POSITIONAL argument after `run`; the model is
// selected with -m provider/model, the working directory with --dir, and
// autonomous tool use is enabled with --auto. As with every provider, a single
// Run is a FRESH session (no conversation state shared between calls), which is
// what loop-eng's verification-independence assumption requires.
//
// This adapter maps the provider-neutral Agent contract onto opencode's native
// flags and absorbs opencode-specific shape (prompt-via-positional-argv,
// multi-provider auth via `opencode auth login` / provider key env). Fatal auth/
// credential errors wrap the shared model.ErrClaudeFatal so SubLoop's fatal
// short-circuit fires uniformly across providers.

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

// opencodeAgent is the opencode provider. binary/model/args come from
// config.ModelRef (Binary defaults to "opencode" via binaryOf; Name → -m;
// Cmd → native passthrough appended before the positional prompt).
type opencodeAgent struct {
	binary string
	model  string
	args   []string
}

// newOpencodeAgent builds an opencodeAgent from a config.ModelRef. Binary falls
// back to "opencode" when unset; Name is the -m model; Cmd is appended verbatim
// before the positional prompt (native passthrough for provider-specific flags).
func newOpencodeAgent(ref config.ModelRef) *opencodeAgent {
	return &opencodeAgent{
		binary: binaryOf(ref, "opencode"),
		model:  ref.Name,
		args:   ref.Cmd,
	}
}

func (a *opencodeAgent) Provider() string { return "opencode" }

// Run executes `opencode run --auto [--dir <workdir>] [-m <model>] <args…>
// <prompt>` with cmd.Dir=workdir, retried on transient failure. The prompt is
// passed as the final positional argument (opencode reads it from argv, not
// stdin). Setting cmd.Dir is the exec_test.go-equivalent contract: the agent's
// edits must land on the isolated worktree, exactly like ClaudeClient.Exec.
func (a *opencodeAgent) Run(ctx context.Context, req AgentRequest) (AgentResult, error) {
	model := req.Model
	if model == "" {
		model = a.model
	}
	out, u, err := runWithRetry(ctx, "opencode run",
		func(ctx context.Context) (string, string, error) {
			return a.runOnce(ctx, req.Workdir, model, req.Prompt)
		},
		isFatalOpencodeError)
	return AgentResult{Out: out, Usage: u}, err
}

// runOnce builds and executes a single `opencode run` attempt. The prompt is
// appended as the LAST positional argument (after --auto, the flags, and native
// passthrough args) so opencode receives it as the task message. cmd.Dir is set
// to dir so the agent operates inside the worktree (--dir is passed too so
// opencode's own cwd resolution agrees). Stdin is left at the null device —
// opencode does not read the prompt from stdin.
func (a *opencodeAgent) runOnce(ctx context.Context, dir, model, prompt string) (string, string, error) {
	args := []string{"run", "--auto"}
	if dir != "" {
		args = append(args, "--dir", dir)
	}
	if model != "" {
		args = append(args, "-m", model)
	}
	args = append(args, a.args...)
	args = append(args, prompt)
	cmd := exec.CommandContext(ctx, a.binary, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err := cmd.Run()
	return out.String(), errBuf.String(), err
}

// fatalOpencodeSignals are lowercased substrings that mark a non-retryable
// opencode failure. Besides auth/credential (mirrors fatalCodexSignals for
// codex), this also covers the balance/quota class ("Insufficient Balance" /
// out-of-quota) that an account with no credit emits — retry cannot heal it, so
// it is fatal (ErrClaudeFatal short-circuit, no 30/60/120s backoff burn).
var fatalOpencodeSignals = []string{
	"authentication", "unauthorized", "not authorized",
	"401", "403",
	"api key", "api_key", "apikey",
	"credential", "not logged in", "login required", "please log in",
	"invalid api key", "missing api key",
	// balance/quota class: no credit on the account. Never self-heals on retry.
	"insufficient balance", "insufficient_balance",
	"out of balance", "no balance",
	"quota", "usage limit", "usage_limit",
}

// isFatalOpencodeError reports whether opencode's combined output looks like a
// non-retryable failure — auth/credential OR an account balance/quota error
// ("Insufficient Balance" / out-of-quota) retry cannot heal. ErrClaudeFatal
// short-circuits it either way.
func isFatalOpencodeError(stderr, stdout string) bool {
	s := strings.ToLower(stderr + " " + stdout)
	for _, sig := range fatalOpencodeSignals {
		if strings.Contains(s, sig) {
			return true
		}
	}
	return false
}

// Check is the doctor/preflight self-check: the opencode binary must be on PATH
// and at least one auth source must be in place — a provider API key env that
// opencode recognizes (ANTHROPIC_API_KEY / OPENAI_API_KEY / OPENROUTER_API_KEY)
// or a saved opencode auth file from `opencode auth login`. Returns a
// descriptive error otherwise. (Auth detection is best-effort and intentionally
// permissive — opencode is multi-provider; the hermetic contract test covers
// only the binary-missing path, which is what doctor gating relies on.)
func (a *opencodeAgent) Check(ctx context.Context) error {
	if err := checkBinary(a.binary); err != nil {
		return err
	}
	for _, env := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "OPENROUTER_API_KEY"} {
		if os.Getenv(env) != "" {
			return nil
		}
	}
	if dir, err := os.UserConfigDir(); err == nil {
		if _, statErr := os.Stat(filepath.Join(dir, "opencode", "auth.json")); statErr == nil {
			return nil
		}
	}
	return fmt.Errorf("opencode: no auth — run `opencode auth login` or set a provider API key env")
}

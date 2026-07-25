// kilo.go is the kilo (Kilo Code) provider implementation of Agent.
//
// Kilo Code (https://kilo.ai/docs/code-with-ai/platforms/cli) is a coding agent
// whose non-interactive entry point is `kilo run --auto`. The prompt is a
// POSITIONAL argument after `run --auto`; the working directory is the process
// cwd (cmd.Dir). As with every provider, a single Run is a FRESH session,
// matching loop-eng's verification-independence assumption.
//
// Divergence notes (issue risk section — absorbed here, not in config):
//   - Model selection has NO CLI flag. Kilo takes the model from env
//     (KILOCODE_MODEL / KILO_PROVIDER) or kilo.jsonc. This adapter forwards the
//     configured model as KILOCODE_MODEL on the child env (appended AFTER the
//     inherited parent env, so ref.Name wins over any ambient KILOCODE_MODEL;
//     the parent env is inherited wholesale so KILO_API_KEY auth and PATH
//     survive).
//   - Autonomous tool use is configured in kilo.jsonc (permission.* = allow),
//     not via a CLI flag; `--auto` is part of the documented non-interactive
//     command and is included as the base here.
//   - `--continue` CANNOT be combined with `run --auto`, so this adapter does
//     not wire resume — every Run is a fresh session, which verification
//     independence wants anyway.
//   - Exit 124 = timeout. Kilo timeouts are treated as RETRYABLE (not fatal):
//     they surface as a normal attempt error and ride the shared retry loop
//     like any transient failure. Only an auth/credential signal promotes to
//     fatal.
//
// Fatal auth/credential errors wrap the shared model.ErrClaudeFatal so SubLoop's
// fatal short-circuit fires uniformly across providers.

package model

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"loop-eng/internal/config"
)

// kiloAgent is the kilo provider. binary/model/args come from config.ModelRef
// (Binary defaults to "kilo" via binaryOf; Name → KILOCODE_MODEL env, NOT a CLI
// flag; Cmd → native passthrough appended before the positional prompt).
type kiloAgent struct {
	binary string
	model  string
	args   []string
}

// newKiloAgent builds a kiloAgent from a config.ModelRef. Binary falls back to
// "kilo" when unset; Name is forwarded as KILOCODE_MODEL (kilo has no -m flag);
// Cmd is appended verbatim before the positional prompt.
func newKiloAgent(ref config.ModelRef) *kiloAgent {
	return &kiloAgent{
		binary: binaryOf(ref, "kilo"),
		model:  ref.Name,
		args:   ref.Cmd,
	}
}

func (a *kiloAgent) Provider() string { return "kilo" }

// Run executes `kilo run --auto <args…> <prompt>` with cmd.Dir=workdir and the
// configured model forwarded as KILOCODE_MODEL, retried on transient failure.
// The prompt is the final positional argument (kilo reads it from argv, not
// stdin). Setting cmd.Dir is the exec_test.go-equivalent contract: edits land on
// the isolated worktree.
func (a *kiloAgent) Run(ctx context.Context, req AgentRequest) (AgentResult, error) {
	model := req.Model
	if model == "" {
		model = a.model
	}
	out, u, err := runWithRetry(ctx, "kilo run",
		func(ctx context.Context) (string, string, error) {
			return a.runOnce(ctx, req.Workdir, model, req.Prompt)
		},
		isFatalKiloError)
	return AgentResult{Out: out, Usage: u}, err
}

// runOnce builds and executes a single `kilo run --auto` attempt. The model is
// forwarded as KILOCODE_MODEL on the child env (kilo has no -m flag); cmd.Dir is
// set to dir because kilo's workdir is the process cwd. The prompt is the last
// positional argument; stdin is left at the null device.
func (a *kiloAgent) runOnce(ctx context.Context, dir, model, prompt string) (string, string, error) {
	args := []string{"run", "--auto"}
	args = append(args, a.args...)
	args = append(args, prompt)
	cmd := exec.CommandContext(ctx, a.binary, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if model != "" {
		// Appended after os.Environ() so ref.Name takes precedence over an
		// ambient KILOCODE_MODEL (last entry wins); parent env inherited so
		// KILO_API_KEY auth and PATH survive.
		cmd.Env = append(os.Environ(), "KILOCODE_MODEL="+model)
	}
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err := cmd.Run()
	return out.String(), errBuf.String(), err
}

// fatalKiloSignals are lowercased substrings that mark a non-retryable kilo
// auth/credential failure. Mirrors fatalCodexSignals's role for codex. Note: a
// kilo timeout (exit 124) is intentionally NOT a fatal signal — it is retryable
// and rides the shared retry loop.
var fatalKiloSignals = []string{
	"authentication", "unauthorized", "not authorized",
	"401", "403",
	"api key", "api_key", "apikey",
	"credential", "not logged in", "login required", "please log in",
	"invalid api key", "missing api key",
}

// isFatalKiloError reports whether kilo's combined output looks like a
// non-retryable auth/credential failure (so ErrClaudeFatal short-circuits it).
func isFatalKiloError(stderr, stdout string) bool {
	s := strings.ToLower(stderr + " " + stdout)
	for _, sig := range fatalKiloSignals {
		if strings.Contains(s, sig) {
			return true
		}
	}
	return false
}

// Check is the doctor/preflight self-check: the kilo binary must be on PATH and
// at least one auth source must be in place — KILO_API_KEY / KILOCODE_API_KEY
// env. Returns a descriptive error otherwise. (Auth detection is best-effort;
// the hermetic contract test covers only the binary-missing path, which is what
// doctor gating relies on.)
func (a *kiloAgent) Check(ctx context.Context) error {
	if err := checkBinary(a.binary); err != nil {
		return err
	}
	for _, env := range []string{"KILO_API_KEY", "KILOCODE_API_KEY"} {
		if os.Getenv(env) != "" {
			return nil
		}
	}
	return fmt.Errorf("kilo: no auth — set KILO_API_KEY (or configure kilo provider auth)")
}

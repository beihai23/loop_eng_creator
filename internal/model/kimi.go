// kimi.go is the kimi (Kimi Code) provider implementation of Agent.
//
// Kimi Code (https://github.com/MoonshotAI/kimi-code) is Moonshot's coding agent.
// Its non-interactive entry point is `kimi -p`, which both disables the TUI AND
// routes tool calls through the `auto` strategy — i.e. -p alone yields the
// autonomous execution loop-eng's execute stage needs. The docs warn that -p's
// implicit auto does NOT stack with --yolo/--auto, so this adapter never adds
// them. The prompt is a POSITIONAL argument after -p; the model is selected
// with -m; the working directory is the process cwd (cmd.Dir). As with every
// provider, a single Run is a FRESH session, matching loop-eng's
// verification-independence assumption.
//
// Divergence note (issue risk section): kimi has NO headless --mcp-config flag
// (MCP is configured via config file / the TUI `/mcp` command). If loop-eng
// later needs to inject MCP at the shell-out, it must do so through kimi's
// config file, not a CLI flag.
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
	"path/filepath"
	"strings"

	"loop-eng/internal/config"
)

// kimiAgent is the kimi provider. binary/model/args come from config.ModelRef
// (Binary defaults to "kimi" via binaryOf; Name → -m; Cmd → native passthrough
// appended before the positional prompt).
type kimiAgent struct {
	binary string
	model  string
	args   []string
}

// newKimiAgent builds a kimiAgent from a config.ModelRef. Binary falls back to
// "kimi" when unset; Name is the -m model; Cmd is appended verbatim before the
// positional prompt (native passthrough for provider-specific flags).
func newKimiAgent(ref config.ModelRef) *kimiAgent {
	return &kimiAgent{
		binary: binaryOf(ref, "kimi"),
		model:  ref.Name,
		args:   ref.Cmd,
	}
}

func (a *kimiAgent) Provider() string { return "kimi" }

// Run executes `kimi -p [-m <model>] <args…> <prompt>` with cmd.Dir=workdir,
// retried on transient failure. The prompt is the final positional argument
// (kimi reads it from argv, not stdin). Setting cmd.Dir is the
// exec_test.go-equivalent contract: edits land on the isolated worktree.
func (a *kimiAgent) Run(ctx context.Context, req AgentRequest) (AgentResult, error) {
	model := req.Model
	if model == "" {
		model = a.model
	}
	out, u, err := runWithRetry(ctx, "kimi -p",
		func(ctx context.Context) (string, string, error) {
			return a.runOnce(ctx, req.Workdir, model, req.Prompt)
		},
		isFatalKimiError)
	return AgentResult{Out: out, Usage: u}, err
}

// runOnce builds and executes a single `kimi -p` attempt. -p is the
// non-interactive entry point and already implies autonomous tool use, so
// --auto/--yolo are NOT added (the docs say they don't stack with -p). The
// prompt is the last positional argument; stdin is left at the null device.
// cmd.Dir is set to dir because kimi's workdir is the process cwd.
func (a *kimiAgent) runOnce(ctx context.Context, dir, model, prompt string) (string, string, error) {
	args := []string{"-p"}
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

// fatalKimiSignals are lowercased substrings that mark a non-retryable kimi
// auth/credential failure. Mirrors fatalCodexSignals's role for codex.
var fatalKimiSignals = []string{
	"authentication", "unauthorized", "not authorized",
	"401", "403",
	"api key", "api_key", "apikey",
	"credential", "not logged in", "login required", "please log in",
	"invalid api key", "missing api key",
}

// isFatalKimiError reports whether kimi's combined output looks like a
// non-retryable auth/credential failure (so ErrClaudeFatal short-circuits it).
func isFatalKimiError(stderr, stdout string) bool {
	s := strings.ToLower(stderr + " " + stdout)
	for _, sig := range fatalKimiSignals {
		if strings.Contains(s, sig) {
			return true
		}
	}
	return false
}

// Check is the doctor/preflight self-check: the kimi binary must be on PATH and
// at least one auth source must be in place — MOONSHOT_API_KEY / KIMI_API_KEY
// env or a saved ~/.kimi login state from `kimi login`. Returns a descriptive
// error otherwise. (Auth detection is best-effort; the hermetic contract test
// covers only the binary-missing path, which is what doctor gating relies on.)
func (a *kimiAgent) Check(ctx context.Context) error {
	if err := checkBinary(a.binary); err != nil {
		return err
	}
	for _, env := range []string{"MOONSHOT_API_KEY", "KIMI_API_KEY"} {
		if os.Getenv(env) != "" {
			return nil
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		if _, statErr := os.Stat(filepath.Join(home, ".kimi")); statErr == nil {
			return nil
		}
	}
	return fmt.Errorf("kimi: no auth — set MOONSHOT_API_KEY/KIMI_API_KEY or run `kimi login`")
}

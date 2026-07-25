package model

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os/exec"
	"strings"
	"time"
)

// claudeRetry is the max number of attempts callWithRetry makes per Call/Exec.
// The claude -p CLI intermittently exits 1 (rate-limit / gateway 5xx / timeout
// / opaque flake), so a single attempt is too brittle for an autonomous loop.
const claudeRetry = 3

// claudeBackoffBase is the base delay for exponential backoff between retries:
// claudeBackoffBase, 2x, 4x (30s, 60s, 120s by default). A small base (1s) does
// NOT give an overloaded inference gateway time to recover — GLM 529 "该模型
// 访问量过大" needs tens of seconds, not 1s. 30s does. It is a package var so
// tests can zero it for speed.
var claudeBackoffBase = 30 * time.Second

// ErrClaudeFatal marks a NON-retryable claude error (auth/credential — HTTP
// 401/403, expired token, not-logged-in). SubLoop treats errors wrapping this
// as terminal: abort the task instead of burning budget retrying an expired
// credential. Retryable flakes (rate-limit, 5xx, timeout, opaque exit-1) are
// NOT fatal and do retry with backoff.
var ErrClaudeFatal = errors.New("claude: fatal (non-retryable) error")

// ClaudeClient shells out to the `claude` CLI in `-p` (print) mode for the
// plan/execute/verify stages. Each Call/Exec is a FRESH `claude -p` session —
// no conversation state is shared between calls, which is required for
// verification independence.
//
// Model, if non-empty, is passed as --model <name> for this role (overriding
// claude's default). Config-driven opt-in (cfg.Models.<role>.Name); empty =
// default model. All via `claude -p`; no SDK, no API key.
type ClaudeClient struct {
	Binary string   // path to the claude executable (e.g. "claude")
	Model  string   // if set, passed as --model (e.g. "haiku"); empty = default model
	Args   []string // extra fixed flags appended after -p (and --model)
}

// NewClaudeClient returns a ClaudeClient backed by the given binary, using
// model ("" = default) and extraArgs (e.g. --dangerously-skip-permissions).
func NewClaudeClient(binary, model string, extraArgs []string) *ClaudeClient {
	return &ClaudeClient{Binary: binary, Model: model, Args: extraArgs}
}

// fatalClaudeSignals are lowercased substrings in claude's output that indicate
// a non-retryable auth/credential error. Everything else (rate-limit / 429,
// gateway 5xx, timeout, opaque exit-1 with empty stderr) is a retryable flake.
var fatalClaudeSignals = []string{
	"authentication", "unauthorized", "not authorized",
	"401", "403",
	"api key", "api_key", "apikey",
	"credential", "not logged in", "login required", "please log in", "please run /login",
}

// isFatalClaudeError reports whether claude's combined output looks like a
// non-retryable auth/credential failure.
func isFatalClaudeError(stderr, stdout string) bool {
	s := strings.ToLower(stderr + " " + stdout)
	for _, sig := range fatalClaudeSignals {
		if strings.Contains(s, sig) {
			return true
		}
	}
	return false
}

// runOnce executes a single `claude -p [--model M] <Args>` attempt with prompt
// on stdin. dir=="" leaves the default working directory; non-empty sets
// cmd.Dir (Exec). model overrides c.Model for this attempt only (the Agent
// layer passes req.Model; "" falls back to the client's configured Model).
// Returns stdout, stderr, error.
func (c *ClaudeClient) runOnce(ctx context.Context, dir, model, prompt string) (string, string, error) {
	args := []string{"-p"}
	if model == "" {
		model = c.Model
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	args = append(args, c.Args...)
	cmd := exec.CommandContext(ctx, c.Binary, args...)
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

// runWithRetry is the shared retry runner for every shell-out provider. It runs
// attempt up to claudeRetry times with exponential backoff
// (claudeBackoffBase, 2x, 4x) + ±200ms jitter. isFatal(short) aborts at once,
// wrapping the error with ErrClaudeFatal so SubLoop's fatal short-circuit fires
// (auth/credential failures must not burn budget retrying). label prefixes the
// exhausted-retries error (e.g. "claude -p", "codex exec") for debuggability.
//
// The final error carries BOTH stderr AND stdout — headless agents often print
// their real error to stdout, which a discard-on-error caller would lose.
//
// This is the provider-common extraction of ClaudeClient.callWithRetry; the
// 30/60/120s + ±200ms jitter + fatal-short-circuit semantics are preserved so
// retry_test.go's three contracts (exhaust-retries / recover / fatal-abort) stay
// green for the claude path, and codex (and future providers) inherit the same
// retry behavior + ErrClaudeFatal wiring.
func runWithRetry(
	ctx context.Context,
	label string,
	attempt func(context.Context) (stdout, stderr string, err error),
	isFatal func(stderr, stdout string) bool,
) (string, Usage, error) {
	var lastOut, lastErrBuf string
	var lastErr error
	for n := 1; n <= claudeRetry; n++ {
		out, errBuf, err := attempt(ctx)
		if err == nil {
			return out, Usage{TokensOut: len(out)}, nil
		}
		lastOut, lastErrBuf, lastErr = out, errBuf, err
		if isFatal != nil && isFatal(lastErrBuf, lastOut) {
			return lastOut, Usage{}, fmt.Errorf("%w: %v (stderr: %q stdout: %.400q)",
				ErrClaudeFatal, lastErr, lastErrBuf, lastOut)
		}
		if n < claudeRetry {
			backoff := claudeBackoffBase << (n - 1) // 30s, 60s, 120s
			jitter := time.Duration(rand.Intn(400)-200) * time.Millisecond
			time.Sleep(backoff + jitter)
		}
	}
	return lastOut, Usage{}, fmt.Errorf("%s: %w after %d attempts (stderr: %q stdout: %.400q)",
		label, lastErr, claudeRetry, lastErrBuf, lastOut)
}

// callWithRetry runs runOnce up to claudeRetry times via the shared runWithRetry
// runner. Retry policy is documented on runWithRetry; isFatalClaudeError supplies
// the claude auth/credential signal set.
func (c *ClaudeClient) callWithRetry(ctx context.Context, dir, model, prompt string) (string, Usage, error) {
	return runWithRetry(ctx, "claude -p",
		func(ctx context.Context) (string, string, error) { return c.runOnce(ctx, dir, model, prompt) },
		isFatalClaudeError)
}

// Call implements Client by running `claude -p [--model M] <Args>` (default
// working directory) with the prompt on stdin, retried on retryable failure.
func (c *ClaudeClient) Call(ctx context.Context, prompt string) (string, Usage, error) {
	return c.callWithRetry(ctx, "", c.Model, prompt)
}

// CallIn implements DirClient: like Call but with cmd.Dir=dir (plan inside the
// attempt worktree). Retried on retryable failure, same as Call/Exec.
func (c *ClaudeClient) CallIn(ctx context.Context, dir, prompt string) (string, Usage, error) {
	return c.callWithRetry(ctx, dir, c.Model, prompt)
}

// Exec implements Executer: `claude -p [--model M] <Args>` with cmd.Dir=worktreeDir
// so the agent's edits land on the isolated worktree. Retried on retryable failure.
func (c *ClaudeClient) Exec(ctx context.Context, worktreeDir, prompt string) (string, Usage, error) {
	return c.callWithRetry(ctx, worktreeDir, c.Model, prompt)
}

// claudeAgent is the claude provider implementation of Agent. It wraps a
// ClaudeClient (the legacy shell-out), so NewClaudeClient / Call / Exec /
// ErrClaudeFatal and friends stay exactly as exec_test.go / retry_test.go /
// subloop.go use them — claudeAgent is a thin Agent view, not a replacement.
type claudeAgent struct{ c *ClaudeClient }

func (a *claudeAgent) Provider() string { return "claude" }

// Run delegates to ClaudeClient.callWithRetry. req.Model overrides the client's
// configured Model when set (the step-level agent hint path, P5); otherwise the
// role's configured model is used.
func (a *claudeAgent) Run(ctx context.Context, req AgentRequest) (AgentResult, error) {
	out, u, err := a.c.callWithRetry(ctx, req.Workdir, req.Model, req.Prompt)
	return AgentResult{Out: out, Usage: u}, err
}

// Check verifies the claude binary is resolvable (doctor / preflight). Auth is
// the Claude Code login (out-of-band); binary presence is the machine-checkable
// half.
func (a *claudeAgent) Check(ctx context.Context) error {
	return checkBinary(a.c.Binary)
}

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
type ClaudeClient struct {
	Binary string   // path to the claude executable (e.g. "claude")
	Args   []string // extra fixed flags appended after "-p"
}

func NewClaudeClient(binary string, extraArgs []string) *ClaudeClient {
	return &ClaudeClient{Binary: binary, Args: extraArgs}
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

// runOnce executes a single `claude -p <Args>` attempt with prompt on stdin.
// dir=="" leaves the default working directory; non-empty sets cmd.Dir (Exec).
// Returns stdout, stderr, error.
func (c *ClaudeClient) runOnce(ctx context.Context, dir, prompt string) (string, string, error) {
	args := append([]string{"-p"}, c.Args...)
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

// callWithRetry runs runOnce up to claudeRetry times. Retry policy:
//   - FATAL error (auth/credential, per fatalClaudeSignals) → abort at once,
//     wrapped with ErrClaudeFatal so SubLoop gives up rather than retrying.
//   - Retryable flake (rate-limit / 5xx / timeout / opaque exit-1) → retry with
//     exponential backoff (claudeBackoffBase, 2x, 4x) + ±200ms jitter, so
//     concurrent flaked tasks don't all retry on the same tick.
//
// The final error carries BOTH stderr AND stdout — claude often prints its real
// error to stdout, which a discard-on-error caller would lose.
func (c *ClaudeClient) callWithRetry(ctx context.Context, dir, prompt string) (string, Usage, error) {
	var lastOut, lastErrBuf string
	var lastErr error
	for attempt := 1; attempt <= claudeRetry; attempt++ {
		out, errBuf, err := c.runOnce(ctx, dir, prompt)
		if err == nil {
			return out, Usage{TokensOut: len(out)}, nil
		}
		lastOut, lastErrBuf, lastErr = out, errBuf, err
		if isFatalClaudeError(lastErrBuf, lastOut) {
			return lastOut, Usage{}, fmt.Errorf("%w: %v (stderr: %q stdout: %.400q)",
				ErrClaudeFatal, lastErr, lastErrBuf, lastOut)
		}
		if attempt < claudeRetry {
			backoff := claudeBackoffBase << (attempt - 1) // 30s, 60s, 120s
			jitter := time.Duration(rand.Intn(400)-200) * time.Millisecond
			time.Sleep(backoff + jitter)
		}
	}
	return lastOut, Usage{}, fmt.Errorf("claude -p: %w after %d attempts (stderr: %q stdout: %.400q)",
		lastErr, claudeRetry, lastErrBuf, lastOut)
}

// Call implements Client by running `claude -p <Args>` (default working
// directory) with the prompt on stdin, retried on retryable failure.
func (c *ClaudeClient) Call(ctx context.Context, prompt string) (string, Usage, error) {
	return c.callWithRetry(ctx, "", prompt)
}

// Exec implements Executer: `claude -p <Args>` with cmd.Dir=worktreeDir so the
// agent's edits land on the isolated worktree. Retried on retryable failure.
func (c *ClaudeClient) Exec(ctx context.Context, worktreeDir, prompt string) (string, Usage, error) {
	return c.callWithRetry(ctx, worktreeDir, prompt)
}

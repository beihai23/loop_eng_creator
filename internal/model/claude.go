package model

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// claudeRetry is the number of attempts callWithRetry makes per Call/Exec. The
// claude -p CLI intermittently exits 1 (transient network/rate-limit, often
// with empty stderr), so a single attempt is too brittle for an autonomous
// loop. Found by the M2 Phase-2 bootstrap (UpdateStatus probe blocked when
// claude -p flaked on plan/verify).
const claudeRetry = 3

// ClaudeClient shells out to the `claude` CLI in `-p` (print) mode for the
// plan/execute/verify stages. Each Call/Exec is a FRESH `claude -p` session —
// no conversation state is shared between calls, which is required for
// verification independence (a verify call must not inherit the execute
// session's memory).
type ClaudeClient struct {
	Binary string   // path to the claude executable (e.g. "claude")
	Args   []string // extra fixed flags appended after "-p"
}

// NewClaudeClient returns a ClaudeClient backed by the given binary path;
// extraArgs are passed verbatim after "-p" on every Call/Exec.
func NewClaudeClient(binary string, extraArgs []string) *ClaudeClient {
	return &ClaudeClient{Binary: binary, Args: extraArgs}
}

// runOnce executes a single `claude -p <Args>` attempt with prompt on stdin.
// dir=="" leaves the default working directory; non-empty sets cmd.Dir (used by
// Exec to run inside the worktree). Returns stdout, stderr, error.
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

// callWithRetry runs runOnce up to claudeRetry times, returning the first
// success. claude -p intermittently exits 1 (transient); retrying makes a
// single flake non-fatal. The final error carries BOTH stderr AND stdout —
// claude often prints its real error message to stdout, which a
// discard-on-error caller would lose (leaving an opaque "exit status 1: ").
func (c *ClaudeClient) callWithRetry(ctx context.Context, dir, prompt string) (string, Usage, error) {
	var lastOut, lastErrBuf string
	var lastErr error
	for attempt := 1; attempt <= claudeRetry; attempt++ {
		out, errBuf, err := c.runOnce(ctx, dir, prompt)
		if err == nil {
			return out, Usage{TokensOut: len(out)}, nil
		}
		lastOut, lastErrBuf, lastErr = out, errBuf, err
	}
	return lastOut, Usage{}, fmt.Errorf("claude -p: %w (stderr: %q stdout: %.400q)", lastErr, lastErrBuf, lastOut)
}

// Call implements Client by running `claude -p <Args>` (default working
// directory) with the prompt on stdin, retried on transient failure.
func (c *ClaudeClient) Call(ctx context.Context, prompt string) (string, Usage, error) {
	return c.callWithRetry(ctx, "", prompt)
}

// Exec implements Executer: `claude -p <Args>` with cmd.Dir=worktreeDir so the
// agent's edits land on the isolated worktree. Retried on transient failure.
func (c *ClaudeClient) Exec(ctx context.Context, worktreeDir, prompt string) (string, Usage, error) {
	return c.callWithRetry(ctx, worktreeDir, prompt)
}

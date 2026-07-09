package model

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// ClaudeClient shells out to the `claude` CLI in `-p` (print) mode for the
// execute and verify stages. Each Call spawns a FRESH `claude -p` session —
// no conversation state is shared between calls, which is required for
// verification independence (a verify call must not inherit the execute
// session's memory).
type ClaudeClient struct {
	Binary string   // path to the claude executable (e.g. "claude")
	Args   []string // extra fixed flags appended after "-p"
}

// NewClaudeClient returns a ClaudeClient backed by the given binary path;
// extraArgs are passed verbatim after "-p" on every Call.
func NewClaudeClient(binary string, extraArgs []string) *ClaudeClient {
	return &ClaudeClient{Binary: binary, Args: extraArgs}
}

// Call implements Client by running `claude -p <extraArgs>` with the prompt
// fed on stdin. The claude CLI does not reliably report token usage, so
// TokensOut is approximated from the byte length of stdout; TokensIn is left
// zero. Real accounting is surfaced in the run report as an estimate.
func (c *ClaudeClient) Call(ctx context.Context, prompt string) (string, Usage, error) {
	args := append([]string{"-p"}, c.Args...)
	cmd := exec.CommandContext(ctx, c.Binary, args...)
	cmd.Stdin = strings.NewReader(prompt)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return "", Usage{}, fmt.Errorf("claude -p: %w: %s", err, errBuf.String())
	}
	return out.String(), Usage{TokensOut: out.Len()}, nil
}

// Exec runs `claude -p <Args>` with the process working directory set to
// worktreeDir and the prompt on stdin. Args (e.g. --dangerously-skip-permissions)
// come from config so execute runs autonomously without permission stalls.
func (c *ClaudeClient) Exec(ctx context.Context, worktreeDir, prompt string) (string, Usage, error) {
	args := append([]string{"-p"}, c.Args...)
	cmd := exec.CommandContext(ctx, c.Binary, args...)
	cmd.Dir = worktreeDir
	cmd.Stdin = strings.NewReader(prompt)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return "", Usage{}, fmt.Errorf("claude -p (exec @ %s): %w: %s", worktreeDir, err, errBuf.String())
	}
	return out.String(), Usage{TokensOut: out.Len()}, nil
}

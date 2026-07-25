// Package model defines the LLM client abstraction used across loop-eng.
//
// The Client interface is FROZEN for the whole M1 plan; budget, skill, and
// subloop layers depend on it. Call always returns the full output string,
// token usage, and any error — there is no streaming surface.
package model

import "context"

// Usage reports token accounting for a single Call.
type Usage struct {
	TokensIn  int
	TokensOut int
}

// Client is the uniform LLM gateway used by triage, plan, execute, and verify.
// Call must be stateless across invocations unless the concrete implementation
// documents otherwise; each call carries its own context and prompt.
type Client interface {
	Call(ctx context.Context, prompt string) (output string, usage Usage, err error)
}

// Executer is the worktree-aware execute gateway. Execute must run the agent
// INSIDE the isolated worktree (cmd.Dir = worktreeDir) so its edits land on the
// worktree, not the base repo. Client.Call carries no directory, hence a
// separate interface (Client.Call stays frozen).
type Executer interface {
	Exec(ctx context.Context, worktreeDir, prompt string) (output string, usage Usage, err error)
}

// DirClient is an OPTIONAL extension of Client for callers that can supply a
// working directory (plan runs inside the attempt's worktree so its exploration
// — and any stray writes — land on the disposable worktree, not the base repo).
// Client.Call stays frozen: consumers type-assert to DirClient and fall back to
// Call when the concrete client does not implement it (e.g. test fakes).
type DirClient interface {
	Client
	CallIn(ctx context.Context, dir, prompt string) (output string, usage Usage, err error)
}

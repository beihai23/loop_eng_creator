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

// agent.go defines the provider-neutral coding-agent layer.
//
// loop-eng's triage/plan/execute/verify stages shell out to a headless coding
// agent CLI. Historically that was hard-wired to `claude -p` (ClaudeClient).
// This file introduces an Agent abstraction so the shell-out target is chosen by
// config (config.ModelRef.Provider): claude today, codex/opencode/kimi/kilo
// tomorrow — each a thin adapter, dispatched by NewAgent (spec §8.10: the
// shell-out is provider-neutral; swapping the binary/agent is a config change).
//
// The frozen Client / Executer interfaces (model.go) are how the rest of
// loop-eng consumes an LLM. Agent is the richer internal contract (it knows its
// provider name, can self-check, and carries a structured AgentResult). The
// AsClient / AsExecuter adapters bridge an Agent onto the frozen interfaces, so
// budget / skill / subloop wiring stays untouched (Client.Call signature,
// Executer.Exec signature, subloop_test.go — all unchanged).

package model

import (
	"context"
	"fmt"
	"os/exec"

	"loop-eng/internal/config"
)

// AgentRequest is the provider-neutral input to a single headless agent call.
type AgentRequest struct {
	Workdir string // cmd.Dir for the agent process ("" = default cwd; Executer sets the worktree)
	Prompt  string // the task prompt, delivered per-provider (stdin / positional)
	Model   string // model override (e.g. "--model haiku"); "" = the agent's configured default
}

// AgentResult is the provider-neutral output of a single headless agent call.
// Out is the final stdout text (the product the loop consumes); Usage carries
// token accounting (best-effort — adapters fall back to len(Out) when a
// provider's structured usage can't be parsed).
type AgentResult struct {
	Out       string
	Usage     Usage
	ExitCode  int
	SessionID string // resumable session / audit id when the provider exposes one
}

// Agent is the provider-neutral coding-agent contract. Each provider (claude,
// codex, …) implements it; NewAgent dispatches by config.ModelRef.Provider.
//
// The dispatch method is named Run (not Exec) to avoid a signature clash with
// Executer.Exec(ctx, workdir, prompt) on a struct that may embed both — Go has
// no overloading, so the two must differ in name.
type Agent interface {
	// Provider returns the provider key ("claude", "codex", …) for observability
	// (trace model_ref) and doctor dispatch.
	Provider() string
	// Run executes one headless agent call, retried on transient failure.
	Run(ctx context.Context, req AgentRequest) (AgentResult, error)
	// Check is the doctor/preflight self-check: is the provider's binary on PATH
	// and is its auth in place? Returns nil when ready, a descriptive error otherwise.
	Check(ctx context.Context) error
}

// NewAgent dispatches an Agent by ref.Provider. "" and "claude" both resolve to
// the claude provider so the default config (no provider field) keeps today's
// `claude -p` out-of-box behavior. An unknown provider returns an error —
// callers (buildModels) surface it rather than silently falling back.
func NewAgent(ref config.ModelRef) (Agent, error) {
	switch p := ref.Provider; p {
	case "", "claude":
		return &claudeAgent{c: NewClaudeClient(binaryOf(ref, "claude"), ref.Name, ref.Cmd)}, nil
	case "codex":
		return newCodexAgent(ref), nil
	default:
		return nil, fmt.Errorf("model: unknown provider %q (want one of claude, codex)", p)
	}
}

// binaryOf returns ref.Binary when set, else the provider's conventional default
// (so a config that only sets `provider: codex` still finds the `codex` binary).
func binaryOf(ref config.ModelRef, def string) string {
	if ref.Binary != "" {
		return ref.Binary
	}
	return def
}

// checkBinary is the shared binary half of Agent.Check: it verifies the
// provider's executable is resolvable. Absolute paths are checked directly;
// bare names are resolved via PATH. Returns a descriptive error when missing.
func checkBinary(binary string) error {
	if binary == "" {
		return fmt.Errorf("agent: binary not configured")
	}
	if _, err := exec.LookPath(binary); err != nil {
		return fmt.Errorf("agent: binary %q not found: %w", binary, err)
	}
	return nil
}

// agentClient adapts an Agent onto the frozen Client interface (Call with no
// directory). Used for plan / triage / verify (the non-worktree roles).
type agentClient struct{ a Agent }

func (c *agentClient) Call(ctx context.Context, prompt string) (string, Usage, error) {
	r, err := c.a.Run(ctx, AgentRequest{Prompt: prompt})
	return r.Out, r.Usage, err
}

// CallIn implements DirClient: like Call but with AgentRequest.Workdir=dir
// (plan inside the attempt worktree). Provider-neutral — each adapter maps
// Workdir onto its native mechanism (claude: cmd.Dir; codex: --cd + cmd.Dir).
func (c *agentClient) CallIn(ctx context.Context, dir, prompt string) (string, Usage, error) {
	r, err := c.a.Run(ctx, AgentRequest{Workdir: dir, Prompt: prompt})
	return r.Out, r.Usage, err
}

// AsClient returns a Client view over a (Call without directory).
func AsClient(a Agent) Client { return &agentClient{a: a} }

// agentExecuter adapts an Agent onto the frozen Executer interface (Exec with a
// worktree directory). Used for the execute role, whose edits must land on the
// isolated worktree (cmd.Dir = worktreeDir).
type agentExecuter struct{ a Agent }

func (e *agentExecuter) Exec(ctx context.Context, worktreeDir, prompt string) (string, Usage, error) {
	r, err := e.a.Run(ctx, AgentRequest{Workdir: worktreeDir, Prompt: prompt})
	return r.Out, r.Usage, err
}

// AsExecuter returns an Executer view over a (Exec in a worktree).
func AsExecuter(a Agent) Executer { return &agentExecuter{a: a} }

// Compile-time guards: the adapters satisfy the frozen interfaces.
var (
	_ Client    = (*agentClient)(nil)
	_ DirClient = (*agentClient)(nil)
	_ Executer  = (*agentExecuter)(nil)
)

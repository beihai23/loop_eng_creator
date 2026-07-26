// agent.go defines the provider-neutral coding-agent layer.
//
// loop-eng's triage/plan/execute/verify stages shell out to a headless coding
// agent CLI. Historically that was hard-wired to `claude -p` (ClaudeClient).
// This file introduces an Agent abstraction so the shell-out target is chosen by
// config (config.ModelRef.Provider): claude, codex, opencode, kimi, kilo — each
// a thin adapter, dispatched by NewAgent (spec §8.10: the shell-out is
// provider-neutral; swapping the binary/agent is a config change).
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
	"slices"
	"sort"
	"strings"

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

// ProviderFactory builds an Agent for one provider key. Each entry in Providers
// is a factory that knows how to construct its provider's adapter from a
// ModelRef. This is the single source of truth for the provider set — NewAgent
// dispatch, config validation (ValidateProviders), and the interactive config
// menu all read Providers, so adding a provider is a one-line map edit (spec
// §8.10 — the shell-out is provider-neutral).
type ProviderFactory func(config.ModelRef) Agent

// Providers is the registry of every supported coding-agent provider. The
// default ("") provider is NOT a key here — ResolveProvider maps "" → "claude"
// before lookup, so the registry holds only explicit providers and a config
// with no provider field keeps today's `claude -p` out-of-box behavior.
var Providers = map[string]ProviderFactory{
	"claude": func(ref config.ModelRef) Agent {
		args := ref.Cmd
		if ref.ReadOnly {
			args = claudeReadOnlyProfile(args)
		}
		return &claudeAgent{c: NewClaudeClient(binaryOf(ref, "claude"), ref.Name, args)}
	},
	"codex":    func(ref config.ModelRef) Agent { return newCodexAgent(ref) },
	"opencode": func(ref config.ModelRef) Agent { return newOpencodeAgent(ref) },
	"kimi":     func(ref config.ModelRef) Agent { return newKimiAgent(ref) },
	"kilo":     func(ref config.ModelRef) Agent { return newKiloAgent(ref) },
}

// ResolveProvider normalizes a provider key: "" → "claude" (the out-of-box
// default; a config that sets no provider field keeps `claude -p` behavior).
// Every other value is returned as-is — validity is checked against Providers
// by the caller (NewAgent / ValidateProviders).
func ResolveProvider(p string) string {
	if p == "" {
		return "claude"
	}
	return p
}

// RegisteredProviders returns the sorted list of registered provider keys — the
// single source of truth consumed by NewAgent's error message, ValidateProviders,
// and the interactive config menu. Sorted so output (errors, menus) is stable.
func RegisteredProviders() []string {
	names := make([]string, 0, len(Providers))
	for k := range Providers {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// NewAgent dispatches an Agent by ref.Provider via the Providers registry. ""
// resolves to claude (ResolveProvider), so the default config (no provider
// field) keeps today's `claude -p` out-of-box behavior. An unknown provider
// returns an error carrying the full valid set — callers (buildModels /
// ValidateProviders / doctor) surface it rather than silently falling back.
func NewAgent(ref config.ModelRef) (Agent, error) {
	f, ok := Providers[ResolveProvider(ref.Provider)]
	if !ok {
		return nil, fmt.Errorf("model: unknown provider %q (want one of: %s)", ref.Provider, strings.Join(RegisteredProviders(), ", "))
	}
	return f(ref), nil
}

// ValidateProviders checks every model role's Provider against the Providers
// registry. It is the single config-side validation entry shared by the CLI
// config-load path (loadConfig) and doctor — the same registry NewAgent
// dispatches on — so an unknown provider fails fast at load time instead of
// surfacing mid-run when NewAgent shells out. It deliberately does NOT call
// config.validate (the model package must not depend on config's internals, and
// a Config that fails other validation — e.g. a zero Budget — should still be
// able to report a bad provider). Empty/claude and every registered provider
// validate clean; anything else yields a role-named error carrying the valid set.
func ValidateProviders(cfg *config.Config) error {
	for _, r := range []struct {
		role string
		ref  config.ModelRef
	}{
		{"triage", cfg.Models.Triage},
		{"plan", cfg.Models.Plan},
		{"execute", cfg.Models.Execute},
		{"verify", cfg.Models.Verify},
	} {
		if _, ok := Providers[ResolveProvider(r.ref.Provider)]; !ok {
			return fmt.Errorf("models.%s: unknown provider %q (want one of: %s)", r.role, r.ref.Provider, strings.Join(RegisteredProviders(), ", "))
		}
	}
	return nil
}

// binaryOf returns ref.Binary when set, else the provider's conventional default
// (so a config that only sets `provider: codex` still finds the `codex` binary).
func binaryOf(ref config.ModelRef, def string) string {
	if ref.Binary != "" {
		return ref.Binary
	}
	return def
}

// claudeReadOnlyProfile transforms a claude argv into an airtight read-only
// profile, so a ReadOnly role (triage/plan/verify) physically cannot modify the
// main repo — not even via Bash (`echo > file`, `sed -i`, or an out-of-repo
// absolute path write). This is the hard guarantee that complements worktree
// isolation (worktree contains in-repo writes; this blocks writes outright).
//
// It strips:
//   - --dangerously-skip-permissions (=bypassPermissions). bypass is mutually
//     exclusive with plan mode, and only one of the two can win — read-only wins,
//     so bypass is removed even if a user mis-configured it back in.
//   - any prior --permission-mode <x> and --permission-mode=<x> (the space form's
//     value is dropped together with the flag), so our injection is authoritative.
//
// Then it appends --permission-mode plan (Bash, Edit, Write, … all run read-only
// at the permission layer — verified against real claude: read-only Bash returns
// normally; `touch /tmp/x` is rejected and the file is not created). As
// defense-in-depth against plan-mode tool-set drift, when cmd carries no
// --disallowedTools it also appends --disallowedTools Edit Write NotebookEdit.
func claudeReadOnlyProfile(cmd []string) []string {
	out := make([]string, 0, len(cmd)+8)
	for i := 0; i < len(cmd); i++ {
		switch a := cmd[i]; {
		case a == "--dangerously-skip-permissions":
			continue // bypass: mutually exclusive with plan mode; read-only wins
		case a == "--permission-mode":
			if i+1 < len(cmd) {
				i++ // drop the separate value too
			}
			continue
		case strings.HasPrefix(a, "--permission-mode="):
			continue // drop the combined form
		default:
			out = append(out, a)
		}
	}
	out = append(out, "--permission-mode", "plan")
	if !slices.Contains(out, "--disallowedTools") {
		out = append(out, "--disallowedTools", "Edit", "Write", "NotebookEdit")
	}
	return out
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

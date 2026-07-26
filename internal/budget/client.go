package budget

import (
	"context"

	"loop-eng/internal/model"
)

// Client is a model.Client decorator that funnels every Call through an
// Enforcer. It exists to close the verify-budget gap carried over from Task 12:
// verify.Chain's Tier interface is frozen (takes no Enforcer), so the verify
// skill's LLM Call — invoked inside Chain → verify.LLM.Check → skill.Run →
// Model.Call — used to bypass the budget entirely, deviating from spec §8.8
// ("每次模型调用前强制").
//
// Wiring (Task 14 run-once): the verify skill's Model is wrapped by this
// decorator sharing the SAME *Enforcer handed to SubLoop.Budget. plan and
// execute keep using the raw client — SubLoop already budget-wraps those two
// manually (Task 12), so wrapping them again here would double-count.
// Net: plan + execute + verify all budgeted, no double-count. Full unification
// (single choke point, removing SubLoop's manual calls) is deferred to M3.
//
// Estimate policy: BeforeCall uses Enf.PerCall as the pre-check estimate.
// PerCall is the configured upper bound for a single LLM call, so it is the
// largest estimate the Enforcer will admit — using it guarantees the verify
// Call is subject to BOTH the per-call cap (estimate > PerCall is impossible
// by construction) AND the per-task running tally (spent + PerCall must fit
// under PerTask). Actual tokens are accounted in AfterCall from the real
// Usage returned by Base, so the tally converges on truth after each Call.
type Client struct {
	Base model.Client
	Enf  *Enforcer
}

// Call implements model.Client. It runs the budget pre-check, delegates to
// Base, then accounts the real usage. On pre-check failure it returns the
// Enforcer error WITHOUT calling Base (the LLM call is never made).
func (c *Client) Call(ctx context.Context, prompt string) (string, model.Usage, error) {
	if err := c.Enf.BeforeCall(c.Enf.PerCall); err != nil {
		return "", model.Usage{}, err
	}
	out, u, err := c.Base.Call(ctx, prompt)
	c.Enf.AfterCall(u)
	return out, u, err
}

// CallIn implements model.DirClient, making *budget.Client transparent to the
// dir-binding machinery (skill.RunIn → DirClient.CallIn). This is the
// production-load-bearing piece of the #81 fix: the verify skill's Model is
// ALWAYS *budget.Client in the assembled SubLoop (run-once, daemon, and the
// subloop agent-hint override all wrap the agent this way), so unless
// *budget.Client implements DirClient, RunIn's `Model.(model.DirClient)`
// assertion fails and SILENTLY degrades to Call — the model call then runs in
// the daemon's cwd (the clean base repo), and an agentic verify agent's
// ground-check sees the wrong tree (#81's false "主仓库无此文件" rejections).
// Setting LLM.Dir alone is not enough; this method makes the chain hold in
// production, not just in tests using a bare DirClient fake.
//
// Semantics mirror Call exactly: BeforeCall(PerCall) budget pre-check (rejects
// WITHOUT calling Base when over budget), delegate to Base — CallIn when Base is
// a DirClient and dir is non-empty (the production agentClient →
// AgentRequest.Workdir → adapter cmd.Dir=dir path), else plain Call (test fakes,
// clients without dir support, empty dir) — then AfterCall the real usage so the
// tally still converges on truth.
func (c *Client) CallIn(ctx context.Context, dir, prompt string) (string, model.Usage, error) {
	if err := c.Enf.BeforeCall(c.Enf.PerCall); err != nil {
		return "", model.Usage{}, err
	}
	if dc, ok := c.Base.(model.DirClient); ok && dir != "" {
		out, u, err := dc.CallIn(ctx, dir, prompt)
		c.Enf.AfterCall(u)
		return out, u, err
	}
	out, u, err := c.Base.Call(ctx, prompt)
	c.Enf.AfterCall(u)
	return out, u, err
}

// Compile-time guards: Client is a model.Client AND a model.DirClient.
var _ model.Client = (*Client)(nil)
var _ model.DirClient = (*Client)(nil)

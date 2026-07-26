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
// ("每次模型调用前强制"). The same decorator now backs triage and help too, so all
// five roles' Model calls are budgeted uniformly.
//
// Estimate policy: the pre-check estimate is Enf.Estimate(Role) — the role's
// last observed per-call usage × a safety factor, falling back to a per-role
// conservative floor on the first call. Unlike the old BeforeCall(PerCall) (an
// estimate that equaled the cap and so could never exceed it), this estimate
// tracks reality and CAN exceed PerCall, so the per-call brake actually fires.
// The failure direction is "block" (spec §8.8): when in doubt, refuse. After a
// Call the real usage is recorded against Role via Enf.Record, which both
// accrues the per-task tally and primes the next Estimate.
type Client struct {
	Base model.Client
	Enf  *Enforcer
	// Role is the budget role this Client speaks as ("verify" | "triage" |
	// "help"). It drives Estimate (per-role floor/history) and Record (per-role
	// last-usage). plan/execute are budgeted by SubLoop directly and do not go
	// through this decorator, so they have no Client/Role here.
	Role string
}

// Call implements model.Client. It runs the budget pre-check using the role's
// real estimate, delegates to Base, then records the real usage against Role.
// On pre-check failure it returns the Enforcer error WITHOUT calling Base (the
// LLM call is never made).
func (c *Client) Call(ctx context.Context, prompt string) (string, model.Usage, error) {
	est := c.Enf.Estimate(c.Role)
	if err := c.Enf.BeforeCall(est); err != nil {
		return "", model.Usage{}, err
	}
	out, u, err := c.Base.Call(ctx, prompt)
	c.Enf.Record(c.Role, u)
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
// Semantics mirror Call exactly: BeforeCall(Estimate(Role)) budget pre-check
// (rejects WITHOUT calling Base when over budget — the estimate now tracks the
// role's real last usage × safety, so it can exceed PerCall and the per-call
// brake fires for real), delegate to Base — CallIn when Base is a DirClient and
// dir is non-empty (the production agentClient → AgentRequest.Workdir → adapter
// cmd.Dir=dir path), else plain Call (test fakes, clients without dir support,
// empty dir) — then Record the real usage against Role so the tally accrues and
// the next Estimate reflects reality.
func (c *Client) CallIn(ctx context.Context, dir, prompt string) (string, model.Usage, error) {
	est := c.Enf.Estimate(c.Role)
	if err := c.Enf.BeforeCall(est); err != nil {
		return "", model.Usage{}, err
	}
	if dc, ok := c.Base.(model.DirClient); ok && dir != "" {
		out, u, err := dc.CallIn(ctx, dir, prompt)
		c.Enf.Record(c.Role, u)
		return out, u, err
	}
	out, u, err := c.Base.Call(ctx, prompt)
	c.Enf.Record(c.Role, u)
	return out, u, err
}

// Compile-time guards: Client is a model.Client AND a model.DirClient.
var _ model.Client = (*Client)(nil)
var _ model.DirClient = (*Client)(nil)

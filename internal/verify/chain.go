package verify

import "context"

// HumanStub: tier-3 自动通过占位。Check 返回 Passed=true、NeedsHuman=false——不触发
// needs-review（M3 的真 park 由注入 SubLoop.HumanTier 的真人审 tier 产 NeedsHuman）。
// 留在链尾只是为了让 Tier3Human=true 时 chain 有一道 tier-3 占位，结果仍取决于 tier1/tier2。
type HumanStub struct{}

func (HumanStub) Check(context.Context, string, []string, string) (VerifyResult, error) {
	return VerifyResult{Passed: true, NeedsHuman: false, Detail: "tier3 auto-pass placeholder"}, nil
}

// Chain 按 tier1→tier2→tier3 顺序跑；前一层不过即短路返回。
// 注意：tier3 若是 HumanStub（Passed=true、NeedsHuman=false），chain 结果取决于 tier1/tier2；
// 若是注入的真人审 tier 产 NeedsHuman=true，则 SubLoop 据此路由到 needs-review。
func Chain(ctx context.Context, tiers []Tier, diff string, criteria []string, priorFailure string) (VerifyResult, error) {
	var last VerifyResult
	for _, t := range tiers {
		r, err := t.Check(ctx, diff, criteria, priorFailure)
		if err != nil {
			return VerifyResult{}, err
		}
		last = r
		if !r.Passed {
			return r, nil // 短路
		}
	}
	return last, nil
}

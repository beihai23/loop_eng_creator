package verify

import "context"

// HumanStub: M1 占位 tier3。Check 返回 NeedsHuman=true，但在 Chain 里不阻断（M3 才真 park）。
type HumanStub struct{}

func (HumanStub) Check(context.Context, string, []string, string) (VerifyResult, error) {
	return VerifyResult{Passed: true, NeedsHuman: true, Detail: "tier3 stub (M3 接 issue 评论)"}, nil
}

// Chain 按 tier1→tier2→tier3 顺序跑；前一层不过即短路返回。
// 注意：M1 里 tier3 是 HumanStub（Passed=true），故 chain 结果取决于 tier1/tier2。
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

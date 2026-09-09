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
//
// ho 是 agent→人的移交包（Handoff）：对实现 HandoffAware 的 tier（tier-3）在
// Check 前注入，TierOutcomes 附带本轮已跑过的 tier 结果快照。tier-1/2 不实现该
// 接口，不受影响；零值 Handoff = 无包（渲染器按空段省略）。
func Chain(ctx context.Context, tiers []Tier, diff string, criteria []string, priorFailure string, ho Handoff) (VerifyResult, error) {
	var last VerifyResult
	// outcomes 在循环外声明——last=r 会覆盖整个 struct（含 Tiers），故累积器
	// 必须独立于 last，否则每轮被清空。每次 append 后把 outcomes 挂回 last.Tiers，
	// 短路时随 last 返回，全过时也随最后一次 last 返回（spec §4.6 逐 tier 落盘）。
	var outcomes []TierOutcome
	for i, t := range tiers {
		// 移交包注入（快照语义，非增量）：tier-3 拿到的是「此刻机器已验过什么」
		// 的完整判词。每 tier 前重注——上一次注入的 outcomes 已过时。
		if ha, ok := t.(HandoffAware); ok {
			p := ho
			p.TierOutcomes = append([]TierOutcome(nil), outcomes...)
			ha.SetHandoff(p)
		}
		r, err := t.Check(ctx, diff, criteria, priorFailure)
		if err != nil {
			return VerifyResult{}, err
		}
		outcomes = append(outcomes, TierOutcome{
			Tier: i + 1, Passed: r.Passed, NeedsHuman: r.NeedsHuman, Detail: r.Detail,
		})
		last = r
		last.Tiers = outcomes
		if !r.Passed {
			return last, nil // 短路：未到的 tier 不进 outcomes
		}
	}
	last.Tiers = outcomes
	return last, nil
}

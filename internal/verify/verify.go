package verify

import "context"

// TierOutcome 是 Chain 求值中单 tier 的结果，供逐 tier 落盘用（spec §4.6）。
// Tier 为 1-based；只有实际跑过的 tier 才会出现（Chain 首失败即短路，后面的 tier 不进此切片）。
type TierOutcome struct {
	Tier       int
	Passed     bool
	NeedsHuman bool
	Detail     string
}

type VerifyResult struct {
	Passed          bool
	Detail          string
	FailingCriteria []string
	NeedsHuman      bool
	Tiers           []TierOutcome // 逐 tier 求值结果（spec §4.6）
}

type Tier interface {
	Check(ctx context.Context, diff string, criteria []string, priorFailure string) (VerifyResult, error)
}

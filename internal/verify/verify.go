package verify

import (
	"context"

	"loop-eng/internal/skill"
)

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
	// FailureClasses 是 tier-2 的归因分类（M2 争议路由，可选）：nil = 模型未给
	// 或 tier-1 短路 → 全按 work 处理（现行为）。仅驳回时有意义。
	FailureClasses []skill.FailureClass
	NeedsHuman     bool
	Tiers          []TierOutcome // 逐 tier 求值结果（spec §4.6）
}

type Tier interface {
	Check(ctx context.Context, diff string, criteria []string, priorFailure string) (VerifyResult, error)
}

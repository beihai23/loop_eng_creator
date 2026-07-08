package verify

import "context"

type VerifyResult struct {
	Passed          bool
	Detail          string
	FailingCriteria []string
	NeedsHuman      bool
}

type Tier interface {
	Check(ctx context.Context, diff string, criteria []string, priorFailure string) (VerifyResult, error)
}

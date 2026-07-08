package verify

import (
	"context"

	"loop-eng/internal/skill"
)

// LLM 用一个 verify skill（其 Model 每次新会话）做语义验证。
// 新鲜上下文由每次 Check → skill.Run → model.Call（新会话）结构性保证，
// LLM 本身不持有执行态。
type LLM struct {
	Skill skill.Skill[skill.VerifyInput, skill.VerifyOutput]
}

func (l LLM) Check(ctx context.Context, diff string, criteria []string, priorFailure string) (VerifyResult, error) {
	out, _, err := l.Skill.Run(ctx, skill.VerifyInput{
		Diff:               diff,
		AcceptanceCriteria: criteria,
		PriorFailureSignal: priorFailure,
	})
	if err != nil {
		return VerifyResult{}, err
	}
	return VerifyResult{
		Passed:          out.Passed,
		Detail:          out.Reason,
		FailingCriteria: out.FailingCriteria,
	}, nil
}

package verify

import (
	"context"
	"strings"

	"loop-eng/internal/model"
	"loop-eng/internal/skill"
)

// LLM 用一个 verify skill（其 Model 每次新会话）做语义验证。
// 新鲜上下文由每次 Check → skill.Run → model.Call（新会话）结构性保证，
// LLM 本身不持有执行态。
type LLM struct {
	Skill skill.Skill[skill.VerifyInput, skill.VerifyOutput]
	// Dir 是 tier-2 模型调用的工作目录（attempt 的 worktree，由 tiersFor 注入）。
	// agentic 的 verify（如 kimi）会拿 diff 对照文件系统 ground-check——#81 的
	// 假驳回就是它看主仓库（HEAD 干净）而不是 worktree 所致。空 = 默认 cwd
	// （旧装配/纯测试），行为与引入前一致。Tier 接口签名不变。
	Dir string
	// Usage 是 tier-2 真实 token 的落库旁路：verify.Chain 的 Tier 接口是冻结契约
	// （Check 不带 usage 返回），故 LLM 用这个指针把 Skill.RunIn 返回的 model.Usage
	// 带回调用方（SubLoop 在 verify step 的 AppendStep 读它落 steps.tokens_in/out）。
	// 值接收者拷贝进 Chain 的 tiers 切片时共享同一指针，所以即便 Check 按值调用，
	// 写入也对调用方可见。nil（既有测试以 LLM{Skill:...} 裸构造时）→ 不写，绝不 panic。
	Usage *model.Usage
}

func (l LLM) Check(ctx context.Context, diff string, criteria []string, priorFailure string) (VerifyResult, error) {
	out, u, err := l.Skill.RunIn(ctx, skill.VerifyInput{
		Diff:               diff,
		AcceptanceCriteria: criteria,
		PriorFailureSignal: priorFailure,
	}, l.Dir)
	// 旁路写 usage 必须在 err 分支之前：parse/调用错误也消耗了 token，落库要反映真值。
	// nil 守卫是必须的——既有测试以 LLM{Skill:...}（Usage 为 nil）构造。
	if l.Usage != nil {
		*l.Usage = u
	}
	if err != nil {
		return VerifyResult{}, err
	}
	return VerifyResult{
		Passed:          out.Passed,
		Detail:          detailFor(out),
		FailingCriteria: out.FailingCriteria,
	}, nil
}

// detailFor 把 verify skill 的结构化输出（skill.VerifyOutput）落成 VerifyResult.Detail。
//
// 可观测性铁律：驳回（Passed=false）时 Detail 永不空。verify.md 的 prompt 已要求模型
// 驳回必给 reason，但 LLM 不可靠（偶尔 reason 留空——这就是 #10 黑箱的根因：trace 里
// 只剩 `passed=false detail=`，既无法 debug，下一轮重试的 priorFailure 也是空）。此处
// 兜底，确保驳回永远可解释、反馈永远可用：
//   - Passed=true：原样返回 Reason（通过是信息性的，可为空，不受兜底影响）。
//   - Passed=false + Reason 非空：用模型自述（最准，逐字保留）。
//   - Passed=false + Reason 空 + FailingCriteria 非空：把未满足标准拼成一行。
//   - 三者皆空：合成一条消息，标明模型未给理由（兜底的兜底）。
func detailFor(out skill.VerifyOutput) string {
	if out.Passed {
		return out.Reason
	}
	if strings.TrimSpace(out.Reason) != "" {
		return out.Reason
	}
	if fc := nonBlank(out.FailingCriteria); len(fc) > 0 {
		return strings.Join(fc, "; ")
	}
	return "verify rejected: LLM returned no reason and no failing_criteria (prompt requires both on reject); re-check each acceptance standard against the diff"
}

// nonBlank 丢弃空白条目，返回纯内容切片（failing_criteria 里可能混入空串）。
func nonBlank(fc []string) []string {
	clean := make([]string, 0, len(fc))
	for _, c := range fc {
		if strings.TrimSpace(c) != "" {
			clean = append(clean, c)
		}
	}
	return clean
}

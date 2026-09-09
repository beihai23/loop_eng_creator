package verify

// handoff.go —— agent→人的移交包（借鉴 Baton HandoffPackage 的字段映射）。
//
// tier-3 人审请求过去只有「验收标准 + 60 行 diff 摘要 + 一句泛泛的判断点」——
// 审的人拿不到独立证据就只能信摘要（认知投降的入口）。移交包把 loop-eng 在
// 机器通道上已达标的「现场级」交接标准带到 agent→人方向：接手的人据此可以直接
// 判断、复现、接受或驳回。
//
// 注入通道：Tier.Check 是冻结契约（见 llm.go / budget/client.go），移交包经
// HandoffAware 可选接口旁路注入，由 verify.Chain 在调用该 tier 前塞入（含本轮
// 已跑过的 tier 结果快照）。非 HandoffAware 的 tier（tier-1/2/测试 fake）不受影响。

// Handoff 是 agent→人的移交包。零值合法：字段为空时渲染器按段省略，评论仍有
// 标准 + diff + 判断点，绝不因缺包阻塞（旧调用方/测试直接构造 Human 的路径）。
type Handoff struct {
	// Attempt/MaxRetries：透明度——这是第几轮、共几轮。
	Attempt, MaxRetries int
	// Worktree/Branch：工作现场。park 前已由 subloop commit，分支在主仓库对象库，
	// worktree 被清理也不丢已人审的工作。
	Worktree, Branch string
	// RunHistory：该 issue 历轮 run 的结局摘要（DB 构建，有界）。
	RunHistory string
	// EnvNotes：execute 自报的「环境与复现」段（宽松提取；旧格式自报无此段为空）。
	EnvNotes string
	// Risks：plan 识别的风险（open threads：接手方必读）。
	Risks []string
	// CriteriaNotes：plan 修订验收标准的理由（未修订为空）。
	CriteriaNotes string
	// TierOutcomes：本轮 tier-1/2 的判词——「机器已验过什么」，人不必重做。
	// Chain 注入（tier-3 运行前的快照）。
	TierOutcomes []TierOutcome
	// TokensIn/TokensOut：本 run 累计 token（透明度：这份 review 烧了多少）。
	TokensIn, TokensOut int
}

// HandoffAware 由消费移交包的 tier 实现（tier-3）。SetHandoff 在每次 Check 前
// 由 Chain 调用；实现应保存整个包（Chain 每次传新快照，非增量）。
type HandoffAware interface {
	SetHandoff(Handoff)
}

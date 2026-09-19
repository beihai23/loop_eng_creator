package loop

// testprep.go —— M1 出题权分离：test-prep step 的执行与考卷回灌。
//
// 设计要点（docs/superpowers/plans/2026-09-18-loop-eng-test-prep-split.md）：
//   - 盲出题：TestPrepInput 白名单制（需求全文/标准原件/上轮考卷），实现侧产物
//     （plan 输出/被拒 diff/verify 判决）一律不进——被考侧的产物是泄题材料，
//     考卷必须从「被要求的」导出、不从「被打算的」导出。
//   - 每轮 attempt 重跑（方案 a）：输入带上轮考卷，「默认沿用」铁律由模板承载；
//     与 plan 的每轮重读状态同构，零状态分支。
//   - 可用性优先（triage-gate arc 同款先例）：test-prep 基础设施错误不 gate
//     loop——step 记 fail、回落 issue 原版标准、tier-1 缺席（tier-2 判），照常跑。

import (
	"context"
	"encoding/json"

	"loop-eng/internal/channel"
	"loop-eng/internal/model"
	"loop-eng/internal/skill"
	"loop-eng/internal/state"
)

// maxPriorExamRunes 是注入 prompt 的上轮考卷上限（与 scene/contract 截断同理：
// 上下文不能吃光预算；完整版永远在 steps.output_json 可查）。
const maxPriorExamRunes = 8000

// runTestPrep 执行一轮 test-prep step：预算闸 → 渲染 prompt → 调模型（worktree
// 只读探索，与 plan 同款 RunIn）→ 记账 → step 落盘（seq=attempt*10+2，role=
// "test-prep"）。返回考卷与真实用量；基础设施错误/解析失败返回 (nil, usage)——
// 调用方回落 issue 原版标准并缺席 tier-1，绝不阻塞 loop。examDispute 非空 =
// 修订模式（M2 争议包回灌，知情修订）。
func (sl *SubLoop) runTestPrep(ctx context.Context, taskID, runID string, attempt int, task channel.Task, priorExam, examDispute, wt string) (*skill.TestPrepOutput, model.Usage) {
	sid := shortID(taskID)
	sl.logf("[subloop] %s phase=test-prep start", sid)
	_ = sl.Store.SetInFlight(taskID, "test-prep")
	var usage model.Usage
	// 预算闸与记账（plan/execute 同款手动包法，buildModels 不再包一层防双计）：
	// BeforeCall 拒付 → 回落出题、照常跑——预算刹车挡的是烧钱，不是考试本身。
	est := sl.Budget.Estimate("test-prep")
	if err := sl.Budget.BeforeCall(est); err != nil {
		sl.logf("[subloop] %s phase=test-prep budget refused (%v) — fall back to issue criteria", sid, err)
		return nil, usage
	}
	sl.Store.AppendBudget(runID, "call", "test-prep", est, sl.Budget.PerCall)
	in := skill.TestPrepInput{
		Task:               task.Description,
		AcceptanceCriteria: task.AcceptanceCriteria, // 标准原件（出题对象）
		Body:               task.Body,               // 全文：出题以原文为准
		PriorExam:          priorExam,               // 上轮考卷（默认沿用）
		DisputePacket:      examDispute,             // M2 争议包（空=正常出题）
	}
	prompt, _ := skill.RenderPrompt(sl.TestPrep.PromptTmpl, in)
	out, u, err := sl.TestPrep.RunIn(ctx, in, wt)
	usage = u
	sl.Budget.Record("test-prep", u)
	// 考卷产出也落 trace（output_json）：出题/修订的审计轨迹（战报/人审可查）。
	outJSON := ""
	if err == nil {
		if jb, mErr := json.Marshal(out); mErr == nil {
			outJSON = string(jb)
		}
	}
	sl.Store.AppendStep(state.StepRow{
		RunID: runID, Seq: attempt*10 + 2, Role: "test-prep",
		Status: statusOf(err), ModelRef: sl.TestPrepModelRef,
		InputJSON: prompt, OutputJSON: outJSON, Error: errStr(err),
		TokensIn: u.TokensIn, TokensOut: u.TokensOut,
	})
	if err != nil {
		sl.logf("[subloop] %s phase=test-prep fail: %v — fall back to issue criteria", sid, err)
		return nil, usage
	}
	sl.logf("[subloop] %s phase=test-prep done", sid)
	return &out, usage
}

// loadPriorExam 跨 run 找回该 issue 最近一次 test-prep 的考卷（Run 开头调用，
// 场景回灌 contract.go 的 loadPriorPlanContract）。best-effort：查询失败返回
// 空串——按「首次出题」处理，绝不阻塞。
func (sl *SubLoop) loadPriorExam(ref string) string {
	if sl.Store == nil || ref == "" {
		return ""
	}
	out, err := sl.Store.LatestTestPrepOutputByRef(ref)
	if err != nil {
		sl.logf("[subloop] load prior exam: %v", err)
		return ""
	}
	return truncateRunes(out, maxPriorExamRunes)
}

// examJSONOf 把本轮考卷序列化为下一轮 attempt 的 PriorExam（run 内回灌，截断同
// loadPriorExam）。nil/序列化失败返回空串（下一轮按首次出题处理）。
func examJSONOf(exam *skill.TestPrepOutput) string {
	if exam == nil {
		return ""
	}
	b, err := json.Marshal(exam)
	if err != nil {
		return ""
	}
	return truncateRunes(string(b), maxPriorExamRunes)
}

// retryDiagnosisForAttempt 在出题权分离（M1）时抑制重试诊断：诊断的元指令是让
// plan 行使 revised_criteria 把证据要求翻译成 tier-1 判据——plan 已无标准修订权
// 可行使，注入只会指向一个不存在的产出字段（M1 已知局限：结构性不可满足的标准
// 暂由 blocked 兜底，知情修订走 M2 争议路由）。legacy（examSeparate=false）保持
// 原行为，逐字不变。
func retryDiagnosisForAttempt(examSeparate bool, attempt int, priorFailure string) string {
	if examSeparate {
		return ""
	}
	return retryDiagnosisFor(attempt, priorFailure)
}

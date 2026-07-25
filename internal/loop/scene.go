package loop

// scene.go —— 失败现场（rejected diff）的回灌通道。
//
// 背景：战报/驳回理由只是「判决」，上一轮 execute 实际写出的 diff 才是「现场」。
// 现场早已落盘（execute step 的 output_json = {out, diff}），但过去没有任何代码
// 路径把它读回来——下一次 run/attempt 只能看着判决书重掷骰子。本文件把现场接回
// 反馈回路：run 内由 verify 驳回处直接更新（不绕 SQLite），跨 run 由 Run 开头按
// issue_ref 从 SQLite 读回（覆盖 daemon 同 task 重跑与 run-once 新 task 行两种形态）。
//
// 安全边界不变：现场只是 prompt 上下文，未验证的工作永远不会因此落地——landing
// 的闸门一直是 verify，不是 worktree 的新旧。

import (
	"context"
	"encoding/json"
	"strings"
)

// maxSceneDiffRunes 是注入 prompt 的现场 diff 上限。超长截断（完整版永远在
// steps.output_json 里可查）——diff 可以很大（生成物/锁文件），prompt 预算不能
// 被一份现场吃光。
const maxSceneDiffRunes = 40000

// executeStepOutput 是 execute step output_json 的形状（与 subloop 落盘处一致）。
type executeStepOutput struct {
	Out  string `json:"out"`
	Diff string `json:"diff"`
}

// diffFromExecuteOutput 从 execute step 的 output_json 提取 diff。解析失败或
// 无 diff 返回空串（按「无现场」处理，绝不因现场缺失阻塞 loop）。
func diffFromExecuteOutput(outputJSON string) string {
	var rec executeStepOutput
	if err := json.Unmarshal([]byte(outputJSON), &rec); err != nil {
		return ""
	}
	return strings.TrimSpace(rec.Diff)
}

// truncateSceneDiff 把现场 diff 截到 maxSceneDiffRunes 以内，截断处标注完整版去向。
func truncateSceneDiff(diff string) string {
	r := []rune(diff)
	if len(r) <= maxSceneDiffRunes {
		return diff
	}
	return string(r[:maxSceneDiffRunes]) + "\n…（diff 过长已截断，完整版见 state.db steps.output_json）"
}

// loadPriorSceneDiff 跨 run 找回该 issue 最近一次 execute 的 diff（Run 开头调用）。
// best-effort：查询失败/无现场都返回空串——现场是增强信号，不是必需品。
func (sl *SubLoop) loadPriorSceneDiff(ctx context.Context, ref string) string {
	if sl.Store == nil || ref == "" {
		return ""
	}
	out, err := sl.Store.LatestExecuteOutputByRef(ref)
	if err != nil {
		sl.logf("[subloop] load prior scene: %v", err)
		return ""
	}
	_ = ctx // 查询走 store（无 ctx 参数）；保留形参位置以备未来 store 支持 ctx
	return diffFromExecuteOutput(out)
}

// rejectedSceneSection 构造 execute prompt 的「上一轮被驳回实现」段：判决（驳回
// 理由，可空）+ 现场（diff）。diff 为空返回空串（无现场不注入）。plan 侧的同款
// 内容经 PlanInput.RejectedDiff + plan.md 条件块渲染，两处文案各自适配角色。
func rejectedSceneSection(priorFailure, diff string) string {
	diff = truncateSceneDiff(strings.TrimSpace(diff))
	if diff == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("---\n上一轮实现被 verify 驳回的现场（diff）。")
	if strings.TrimSpace(priorFailure) != "" {
		b.WriteString("\n驳回理由：\n" + priorFailure + "\n")
	}
	b.WriteString("\n上一轮实际写出的改动——在它的基础上修正，或判断方向错误后推倒重来，" +
		"由你决定；不要因为它的存在就默认沿旧路走（它被判过不合格）：\n```diff\n" + diff + "\n```\n")
	return b.String()
}

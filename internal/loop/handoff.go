package loop

// handoff.go —— verify 调用点的移交包组装 + execute 自报「环境与复现」段的宽松提取。
//
// 移交包（verify.Handoff）把 loop-eng 机器通道的「现场级」交接带到 agent→人方向
// （tier-3 人审评论）：判决之外，现场、环境、风险、历轮、token 透明度一并给到
// 接手的人。字段全部来自 Run 作用域的实时/落盘数据，无新增查询。

import (
	"strings"

	"loop-eng/internal/skill"
	"loop-eng/internal/verify"
)

// maxEnvNotesRunes 是注入移交包的环境说明上限（与 scene/contract 的截断同理：
// 上下文不能吃光预算；完整版永远在 steps.output_json）。
const maxEnvNotesRunes = 4000

// buildHandoff 组装 agent→人的移交包。branch 是本轮 park 前将 commit 的分支名
// （branchName(taskID, attempt)）——即使本轮最终未 park（verify 驳回短路），
// 包里的分支名也只是无人消费的字符串，无副作用。criteriaRevised 时带 plan 的
// 修订理由（人审的审计线索：按修订版判过）。
func (sl *SubLoop) buildHandoff(attempt int, wt, branch string, planOut skill.PlanOutput,
	criteriaRevised bool, execOut, runHistory string, tokensIn, tokensOut int) verify.Handoff {
	ho := verify.Handoff{
		Attempt:    attempt,
		MaxRetries: sl.Budget.MaxRetries,
		Worktree:   wt,
		Branch:     branch,
		RunHistory: runHistory,
		EnvNotes:   extractEnvNotes(execOut),
		Risks:      planOut.Risks,
		TokensIn:   tokensIn,
		TokensOut:  tokensOut,
	}
	if criteriaRevised {
		ho.CriteriaNotes = planOut.CriteriaNotes
	}
	return ho
}

// extractEnvNotes 从 execute 自报宽松提取「环境与复现」段：匹配以「环境与复现」
// 为题的 markdown 标题行（1-3 级井号任意，题名允许尾随空白），截到下一同级或
// 更高级标题为止。找不到返回空串——旧格式自报没有此段，按「无环境说明」渲染，
// 绝不因格式不符阻塞。与 tier-1 契约同款宽容纪律：提示词要求 ≠ 模型必然照办，
// 提取端永不报错。
func extractEnvNotes(self string) string {
	lines := strings.Split(self, "\n")
	start, level := -1, 0
	for i, ln := range lines {
		t := strings.TrimSpace(ln)
		if !strings.HasPrefix(t, "#") {
			continue
		}
		lvl := headingLevel(t)
		if lvl > 0 && strings.TrimSpace(strings.TrimLeft(t, "#")) == "环境与复现" {
			start, level = i, lvl
			break
		}
	}
	if start < 0 {
		return ""
	}
	var b strings.Builder
	for _, ln := range lines[start+1:] {
		t := strings.TrimSpace(ln)
		if lvl := headingLevel(t); lvl > 0 && lvl <= level {
			break // 同级或更高级标题 → 段结束
		}
		b.WriteString(ln)
		b.WriteString("\n")
	}
	out := strings.TrimSpace(b.String())
	if r := []rune(out); len(r) > maxEnvNotesRunes {
		out = string(r[:maxEnvNotesRunes]) + "\n…（过长已截断，完整版见 state.db steps.output_json）"
	}
	return out
}

// headingLevel 返回 markdown 标题的井号级数（1-6）；非标题（含 `#` 起头但井号后
// 无内容的行、代码围栏内假标题）返回 0。宽松提取不追求完全正确的 markdown 语义
// ——只要误判方向是「少提取」（漏段按无环境说明处理），没有误伤面。
func headingLevel(t string) int {
	if t == "" || !strings.HasPrefix(t, "#") {
		return 0
	}
	lvl := len(t) - len(strings.TrimLeft(t, "#"))
	if lvl < 1 || lvl > 6 {
		return 0
	}
	// 井号后必须跟空白或行尾（#tag 不是标题）。
	rest := t[lvl:]
	if rest != "" && !strings.HasPrefix(rest, " ") && !strings.HasPrefix(rest, "\t") {
		return 0
	}
	return lvl
}

package loop

// runhistory.go —— run history 回灌通道：跨 run 的机器记忆从 DB 构建。
//
// 背景：plan/execute 过去的跨 run 记忆靠 collectIssueComments 读回 issue 评论
// 线程——那里面混着 daemon 自己发的战报（VERIFY-FAIL/DONE/BLOCKED），同一条驳回
// 理由以散文形式第二次进 prompt，且随轮次无界膨胀（自回声）。战报是人可读写回，
// 只写不读；机器要的「这个任务之前跑过几轮、各轮怎么结的、最近驳回是什么」全部
// 在 DB（runs/steps）里有结构化记录。本文件把它构建成有界的文本摘要：
// 每条已终结 run 一行（outcome + 最后一次 verify 驳回理由截断），行数 ≤
// maxRunHistoryRuns——大小随 run 数有界，不随重试轮次膨胀。
//
// 与 loadPriorSceneDiff / loadPriorPlanContract 同款姿势：store 取原始数据，
// 这里解析格式化（verifyTrace 是 loop 私有形状，state 不解析）。

import (
	"encoding/json"
	"strings"

	"loop-eng/internal/state"
)

// maxRunHistoryRuns 是注入 prompt 的历史 run 条数上限（取最近的 N 条）。
// 超出时更早的历史由「共 N 条，以下为最近 M 条」的语义截断——run 数本身有界
// （一个 issue 通常个位数 run），上限只是防御性的。
const maxRunHistoryRuns = 8

// maxRunHistoryDetailRunes 是单行驳回理由的截断长度：一行一个要点，完整版永远在
// steps.output_json / verify step 里可查。
const maxRunHistoryDetailRunes = 160

// BuildRunHistory 从 DB 构建该 issue_ref 的历轮 run 摘要文本（每条已终结 run 一行，
// 时间升序）。供 plan（PlanInput.RunHistory）、triage（TriageInput.RunHistory）与
// execute prompt 的历轮摘要段共用——cli/daemon 的 triage 接线也调它，故为包级函数。
// best-effort：nil store / 空 ref / 查询失败 / 无历史均返回空串（历史是增强信号，
// 不是必需品；首次 run 本就为空）。
func BuildRunHistory(st *state.Store, ref string) string {
	if st == nil || ref == "" {
		return ""
	}
	rows, err := st.RunHistoryByRef(ref, maxRunHistoryRuns)
	if err != nil || len(rows) == 0 {
		return ""
	}
	var b strings.Builder
	for _, r := range rows {
		b.WriteString("- " + r.Outcome)
		if d := runHistoryDetail(r.VerifyOutput); d != "" {
			b.WriteString(" — " + d)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// runHistoryDetail 从 verify step 的 output_json（verifyTrace）提取驳回理由并截断。
// 解析失败/无 detail（如 done run 的「全部满足」可留空）返回空串——该行只留 outcome。
func runHistoryDetail(verifyOutput string) string {
	if strings.TrimSpace(verifyOutput) == "" {
		return ""
	}
	var vt verifyTrace
	if err := json.Unmarshal([]byte(verifyOutput), &vt); err != nil {
		return ""
	}
	return truncateStr(strings.TrimSpace(vt.Detail), maxRunHistoryDetailRunes)
}

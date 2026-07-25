package loop

// contract.go —— plan 冻结合同的双向可见性。
//
// 根治 #71 三轮 blocked 的病根：plan 把 parseClaudeResult 的签名冻结进 tier-1
// 测试（4→3→2 每轮重设计），execute 看不到这份合同、每轮瞎猜（3→2→3），双方
// 互不可见、同时对陈旧信号反应 → 振荡。修法是让合同有唯一所有者（plan）且
// 双向可见：
//
//   - execute 侧（executeContractSection）：本轮 plan 冻结的合同（steps 里冻结的
//     签名 + verify_script 验收脚本）直达 execute prompt——被合同约束的人必须
//     能看到合同，tier-1 是按它判的。
//   - plan 侧（PriorPlanContract）：上一轮冻结的合同回灌下一轮 plan（run 内内存
//     直传，跨 run 按 issue_ref 从 steps.output_json 读回）——默认保持稳定，
//     驳回理由证明合同本身错误时才修订，不要每轮盲重掷签名。

import (
	"encoding/json"
	"fmt"
	"strings"

	"loop-eng/internal/skill"
)

// maxContractRunes 是注入 prompt 的合同文本上限（与 maxSceneDiffRunes 同理：
// 合同是上下文，不能吃掉整个 prompt 预算；完整版永远在 steps.output_json）。
const maxContractRunes = 20000

// planContract 是 PlanOutput 的合同投影——回灌/注入只带约束实现的两块：
// 冻结的步骤（含签名）与 tier-1 验收脚本。risks/revised_criteria 走原有通道。
type planContract struct {
	Plan         []skill.PlanStep         `json:"plan"`
	VerifyScript *skill.PlanVerifyScript  `json:"verify_script,omitempty"`
}

// contractOf 从一次 plan 产出提取合同投影（JSON）。空 plan / 序列化失败返回空串
// （按「无合同」处理——合同是增强信号，绝不阻塞 loop）。
func contractOf(planOut skill.PlanOutput) string {
	if len(planOut.Plan) == 0 {
		return ""
	}
	b, err := json.Marshal(planContract{Plan: planOut.Plan, VerifyScript: planOut.VerifyScript})
	if err != nil {
		return ""
	}
	return truncateRunes(string(b), maxContractRunes)
}

// loadPriorPlanContract 跨 run 找回该 issue 上一轮 plan 冻结的合同（Run 开头调用）。
// best-effort：查询/解析失败都返回空串。
func (sl *SubLoop) loadPriorPlanContract(ref string) string {
	if sl.Store == nil || ref == "" {
		return ""
	}
	out, err := sl.Store.LatestPlanOutputByRef(ref)
	if err != nil {
		sl.logf("[subloop] load prior plan contract: %v", err)
		return ""
	}
	if out == "" {
		return ""
	}
	var po skill.PlanOutput
	if err := json.Unmarshal([]byte(out), &po); err != nil {
		return ""
	}
	return contractOf(po)
}

// executeContractSection 构造 execute prompt 的「本轮实现合同」段：plan 冻结的
// 步骤（含签名）+ tier-1 验收脚本全文。execute 过去从来看不到这两样——它要猜
// tier-1 按什么判，#71 三轮振荡的直接根源。空合同（plan 未产出步骤）返回空串。
func executeContractSection(planOut skill.PlanOutput) string {
	if len(planOut.Plan) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("---\n本轮 plan 冻结的实现合同（tier-1 将严格按此验收——你新增/修改的函数、" +
		"类型、API 签名必须与合同逐字一致；plan 已替你做完设计决策，不要发明第三种签名）:\n")
	for i, st := range planOut.Plan {
		fmt.Fprintf(&b, "%d. %s", i+1, st.Step)
		if len(st.Files) > 0 {
			b.WriteString("（files: " + strings.Join(st.Files, ", ") + "）")
		}
		if st.Expected != "" {
			b.WriteString(" → 预期: " + st.Expected)
		}
		b.WriteString("\n")
	}
	if vs := planOut.VerifyScript; vs != nil && vs.Valid() {
		b.WriteString("\ntier-1 验收脚本")
		if vs.File != "" {
			b.WriteString("（将写入 " + vs.File)
		}
		b.WriteString("，运行: " + strings.Join(vs.Run, " "))
		if vs.File != "" {
			b.WriteString("）")
		}
		b.WriteString(":\n")
		if vs.Body != "" {
			b.WriteString("```\n" + truncateRunes(vs.Body, maxContractRunes) + "\n```\n")
		}
	}
	return b.String()
}

// truncateRunes 截断到 n 个 rune，截断处标注去向（合同/现场的完整版都在 state.db）。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "\n…（过长已截断，完整版见 state.db steps.output_json）"
}

package cli

import (
	"strings"
	"testing"
)

// TestZZPlanExploreTier1Contract 是 tier-1 验收：plan.md embed 必须含「主动探索仓库」指令。
// 只断言结构落点（embed 文本含标记串），不断言「plan 探索后规划更好」（非确定性，#47 教训）。
// 文件名/函数名用 zz 前缀，避免与 implementation 可能新增的常驻 TestPlanPromptExploresRepo 冲突。
func TestZZPlanExploreTier1Contract(t *testing.T) {
	p := mustSkillPrompt("plan")
	for _, want := range []string{"只读不写", "repo-knowledge-map", "主动探索", "禁止发明不存在的文件"} {
		if !strings.Contains(p, want) {
			t.Fatalf("plan.md embed 缺少探索指令标记 %q; rendered plan prompt:\n%s", want, p)
		}
	}
}

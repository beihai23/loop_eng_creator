package tui

import (
	"strings"
	"testing"

	"loop-eng/internal/config"
	"loop-eng/internal/state"
)

func TestRenderDetailFields(t *testing.T) {
	st, _ := state.Open(t.TempDir() + "/state.db")
	defer st.Close()
	tid, _ := st.InsertTask(state.TaskRow{
		IssueRef: "#18", Description: "给 budget 加硬上限",
		TaskType: "feature", Criteria: []string{"命中上限立即停", "落盘 budget_ledger"},
	})
	rid, _ := st.StartRun(tid)
	_ = st.AppendVerification(rid, 1, true, "go test: ok")
	_ = st.AppendVerification(rid, 2, false, "LLM: diff unrelated")

	cfg := &config.Config{}
	cfg.Models.Verify = config.ModelRef{Name: "glm-5.2"}
	cfg.Verify.Tier3Human = true

	out := RenderDetail(st, cfg, tid)
	for _, want := range []string{"#18", "给 budget 加硬上限", "feature",
		"验收方式", "go test", "glm-5.2", "人审", "验收标准", "命中上限立即停", "落盘 budget_ledger"} {
		if !strings.Contains(out, want) {
			t.Fatalf("detail missing %q in:\n%s", want, out)
		}
	}
	// 标题行不应出现双 #（IssueRef 已含 #）
	if !strings.Contains(out, "#18 给 budget 加硬上限") {
		t.Fatalf("detail title not exact: want %q in:\n%s", "#18 给 budget 加硬上限", out)
	}
	if strings.Contains(out, "##18") {
		t.Fatalf("detail title has double-hash ##18 in:\n%s", out)
	}
	// 逐 tier 状态符号
	if !strings.Contains(out, "✓ passed") {
		t.Fatalf("detail missing tier-1 ✓ passed in:\n%s", out)
	}
	if !strings.Contains(out, "✗") {
		t.Fatalf("detail missing tier-2 ✗ in:\n%s", out)
	}
}

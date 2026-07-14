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
}

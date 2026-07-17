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
	cfg.Budget.PerTaskTokens = 100000

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
	// I2：预算行展示真实上限（cfg.Budget.PerTaskTokens）
	if !strings.Contains(out, "100000") {
		t.Fatalf("detail budget line missing real cap 100000 in:\n%s", out)
	}
}

// TestRenderDetailDoneTaskShowsTiersAndBudget 钉死 done 任务的详情：
// run 已 ended_at（无 active run），verifications 三层有记录。RenderDetail 须兜底到
// 该 run，让逐 tier 状态/预算有数据——三层显示 ✓/✗（不再是全 —）、预算有值、
// tier-1 标签取 verification Detail 冒号前的真值（不再是固定 go test ./...）。
func TestRenderDetailDoneTaskShowsTiersAndBudget(t *testing.T) {
	st, _ := state.Open(t.TempDir() + "/state.db")
	defer st.Close()
	tid, _ := st.InsertTask(state.TaskRow{
		IssueRef: "#22", Description: "三层验证 + 预算落盘",
		TaskType: "feature", Criteria: []string{"逐 tier 落 verifications", "预算有值"},
	})
	rid, _ := st.StartRun(tid)

	// 三层验证落盘：tier-1/tier-3 过、tier-2 挂（同时覆盖 ✓ 与 ✗ 两条路径）。
	_ = st.AppendVerification(rid, 1, true, "go-test: ok")
	_ = st.AppendVerification(rid, 2, false, "glm-5.2: diff unrelated to criteria")
	_ = st.AppendVerification(rid, 3, true, "人审: ok")
	// 预算有花费：used=5000。
	_ = st.AppendBudget(rid, "call", "tokens", 5000, 100000)
	// run 跑完 → done（ended_at 落盘，ActiveRun 因此为空）。
	_ = st.EndRun(rid, "done")

	cfg := &config.Config{}
	cfg.Models.Verify = config.ModelRef{Name: "glm-5.2"}
	cfg.Verify.Tier3Human = true
	cfg.Budget.PerTaskTokens = 100000

	out := RenderDetail(st, cfg, tid)

	// 三层不再是全 —：tier-1/tier-3 过（✓ passed）、tier-2 挂（✗ + 原因）。
	if !strings.Contains(out, "✓ passed") {
		t.Fatalf("done task missing ✓ passed (tier-1/3) in:\n%s", out)
	}
	if !strings.Contains(out, "✗") {
		t.Fatalf("done task missing tier-2 ✗ reason in:\n%s", out)
	}
	// tier-1 标签取 verification Detail 冒号前的真值，不再是固定 go test ./...
	if !strings.Contains(out, "go-test") {
		t.Fatalf("done task tier-1 label not derived from verification Detail in:\n%s", out)
	}
	if strings.Contains(out, "go test ./...") {
		t.Fatalf("done task tier-1 still hardcoded 'go test ./...' in:\n%s", out)
	}
	// 预算有值：used=5000、limit=100000。
	if !strings.Contains(out, "5000") {
		t.Fatalf("done task budget used missing in:\n%s", out)
	}
	if !strings.Contains(out, "100000") {
		t.Fatalf("done task budget limit missing in:\n%s", out)
	}
}

// TestRenderDetailRunningTaskNoRegression 保证 in-flight 任务（有 active run）在
// run_id 兜底改造后不回归：逐 tier 状态、tier-1 标签、预算照常显示。
func TestRenderDetailRunningTaskNoRegression(t *testing.T) {
	st, _ := state.Open(t.TempDir() + "/state.db")
	defer st.Close()
	tid, _ := st.InsertTask(state.TaskRow{
		IssueRef: "#31", Description: "running 任务不回归",
		TaskType: "feature", Criteria: []string{"逐 tier 实时状态"},
	})
	rid, _ := st.StartRun(tid) // 未 EndRun → 仍是 active run
	_ = st.AppendVerification(rid, 1, true, "go-test: ok")

	cfg := &config.Config{}
	cfg.Models.Verify = config.ModelRef{Name: "glm-5.2"}
	cfg.Budget.PerTaskTokens = 80000

	out := RenderDetail(st, cfg, tid)

	// running 任务走 ActiveRun 分支：tier-1 过、tier-1 标签派生、预算上限照常。
	if !strings.Contains(out, "✓ passed") {
		t.Fatalf("running task missing tier-1 ✓ passed in:\n%s", out)
	}
	if !strings.Contains(out, "go-test") {
		t.Fatalf("running task tier-1 label not derived in:\n%s", out)
	}
	if !strings.Contains(out, "80000") {
		t.Fatalf("running task budget limit missing in:\n%s", out)
	}
	// 启动/已运行 行只在 active run 显示——running 任务须有。
	if !strings.Contains(out, "已运行") {
		t.Fatalf("running task missing 已运行 elapsed line in:\n%s", out)
	}
}

// TestRenderDetailTier1LabelPlanNotProduced 钉死 tier-1 标签的兜底：无 tier=1 行
// （plan 未产出验收脚本，链直接落 tier-2）时，tier-1 标签显示 (plan 未产出)。
func TestRenderDetailTier1LabelPlanNotProduced(t *testing.T) {
	st, _ := state.Open(t.TempDir() + "/state.db")
	defer st.Close()
	tid, _ := st.InsertTask(state.TaskRow{IssueRef: "#7", Description: "plan 未产出 tier-1", TaskType: "bug"})
	rid, _ := st.StartRun(tid)
	// 只有 tier-2 落盘——plan 未产出 tier-1 验收脚本。
	_ = st.AppendVerification(rid, 2, true, "glm-5.2: ok")
	_ = st.EndRun(rid, "done")

	cfg := &config.Config{}
	cfg.Models.Verify = config.ModelRef{Name: "glm-5.2"}

	out := RenderDetail(st, cfg, tid)
	if !strings.Contains(out, "(plan 未产出)") {
		t.Fatalf("missing (plan 未产出) tier-1 label when no tier=1 row in:\n%s", out)
	}
}

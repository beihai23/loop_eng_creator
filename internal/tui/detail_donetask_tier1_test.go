package tui

import (
    "strings"
    "testing"

    "loop-eng/internal/config"
    "loop-eng/internal/state"
)

// TestTier1DoneTaskShowsTiersAndBudget: done task whose run is already ended
// with 3-tier verifications recorded. ActiveRun returns ok=false here, so
// RenderDetail must fall back to the RunsOfTask tail or every tier renders ---
// and budget shows 0/0.
func TestTier1DoneTaskShowsTiersAndBudget(t *testing.T) {
    st, _ := state.Open(t.TempDir() + "/state.db")
    defer st.Close()
    tid, _ := st.InsertTask(state.TaskRow{
        IssueRef: "#99", Description: "done detail", TaskType: "feature",
        Criteria: []string{"c1"},
    })
    rid, _ := st.StartRun(tid)
    _ = st.AppendVerification(rid, 1, true, "make verify-acc: ok")
    _ = st.AppendVerification(rid, 2, false, "LLM: diff unrelated")
    _ = st.AppendVerification(rid, 3, true, "human: ok")
    _ = st.AppendBudget(rid, "call", "tokens", 5000, 0)
    if err := st.EndRun(rid, "done"); err != nil {
        t.Fatalf("EndRun: %v", err)
    }
    cfg := &config.Config{}
    cfg.Models.Verify = config.ModelRef{Name: "glm-5.2"}
    cfg.Verify.Tier3Human = true
    cfg.Budget.PerTaskTokens = 100000

    out := RenderDetail(st, cfg, tid)
    for _, want := range []string{"✓ passed", "✗", "make verify-acc", "5000", "100000"} {
        if !strings.Contains(out, want) {
            t.Fatalf("done-task detail missing %q in:\n%s", want, out)
        }
    }
}

// TestTier1DoneTaskNoTier1Label: done task with no tier=1 verification row ->
// the tier-1 label must read the placeholder (plan 未产出).
func TestTier1DoneTaskNoTier1Label(t *testing.T) {
    st, _ := state.Open(t.TempDir() + "/state.db")
    defer st.Close()
    tid, _ := st.InsertTask(state.TaskRow{IssueRef: "#100", Description: "no tier1", TaskType: "feature"})
    rid, _ := st.StartRun(tid)
    _ = st.AppendVerification(rid, 2, true, "LLM: ok") // intentionally no tier=1 row
    if err := st.EndRun(rid, "done"); err != nil {
        t.Fatalf("EndRun: %v", err)
    }
    out := RenderDetail(st, &config.Config{}, tid)
    if !strings.Contains(out, "(plan 未产出)") {
        t.Fatalf("want tier-1 label (plan 未产出) when no tier=1 row; got:\n%s", out)
    }
}

// TestTier1RunningTaskNoRegress: a running task (no EndRun) must still resolve
// via ActiveRun; the new fallback must not change its tier/budget rendering.
func TestTier1RunningTaskNoRegress(t *testing.T) {
    st, _ := state.Open(t.TempDir() + "/state.db")
    defer st.Close()
    tid, _ := st.InsertTask(state.TaskRow{IssueRef: "#101", Description: "running", TaskType: "feature"})
    rid, _ := st.StartRun(tid) // NOT ended -> ActiveRun hits
    _ = st.AppendVerification(rid, 1, true, "go test: ok")
    _ = st.AppendVerification(rid, 2, false, "diff unrelated")
    _ = st.AppendBudget(rid, "call", "tokens", 1234, 0)
    cfg := &config.Config{}
    cfg.Budget.PerTaskTokens = 50000
    out := RenderDetail(st, cfg, tid)
    for _, want := range []string{"✓ passed", "✗", "1234"} {
        if !strings.Contains(out, want) {
            t.Fatalf("running-task detail missing %q in:\n%s", want, out)
        }
    }
}

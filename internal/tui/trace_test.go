package tui

import (
	"strings"
	"testing"

	"loop-eng/internal/state"
)

// runs 有记录（走 StartRun/EndRun）：每条 run = 一轮，steps 直接对应。
func TestRenderTraceGroupsByRun(t *testing.T) {
	st, _ := state.Open(t.TempDir() + "/state.db")
	defer st.Close()
	tid, _ := st.InsertTask(state.TaskRow{IssueRef: "#1", Description: "d"})
	r1, _ := st.StartRun(tid)
	_ = st.AppendStep(state.StepRow{RunID: r1, Seq: 1, Role: "plan", Status: "ok"})
	_ = st.EndRun(r1, "needs-review")
	r2, _ := st.StartRun(tid)
	_ = st.AppendStep(state.StepRow{RunID: r2, Seq: 1, Role: "plan", Status: "ok"})
	_ = st.EndRun(r2, "done")

	out := RenderTrace(st, tid)
	if !strings.Contains(out, "第 1 轮") || !strings.Contains(out, "第 2 轮") {
		t.Fatalf("两轮卡片缺失:\n%s", out)
	}
	if !strings.Contains(out, "plan") {
		t.Fatalf("plan step 缺失:\n%s", out)
	}
}

// runs 表为空（旧数据）：按 transitions 的派发切轮重建，结局原因压成人话。
func TestRenderTraceRebuildsWhenRunsEmpty(t *testing.T) {
	st, _ := state.Open(t.TempDir() + "/state.db")
	defer st.Close()
	tid, _ := st.InsertTask(state.TaskRow{IssueRef: "#1", Description: "d"})
	st.AppendTransition(tid, "new", "running", "dispatched")
	_ = st.AppendStep(state.StepRow{RunID: tid, Role: "plan", Status: "ok"})
	st.AppendTransition(tid, "running", "blocked", "retries exhausted: verify 驳回")

	out := RenderTrace(st, tid)
	if !strings.Contains(out, "第 1 轮") {
		t.Fatalf("runs 为空时应按派发重建出轮次:\n%s", out)
	}
	if !strings.Contains(out, "重试耗尽") {
		t.Fatalf("结局原因应压成人话:\n%s", out)
	}
}

// 纯单元：inferRounds 按 transitions 派发切轮 + steps 落窗 + 结局态/原因。
func TestInferRoundsFromTransitions(t *testing.T) {
	trans := []state.TransitionRow{
		{From: "new", To: "running", Reason: "dispatched", At: "2026-07-10T03:44:00Z"},
		{From: "running", To: "blocked", Reason: "retries exhausted", At: "2026-07-10T04:19:00Z"},
		{From: "blocked", To: "new", Reason: "resumed", At: "2026-07-10T13:53:00Z"},
		{From: "new", To: "running", Reason: "dispatched", At: "2026-07-10T14:07:00Z"},
		{From: "running", To: "done", Reason: "tier3 auto-pass placeholder", At: "2026-07-10T15:15:00Z"},
	}
	steps := []state.StepRow{
		{RunID: "t", Role: "plan", Status: "ok", At: "2026-07-10T03:59:00Z"},
		{RunID: "t", Role: "verify", Status: "fail", At: "2026-07-10T04:08:00Z"},
		{RunID: "t", Role: "plan", Status: "ok", At: "2026-07-10T14:10:00Z"},
		{RunID: "t", Role: "verify", Status: "ok", At: "2026-07-10T15:10:00Z"},
	}
	rounds := inferRounds(nil, trans, nil, steps)
	if len(rounds) != 2 {
		t.Fatalf("want 2 rounds, got %d", len(rounds))
	}
	if rounds[0].outcome != "blocked" || len(rounds[0].steps) != 2 {
		t.Errorf("round1: outcome=%s steps=%d (want blocked/2)", rounds[0].outcome, len(rounds[0].steps))
	}
	if rounds[1].outcome != "done" || len(rounds[1].steps) != 2 {
		t.Errorf("round2: outcome=%s steps=%d (want done/2)", rounds[1].outcome, len(rounds[1].steps))
	}
}

// 纯单元：dedupe 去掉 daemon/subloop 双写的回声（ran + 空 from 的 dispatched）。
func TestDedupeTransitions(t *testing.T) {
	in := []state.TransitionRow{
		{From: "new", To: "running", Reason: "dispatched", At: "1"},
		{From: "", To: "running", Reason: "dispatched", At: "2"}, // 回声：去
		{From: "running", To: "blocked", Reason: "retries exhausted", At: "3"},
		{From: "running", To: "blocked", Reason: "ran", At: "4"}, // 回声：去
	}
	out := dedupeTransitions(in)
	if len(out) != 2 {
		t.Fatalf("want 2 after dedupe, got %d: %+v", len(out), out)
	}
}

// 纯单元：summarizeReason 把原始报文压成人话。
func TestSummarizeReason(t *testing.T) {
	cases := map[string]string{
		"fatal model error: claude ... 401 Authentication Fails":           "模型 401 认证失败",
		"retries exhausted: plan error: context canceled after 3 attempts": "模型调用被取消",
		"retries exhausted: ":         "重试耗尽",
		"tier3 auto-pass placeholder": "tier-3 自动放行",
		"transient infra; re-queued":  "瞬时故障，已重排队",
	}
	for raw, want := range cases {
		if got := summarizeReason(raw, "blocked"); got != want {
			t.Errorf("summarizeReason(%q) = %q, want %q", raw, got, want)
		}
	}
}

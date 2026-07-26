package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"loop-eng/internal/state"
)

// TestTier1ResumeCancelled 验收 plan 契约：cancelled 任务经 TUI resume 命令 →
// cancelled→new，payload 注入为 resume feedback，命令标记 applied。
func TestTier1ResumeCancelled(t *testing.T) {
	st := newTestStore(t)
	eng := &Engine{Channel: &scriptedChannel{}, Store: st, Interval: time.Second}

	id, err := st.InsertTask(state.TaskRow{IssueRef: "o/r#1", Description: "d"})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := st.AppendTransition(id, "new", "cancelled", "cancelled by TUI"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if err := st.InsertCommand(id, "resume", "retry with fix Y"); err != nil {
		t.Fatalf("insert cmd: %v", err)
	}

	if err := eng.drainCommands(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}

	if got := statusOf(t, st, id); got != "new" {
		t.Fatalf("status=%q want new", got)
	}
	fb, err := st.PopResumeFeedback(id)
	if err != nil || !strings.Contains(fb, "fix Y") {
		t.Fatalf("feedback=%q err=%v want contains 'fix Y'", fb, err)
	}
	trans, _ := st.Transitions(id)
	var saw bool
	for _, tr := range trans {
		if tr.From == "cancelled" && tr.To == "new" && strings.HasPrefix(tr.Reason, "tui resume:") {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("no cancelled→new resume transition in %+v", trans)
	}
	if pend, _ := st.PendingCommands(); len(pend) != 0 {
		t.Fatalf("pending=%d want 0", len(pend))
	}
}

// TestTier1ResumeIdempotentTerminal 验收 plan 契约：对 done/error 任务 resume
// 是 no-op（不迁移、不写 feedback），只回写 applied_at。
func TestTier1ResumeIdempotentTerminal(t *testing.T) {
	st := newTestStore(t)
	eng := &Engine{Channel: &scriptedChannel{}, Store: st, Interval: time.Second}

	mk := func(term string) string {
		id, err := st.InsertTask(state.TaskRow{IssueRef: "o/r#" + term, Description: "d"})
		if err != nil {
			t.Fatalf("insert %s: %v", term, err)
		}
		if err := st.AppendTransition(id, "new", term, "ran"); err != nil {
			t.Fatalf("transition %s: %v", term, err)
		}
		if err := st.InsertCommand(id, "resume", "should be ignored"); err != nil {
			t.Fatalf("insert cmd %s: %v", term, err)
		}
		return id
	}
	doneID := mk("done")
	errID := mk("error")

	if err := eng.drainCommands(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}

	for _, c := range []struct{ id, want string }{{doneID, "done"}, {errID, "error"}} {
		if got := statusOf(t, st, c.id); got != c.want {
			t.Fatalf("status=%q want %q (resume must be idempotent)", got, c.want)
		}
		fb, err := st.PopResumeFeedback(c.id)
		if err != nil || fb != "" {
			t.Fatalf("%s feedback=%q want empty (no side effect)", c.want, fb)
		}
		trans, _ := st.Transitions(c.id)
		for _, tr := range trans {
			if tr.To == "new" {
				t.Fatalf("%s task got a →new transition %+v (must be no-op)", c.want, tr)
			}
		}
	}
	if pend, _ := st.PendingCommands(); len(pend) != 0 {
		t.Fatalf("pending=%d want 0 (terminal resume must still be marked applied)", len(pend))
	}
}
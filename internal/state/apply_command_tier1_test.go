package state

import "testing"

// TestTier1ApplyCommandIdempotent 钉死 Store.ApplyCommand 的 crash-mid 幂等契约
//（applyCommand + MarkCommandApplied 两步非事务的原子替代）：transition 与
// applied_at 标记在同一事务提交，故对同一命令再次 apply（crash 重启后的重 drain）
// 不写重复 transition、不写错 from_status。
func TestTier1ApplyCommandIdempotent(t *testing.T) {
	s, err := Open(t.TempDir() + "/tier1.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	id, err := s.InsertTask(TaskRow{IssueRef: "o/r#1", Description: "d"})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := s.AppendTransition(id, "new", "blocked", "exhausted"); err != nil {
		t.Fatalf("park: %v", err)
	}
	if err := s.InsertCommand(id, "resume", "retry fix Z"); err != nil {
		t.Fatalf("insert cmd: %v", err)
	}
	pending, err := s.PendingCommands()
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending=%d err=%v want 1", len(pending), err)
	}
	cmdID := pending[0].ID
	resumeFrom := []string{"needs-review", "blocked", "needs-info", "needs-human-decision", "cancelled"}

	// 第一次 apply：blocked → new，feedback 落盘，命令标记 applied。
	applied, transitioned, err := s.ApplyCommand(cmdID, id, resumeFrom, "new", "tui resume: retry fix Z", "retry fix Z")
	if err != nil || !applied || !transitioned {
		t.Fatalf("apply 1: applied=%v transitioned=%v err=%v want true/true/nil", applied, transitioned, err)
	}

	// 再次 apply（crash 重启重 drain）：命令已 applied → 完全 no-op。
	applied2, transitioned2, err := s.ApplyCommand(cmdID, id, resumeFrom, "new", "tui resume: retry fix Z", "retry fix Z")
	if err != nil || applied2 || transitioned2 {
		t.Fatalf("apply 2: applied=%v transitioned=%v err=%v want false/false/nil (re-drain must be a no-op)", applied2, transitioned2, err)
	}

	// 恰好一条 →new transition 且 from_status 正确——无重复、无错 from_status。
	trans, err := s.Transitions(id)
	if err != nil {
		t.Fatalf("transitions: %v", err)
	}
	rows := 0
	for _, tr := range trans {
		if tr.From == "" {
			t.Fatalf("transition with empty from_status (wrong from_status): %+v", tr)
		}
		if tr.To == "new" {
			rows++
			if tr.From != "blocked" {
				t.Fatalf("resume transition wrong from_status=%q want blocked", tr.From)
			}
		}
	}
	if rows != 1 {
		t.Fatalf("resume transitions=%d want exactly 1 (no duplicate after crash-restart re-drain): %+v", rows, trans)
	}
}

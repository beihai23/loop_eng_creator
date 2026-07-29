package state

// runhistory_test.go 钉死 RunHistoryByRef 的取数契约（见 state.go 该函数注释）：
// run history 回灌（plan/triage/execute 的跨 run 机器记忆从 DB 构建，战报评论只写
// 不读）依赖这里返回「已终结 run + 各 run 最后一次 verify output」，且分诊附属 run
// 与未终结 run 不得混入。

import (
	"strings"
	"testing"
)

// seedRun 开一个 run 并把 started_at 钉到指定值（测试内多 run 同秒创建，排序
// 需要确定性的时间戳），附一次 verify step（output 可空串 = 无 verify step），
// 最后以 outcome 收尾（outcome 空串 = 不收尾，模拟未终结 run）。
func seedRun(t *testing.T, s *Store, taskID, startedAt, verifyOut, outcome string) {
	t.Helper()
	rid, err := s.StartRun(taskID)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE runs SET started_at=? WHERE id=?`, startedAt, rid); err != nil {
		t.Fatalf("pin started_at: %v", err)
	}
	if verifyOut != "" {
		if err := s.AppendStep(StepRow{RunID: rid, Seq: 13, Role: "verify", Status: "fail", OutputJSON: verifyOut}); err != nil {
			t.Fatalf("AppendStep: %v", err)
		}
	}
	if outcome != "" {
		if err := s.EndRun(rid, outcome); err != nil {
			t.Fatalf("EndRun: %v", err)
		}
	}
}

func TestRunHistoryByRef(t *testing.T) {
	s, err := Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	tid, err := s.InsertTask(TaskRow{IssueRef: "#9", Description: "d", TaskType: "feat"})
	if err != nil {
		t.Fatalf("InsertTask: %v", err)
	}
	// 两轮已终结 run（老 blocked 带 verify 驳回、新 done 无 verify step）、
	// 一轮 triage 附属 run、一轮未终结 run（当前正在跑的）。
	seedRun(t, s, tid, "2026-07-01T00:00:00Z", `{"passed":false,"detail":"缺 healthcheck endpoint"}`, "blocked")
	seedRun(t, s, tid, "2026-07-02T00:00:00Z", "", "done")
	seedRun(t, s, tid, "2026-07-03T00:00:00Z", "", "triage")
	seedRun(t, s, tid, "2026-07-04T00:00:00Z", "", "")

	rows, err := s.RunHistoryByRef("#9", 8)
	if err != nil {
		t.Fatalf("RunHistoryByRef: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 finished non-triage runs, got %d: %+v", len(rows), rows)
	}
	// 时间升序：老的 blocked 在前。
	if rows[0].Outcome != "blocked" || rows[1].Outcome != "done" {
		t.Fatalf("outcome 顺序错: %+v", rows)
	}
	if !strings.Contains(rows[0].VerifyOutput, "缺 healthcheck endpoint") {
		t.Fatalf("blocked run 应带最后一次 verify 的原始 output: %q", rows[0].VerifyOutput)
	}
	if rows[1].VerifyOutput != "" {
		t.Fatalf("无 verify step 的 run VerifyOutput 应为空: %q", rows[1].VerifyOutput)
	}
}

func TestRunHistoryByRefLimit(t *testing.T) {
	s, err := Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	tid, _ := s.InsertTask(TaskRow{IssueRef: "#L", Description: "d"})
	for _, day := range []string{"01", "02", "03"} {
		seedRun(t, s, tid, "2026-07-"+day+"T00:00:00Z", "", "blocked")
	}
	rows, err := s.RunHistoryByRef("#L", 2)
	if err != nil {
		t.Fatalf("RunHistoryByRef: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("limit=2 应返回 2 条, got %d", len(rows))
	}
	// limit 取「最近的 N 条」：返回的应是 07-02 与 07-03 两轮，且升序。
	if rows[0].RunID == rows[1].RunID {
		t.Fatal("返回了重复 run")
	}
}

func TestRunHistoryByRefEmpty(t *testing.T) {
	s, err := Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	rows, err := s.RunHistoryByRef("#nonexistent", 8)
	if err != nil {
		t.Fatalf("RunHistoryByRef: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("未知 ref 应返回空, got %+v", rows)
	}
}

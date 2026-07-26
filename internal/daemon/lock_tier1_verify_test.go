package daemon

import (
	"strings"
	"sync"
	"testing"

	"loop-eng/internal/state"
)

// TestTier1SingleInstanceLock pins 验收#1+#3：同一 repo 上第二个 AcquireLock 被拒并说明持锁者；
// 持锁者释放（Close fd = 进程退出的 flock 释放等价语义）后锁无残留、可重取。
func TestTier1SingleInstanceLock(t *testing.T) {
	dir := t.TempDir()

	first, err := AcquireLock(dir)
	if err != nil {
		t.Fatalf("first AcquireLock: %v", err)
	}
	defer first.Close()

	if _, err := AcquireLock(dir); err == nil {
		t.Fatal("second AcquireLock on a locked repo must fail (single-instance protection)")
	} else if !strings.Contains(err.Error(), "another") && !strings.Contains(err.Error(), "already running") {
		t.Fatalf("rejection error must explain *why* (another running daemon), got: %v", err)
	}

	// 释放 → 锁应消失（进程异常退出由内核回收 flock，与此等价）。
	if err := first.Close(); err != nil {
		t.Fatalf("close first lock: %v", err)
	}
	again, err := AcquireLock(dir)
	if err != nil {
		t.Fatalf("after release, AcquireLock must succeed (no stale residue): %v", err)
	}
	again.Close()
}

// TestTier1ClaimTaskOnlyOneWinner pins 验收#2：new→running 是条件更新，两路（含并发）抢同一 task
// 只能有一路 win。SQLite 串行化下，串行双调即足以钉死契约；再加 16 路并发 burst 兜底。
func TestTier1ClaimTaskOnlyOneWinner(t *testing.T) {
	st, err := state.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	id, err := st.InsertTask(state.TaskRow{IssueRef: "race-1", Description: "d"})
	if err != nil {
		t.Fatalf("InsertTask: %v", err)
	}

	// 串行：第一次 win，第二次 lose。
	w1, err := st.ClaimTask(id)
	if err != nil {
		t.Fatalf("first ClaimTask: %v", err)
	}
	w2, err := st.ClaimTask(id)
	if err != nil {
		t.Fatalf("second ClaimTask: %v", err)
	}
	if !w1 || w2 {
		t.Fatalf("sequential claim: exactly one winner expected, got first=%v second=%v", w1, w2)
	}

	// 并发 burst（新 task）：仍至多一个 win。
	id2, err := st.InsertTask(state.TaskRow{IssueRef: "race-2", Description: "d"})
	if err != nil {
		t.Fatalf("InsertTask#2: %v", err)
	}
	var wg sync.WaitGroup
	wins := make(chan bool, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := st.ClaimTask(id2)
			if err == nil {
				wins <- ok
			}
		}()
	}
	wg.Wait()
	close(wins)
	n := 0
	for w := range wins {
		if w {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("concurrent claim: exactly one winner expected, got %d", n)
	}
}

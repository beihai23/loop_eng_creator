package state

import (
	"strconv"
	"strings"
	"sync"
	"testing"
)

// TestTier1WALAndBusyTimeout pins the new Open contract: WAL journal mode + a
// non-zero busy_timeout on every pooled connection (set via DSN pragmas). This
// is what lets the dashboard process read state.db while the daemon writes, and
// lets the daemon's two write goroutines wait instead of erroring.
func TestTier1WALAndBusyTimeout(t *testing.T) {
	st, err := Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	var jm string
	if err := st.db.QueryRow(`PRAGMA journal_mode`).Scan(&jm); err != nil {
		t.Fatalf("journal_mode query: %v", err)
	}
	if jm != "wal" {
		t.Fatalf("journal_mode=%q want wal (Open must enable WAL)", jm)
	}

	var bt int
	if err := st.db.QueryRow(`PRAGMA busy_timeout`).Scan(&bt); err != nil {
		t.Fatalf("busy_timeout query: %v", err)
	}
	if bt < 1000 {
		t.Fatalf("busy_timeout=%dms want >=1000 (Open must set a busy timeout)", bt)
	}
}

// TestTier1ConcurrentWritesNoLock drives many goroutines through the Store's
// write API on one Open handle. Under -race it must show no data race and no
// "database is locked" / SQLITE_BUSY; every insert must persist. With the OLD
// Open (no busy_timeout) this fails with "database is locked".
func TestTier1ConcurrentWritesNoLock(t *testing.T) {
	st, err := Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	const N, M = 8, 50
	var wg sync.WaitGroup
	errs := make(chan error, N*M)
	for g := 0; g < N; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < M; i++ {
				id, err := st.InsertTask(TaskRow{
					IssueRef:    "g" + strconv.Itoa(g) + "-" + strconv.Itoa(i),
					Description: "d",
				})
				if err != nil {
					errs <- err
					continue
				}
				if err := st.AppendTransition(id, "new", "running", "tier1"); err != nil {
					errs <- err
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		s := err.Error()
		if strings.Contains(s, "locked") || strings.Contains(s, "busy") || strings.Contains(s, "BUSY") {
			t.Fatalf("concurrent write hit a lock error (WAL+busy_timeout not effective): %v", s)
		}
		t.Fatalf("unexpected write error: %v", err)
	}

	rows, err := st.ListStatuses()
	if err != nil {
		t.Fatalf("ListStatuses: %v", err)
	}
	if len(rows) != N*M {
		t.Fatalf("persisted=%d want %d (lost inserts under concurrency)", len(rows), N*M)
	}
}

package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// AcquireLock takes an exclusive, non-blocking flock on <repo>/.loop/daemon.lock
// for single-instance protection of the resident daemon. Two daemons pointed at
// the same repo would otherwise each pull the same FIFO head on the same tick and
// double-dispatch it — two worktrees, double token spend, racing FF-merges into
// main. SQLite WAL prevents data corruption but not the NextReadyTask→running
// read-modify-write race; this lock is the front door that keeps the second
// daemon from ever starting.
//
// The lock is flock(LOCK_EX|LOCK_NB) bound to the returned *os.File's fd. The
// caller holds that fd for the daemon's whole lifetime (the CLI RunE does
// `defer lockFile.Close()`); closing the fd OR the process exiting — cleanly or
// killed (SIGKILL, OOM, power) — lets the kernel reclaim the fd and release the
// flock automatically. There is no PID file to clean up and no stale residue on
// a crash, which is acceptance #3: flock semantics mean a restart always finds
// the lock free (held by no one) unless a live daemon genuinely owns it.
//
// On contention the returned error names the reason ("another loop-eng daemon is
// already running") plus the best-effort holding PID read from the lock file
// (empty/absent = "unknown"), so cobra surfaces to stderr exactly why the second
// instance was refused. On success the holder stamps its own PID into the file
// so a later contender can report whom to blame. ClaimTask is the back stop that
// still holds if this lock is bypassed (e.g. a hand-deleted lock file).
func AcquireLock(repo string) (*os.File, error) {
	// Defensive: the daemon boots after init has created .loop, but a bare `--repo`
	// against a fresh checkout (or a temp dir in tests) may not have it yet.
	if err := os.MkdirAll(filepath.Join(repo, ".loop"), 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(repo, ".loop", "daemon.lock")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		holder := readLockPID(f) // best-effort: who currently owns the lock?
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, fmt.Errorf(
				"another loop-eng daemon is already running on %s (holding pid %s) — refusing to start a second instance",
				repo, holder)
		}
		return nil, err
	}
	// We hold the lock. Stamp our PID so a contender can report the holder. We
	// rewrite rather than open with O_TRUNC: O_TRUNC would mutate the file BEFORE
	// we held the lock, racing a concurrent holder's content. Truncate under the
	// lock is safe and atomic w.r.t. other contenders.
	if err := f.Truncate(0); err != nil {
		f.Close()
		return nil, err
	}
	if _, err := f.Seek(0, 0); err != nil {
		f.Close()
		return nil, err
	}
	if _, err := fmt.Fprintf(f, "%d", os.Getpid()); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// readLockPID reads the holding PID a previous winner wrote into the lock file,
// best-effort. It only enriches the "already running" error so the user knows
// which process owns the lock. Any read/seek error or empty/garbage content →
// "unknown" (never blocks lock acquisition reporting). Reads from the same fd
// (already Seek'd to 0 by the caller path) — the page cache is coherent on the
// same host, so the just-written PID is visible without an fsync.
func readLockPID(f *os.File) string {
	if _, err := f.Seek(0, 0); err != nil {
		return "unknown"
	}
	var buf [32]byte
	n, err := f.Read(buf[:])
	if err != nil || n == 0 {
		return "unknown"
	}
	s := strings.TrimSpace(string(buf[:n]))
	if s == "" {
		return "unknown"
	}
	return s
}

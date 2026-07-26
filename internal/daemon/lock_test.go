package daemon

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestAcquireLockRejectsSecondInstance pins 验收#1：同一 repo 上第二个 AcquireLock
// 必须失败，且错误文案要说清原因（另一个 daemon 已在跑）。flock LOCK_EX|LOCK_NB 是
// fd 级独占，同一进程内第二个 fd 取不到也成立，故可在单测里直接验证。
func TestAcquireLockRejectsSecondInstance(t *testing.T) {
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
}

// TestAcquireLockReleasesOnClose pins 验收#3：持锁 fd Close 后锁无残留、可重取。
// flock 绑在 fd 上——Close 释放 fd 与进程退出时内核回收是等价语义，故无 PID 文件清理
// 问题、重启无 stale 残留。
func TestAcquireLockReleasesOnClose(t *testing.T) {
	dir := t.TempDir()

	first, err := AcquireLock(dir)
	if err != nil {
		t.Fatalf("first AcquireLock: %v", err)
	}
	// 持有期间第二个必失败。
	if _, err := AcquireLock(dir); err == nil {
		first.Close()
		t.Fatal("second AcquireLock must fail while first holds the lock")
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first lock: %v", err)
	}
	// 释放后必须能重取（无 stale 残留）。
	again, err := AcquireLock(dir)
	if err != nil {
		t.Fatalf("after release, AcquireLock must succeed (no stale residue): %v", err)
	}
	again.Close()
}

// TestAcquireLockStampsHolderPID 钉死锁文件写入持锁 PID：第二个实例被拒时错误文案
// 带当前进程 PID（best-effort，便于用户定位是谁占了锁）。
func TestAcquireLockStampsHolderPID(t *testing.T) {
	dir := t.TempDir()

	first, err := AcquireLock(dir)
	if err != nil {
		t.Fatalf("first AcquireLock: %v", err)
	}
	defer first.Close()

	want := strconv.Itoa(os.Getpid())
	got, err := os.ReadFile(filepath.Join(dir, ".loop", "daemon.lock"))
	if err != nil {
		t.Fatalf("read lock file: %v", err)
	}
	if strings.TrimSpace(string(got)) != want {
		t.Fatalf("lock file PID = %q, want %s", string(got), want)
	}

	// 第二个被拒时错误文案应带持锁 PID（同进程，故 PID 相同）。
	_, err = AcquireLock(dir)
	if err == nil {
		t.Fatal("second AcquireLock must fail")
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("rejection error should carry holder PID %s, got: %v", want, err)
	}
}

// TestAcquireLockCreatesLoopDir pins the defensive MkdirAll: a bare repo without
// a .loop dir must still acquire the lock (the daemon boots after init normally
// creates it, but a fresh checkout / temp dir may not have it yet).
func TestAcquireLockCreatesLoopDir(t *testing.T) {
	dir := t.TempDir()
	// No .loop dir present.
	if _, err := os.Stat(filepath.Join(dir, ".loop")); !os.IsNotExist(err) {
		t.Fatalf(".loop should not exist yet: %v", err)
	}
	first, err := AcquireLock(dir)
	if err != nil {
		t.Fatalf("AcquireLock on a repo without .loop: %v", err)
	}
	defer first.Close()
	if _, err := os.Stat(filepath.Join(dir, ".loop", "daemon.lock")); err != nil {
		t.Fatalf("daemon.lock should be created: %v", err)
	}
}

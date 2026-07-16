package isolation

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, c := range [][]string{
		{"git", "init", "-q"},
		{"git", "config", "user.email", "t@t"},
		{"git", "config", "user.name", "t"},
	} {
		if out, err := exec.Command(c[0], append(c[1:], dir)...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	mustWrite(t, filepath.Join(dir, "README"), "hi")
	for _, c := range [][]string{
		{"git", "-C", dir, "add", "-A"},
		{"git", "-C", dir, "commit", "-q", "-m", "init"},
	} {
		if out, err := exec.Command(c[0], c[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	return dir
}

func mustWrite(t *testing.T, p, body string) {
	if err := writeFile(p, body); err != nil {
		t.Fatal(err)
	}
}

func TestCreateAndDiscard(t *testing.T) {
	repo := initRepo(t)
	wt, err := Create(repo, "run-abc")
	if err != nil {
		t.Fatal(err)
	}
	if err := Discard(repo, wt); err != nil {
		t.Fatal(err)
	}
}

// TestCreateReturnsAbsolutePath pins the doc contract ("返回其绝对路径"):
// Create must return an absolute path even when baseRepo is relative — which
// is the run-once default (e.g. `--repo .`). Downstream callers feed the
// result straight into exec.Cmd.Dir and `git -C <repo> ... <wt>`, where a
// relative path is a latent bug (M1 final review: fix-before-merge).
func TestCreateReturnsAbsolutePath(t *testing.T) {
	t.Run("absolute baseRepo", func(t *testing.T) {
		repo := initRepo(t)
		wt, err := Create(repo, "run-abs")
		if err != nil {
			t.Fatal(err)
		}
		if !filepath.IsAbs(wt) {
			t.Fatalf("Create(absolute %q) = %q; want absolute path", repo, wt)
		}
		if err := Discard(repo, wt); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("relative baseRepo", func(t *testing.T) {
		repo := initRepo(t)
		// Create resolves a relative baseRepo against the process cwd, so chdir
		// into the repo first — mirrors `run-once --repo .` invoked from repo root.
		t.Chdir(repo)
		wt, err := Create(".", "run-rel")
		if err != nil {
			t.Fatal(err)
		}
		if !filepath.IsAbs(wt) {
			t.Fatalf("Create(%q) = %q; want absolute path", ".", wt)
		}
		// The absolute path must point at the real worktree, not just any abs path.
		if fi, err := os.Stat(wt); err != nil || !fi.IsDir() {
			t.Fatalf("returned path %q does not exist as a directory: %v", wt, err)
		}
		if err := Discard(".", wt); err != nil {
			t.Fatal(err)
		}
	})
}

// TestCreateToleratesLeftoverBranch: 一个残留的分支（上次 run 在 Discard 前崩溃、
// land 失败、或手工遗留）不得让下一次 Create 崩。用 -B 强制重置，而非 -b 的
// "already exists" 报错（#20 在 retry attempt 2 正是栽在这）。
func TestCreateToleratesLeftoverBranch(t *testing.T) {
	repo := initRepo(t)
	// 第一次 create，然后只删 worktree、留分支（模拟崩溃前没跑到 Discard）。
	wt, err := Create(repo, "run-leftover")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", repo, "worktree", "remove", "--force", wt).CombinedOutput(); err != nil {
		t.Fatalf("remove worktree (precondition): %v: %s", err, out)
	}
	// 前置确认：分支确实残留。
	if out, _ := exec.Command("git", "-C", repo, "branch", "--list", "loop/run-leftover").CombinedOutput(); strings.TrimSpace(string(out)) == "" {
		t.Fatal("precondition failed: leftover branch loop/run-leftover should still exist")
	}
	// 同 runID 再 create：-b 会崩 "already exists"；-B 应成功重置。
	wt2, err := Create(repo, "run-leftover")
	if err != nil {
		t.Fatalf("Create must tolerate leftover branch: %v", err)
	}
	if err := Discard(repo, wt2); err != nil {
		t.Fatal(err)
	}
}

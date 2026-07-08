// internal/cli/testhelper_test.go
package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// initGitRepo scaffolds an empty git repo at dir (git init + initial commit),
// mirroring the isolation test fixture. Used by init/run-once tests that need
// a real worktree base.
func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	for _, c := range [][]string{
		{"git", "init", "-q", dir},
		{"git", "-C", dir, "config", "user.email", "t@t"},
		{"git", "-C", dir, "config", "user.name", "t"},
	} {
		if out, err := exec.Command(c[0], c[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("hi\n"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, c := range [][]string{
		{"git", "-C", dir, "add", "-A"},
		{"git", "-C", dir, "commit", "-q", "-m", "init"},
	} {
		if out, err := exec.Command(c[0], c[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
}

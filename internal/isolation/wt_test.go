package isolation

import (
	"os/exec"
	"path/filepath"
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

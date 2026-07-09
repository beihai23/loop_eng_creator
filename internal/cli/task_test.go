package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTaskNewDetectsGoAndSuggestsCriteria(t *testing.T) {
	repo := t.TempDir()
	// Simulate a Go project
	os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module test\n"), 0644)

	root := NewRootCmd()
	root.SetArgs([]string{"task", "new", "fix the bug", "--repo", repo, "--type", "bugfix"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}

	// Check the inbox file was created with Go-specific criteria
	entries, _ := os.ReadDir(filepath.Join(repo, "inbox"))
	if len(entries) != 1 {
		t.Fatalf("expected 1 inbox file, got %d", len(entries))
	}
	body, _ := os.ReadFile(filepath.Join(repo, "inbox", entries[0].Name()))
	s := string(body)
	if !strings.Contains(s, "fix the bug") {
		t.Fatalf("task file missing description: %s", s)
	}
	if !strings.Contains(s, "go test ./...") {
		t.Fatalf("Go criteria not suggested: %s", s)
	}
	if !strings.Contains(s, "bugfix") {
		t.Fatalf("task type not set: %s", s)
	}
}

func TestTaskNewDetectsPython(t *testing.T) {
	repo := t.TempDir()
	os.WriteFile(filepath.Join(repo, "pyproject.toml"), []byte("[project]\n"), 0644)

	root := NewRootCmd()
	root.SetArgs([]string{"task", "new", "add feature", "--repo", repo})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(filepath.Join(repo, "inbox", "1.md"))
	if !strings.Contains(string(body), "pytest") {
		t.Fatalf("Python criteria not suggested: %s", body)
	}
}

func TestTaskNewUnknownLanguage(t *testing.T) {
	repo := t.TempDir()
	// No language markers
	root := NewRootCmd()
	root.SetArgs([]string{"task", "new", "do something", "--repo", repo})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(filepath.Join(repo, "inbox", "1.md"))
	if !strings.Contains(string(body), "verify.deterministic") {
		t.Fatalf("unknown language should suggest configuring verify: %s", body)
	}
}

func TestNextInboxNumber(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "1.md"), []byte("a"), 0644)
	os.WriteFile(filepath.Join(dir, "3.md"), []byte("b"), 0644)
	if n := nextInboxNumber(dir); n != 4 {
		t.Fatalf("expected 4, got %d", n)
	}
}

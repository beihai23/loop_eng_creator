package model

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTier1ReadonlyToolsPassthrough(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "argv")
	bin := writeTier1ArgRecorder(t, argsFile)
	c := NewClaudeClient(bin, "", []string{"--dangerously-skip-permissions", "--disallowedTools", "Edit", "Write", "NotebookEdit"})
	if _, _, err := c.Call(context.Background(), "plan"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--dangerously-skip-permissions", "--disallowedTools", "Edit", "Write", "NotebookEdit"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("missing %q in argv: %s", want, raw)
		}
	}
}

func writeTier1ArgRecorder(t *testing.T, argsFile string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-claude")
	script := `#!/bin/sh
cat >/dev/null
for a in "$@"; do echo "$a"; done > ` + argsFile + "\n"
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	return path
}
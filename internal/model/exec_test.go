package model

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeFakeBinary writes a script that discards stdin and prints the current
// working directory. This lets us verify that Exec sets cmd.Dir=worktreeDir
// without depending on a real claude binary.
func writeFakeBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	var path string
	var content string
	if runtime.GOOS == "windows" {
		path = filepath.Join(dir, "fake-claude.bat")
		content = "@echo off\r\nping -n 1 127.0.0.1 >nul\r\ncd\r\n"
	} else {
		path = filepath.Join(dir, "fake-claude")
		content = "#!/bin/sh\ncat >/dev/null\npwd\n"
	}
	if err := os.WriteFile(path, []byte(content), 0755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestClaudeClientExecRunsInWorktreeDir(t *testing.T) {
	bin := writeFakeBinary(t)
	wt := t.TempDir()
	c := NewClaudeClient(bin, []string{"--dangerously-skip-permissions"})
	out, _, err := c.Exec(context.Background(), wt, "do the task")
	if err != nil {
		t.Fatal(err)
	}
	// out is the cwd printed by the script; it must equal wt (proving cmd.Dir=wt).
	abs, _ := filepath.Abs(wt)
	if !strings.Contains(out, abs) {
		t.Fatalf("Exec should run in worktree %s, got %q", abs, out)
	}
}

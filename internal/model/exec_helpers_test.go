package model

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// writeFakePwdBinary writes a stand-in executable that ignores stdin/argv and
// prints its cwd. The opencode/kimi/kilo contract tests use it to assert the
// provider's Exec sets cmd.Dir=worktreeDir (the exec_test.go-equivalent
// contract) without a real binary. name disambiguates the on-disk script per
// provider so it never collides with a concurrently-present fake in the package.
//
// This is the positional-provider analogue of exec_codex_test.go's
// writeFakeCodexBinary; the three new providers take the prompt as a positional
// argv element (not stdin like codex), but the cmd.Dir contract is identical.
func writeFakePwdBinary(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	var path, content string
	if runtime.GOOS == "windows" {
		path = filepath.Join(dir, name+".bat")
		content = "@echo off\r\ncd\r\n"
	} else {
		path = filepath.Join(dir, name)
		content = "#!/bin/sh\ncat >/dev/null\npwd\n"
	}
	if err := os.WriteFile(path, []byte(content), 0755); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeFakeArgvBinary writes a stand-in executable that echoes every argv
// element (one per line). The opencode/kimi/kilo prompt-delivery contract tests
// use it to assert the prompt reaches the binary as a positional argv element —
// these providers take the prompt positionally (unlike codex, which reads stdin,
// exercised by TestCodexAgentDeliversPromptViaStdin's byte-counting fake).
func writeFakeArgvBinary(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	var path, content string
	if runtime.GOOS == "windows" {
		path = filepath.Join(dir, name+".bat")
		content = "@echo off\r\necho %*\r\n"
	} else {
		path = filepath.Join(dir, name)
		content = "#!/bin/sh\nprintf '%s\\n' \"$@\"\n"
	}
	if err := os.WriteFile(path, []byte(content), 0755); err != nil {
		t.Fatal(err)
	}
	return path
}

package model

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

// writeFakeFatalBinary writes a stand-in executable that consumes stdin,
// appends one tally line per invocation to <countDir>/count (mirrors
// retry_test.go's writeFakeBinaryFailing proven count pattern), prints msg to
// stdout via a single-quoted printf, and exits 1 — the exact shape of a
// provider emitting a fatal config/env/balance signal. Every provider's
// fatal-short-circuit test uses it to assert runWithRetry aborted after exactly
// 1 attempt with the error wrapped in ErrClaudeFatal. Returns the binary path
// and the count dir the caller reads <countDir>/count from.
func writeFakeFatalBinary(t *testing.T, name, msg string) (bin, countDir string) {
	t.Helper()
	countDir = t.TempDir()
	bin = filepath.Join(countDir, name)
	// Single-quote msg so printf emits it verbatim; escape any embedded single
	// quote via the standard '\'' shuffle (this is why this file imports strings).
	quoted := "'" + strings.ReplaceAll(msg, "'", `'\''`) + "'"
	script := "#!/bin/sh\ncat >/dev/null\n" +
		"N=$(cat \"" + countDir + "/count\" 2>/dev/null | wc -l | tr -d ' '); N=$((N+1)); printf \"%s\\n\" \"$N\" >> \"" + countDir + "/count\"\n" +
		"printf '%s\\n' " + quoted + "\n" +
		"exit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	return bin, countDir
}

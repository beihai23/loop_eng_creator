package model

// real_tokens_tier1_test.go 钉死 #71-A（claude 真实 token 采集）：
//  - `--output-format json` 信封被解析：Out = 最终文本（语义不变），Usage = 真实
//    token（不再是 len(Out) 估算）；
//  - 兜底：输出不是信封（旧版本/异常文本）→ Out 原文、Usage 退回估算，不致命。

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// writeEnvelopeBinary 写一个打印 `--output-format json` 成功信封的假 claude。
func writeEnvelopeBinary(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	var path string
	var content string
	if runtime.GOOS == "windows" {
		path = filepath.Join(dir, "fake-claude.bat")
		content = "@echo off\r\n" + body + "\r\n"
	} else {
		path = filepath.Join(dir, "fake-claude")
		content = "#!/bin/sh\ncat >/dev/null\n" + body + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTier1ClaudeRealTokensFromEnvelope(t *testing.T) {
	bin := writeEnvelopeBinary(t,
		`printf '%s' '{"type":"result","subtype":"success","is_error":false,`+
			`"result":"{\"plan\":[{\"step\":\"s\"}]}","session_id":"sess-1",`+
			`"usage":{"input_tokens":1234,"output_tokens":56}}'`)
	c := NewClaudeClient(bin, "", nil)
	out, u, err := c.Call(context.Background(), "plan something")
	if err != nil {
		t.Fatal(err)
	}
	// Out 语义不变：最终 assistant 文本（模型自己产的 JSON），不是信封
	if out != `{"plan":[{"step":"s"}]}` {
		t.Fatalf("Out should be the envelope's result text, got %q", out)
	}
	// 真实 token，不是 len(out) 估算
	if u.TokensIn != 1234 || u.TokensOut != 56 {
		t.Fatalf("want real usage (1234,56), got (%d,%d)", u.TokensIn, u.TokensOut)
	}
}

func TestTier1ClaudeRealTokensFallback(t *testing.T) {
	// 非信封输出（旧 claude / 异常路径）→ Out 原文、TokensOut 退回 len 估算，不报错
	bin := writeEnvelopeBinary(t, `printf 'plain model output'`)
	c := NewClaudeClient(bin, "", nil)
	out, u, err := c.Call(context.Background(), "q")
	if err != nil {
		t.Fatal(err)
	}
	if out != "plain model output" {
		t.Fatalf("fallback Out should be the raw text, got %q", out)
	}
	if u.TokensOut != len("plain model output") {
		t.Fatalf("fallback TokensOut should be len(Out)=%d, got %d", len("plain model output"), u.TokensOut)
	}
}

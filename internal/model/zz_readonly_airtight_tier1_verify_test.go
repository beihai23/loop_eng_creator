package model

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"loop-eng/internal/config"
)

// TestTier1ReadonlyAirtightProfile 钉死只读 profile 的气密契约：NewAgent 对
// ReadOnly=true 的 claude 角色产出的 argv 必须含 --permission-mode plan +
// --disallowedTools Edit Write NotebookEdit，且绝不含 --dangerously-skip-permissions
//（bypassPermissions 与 plan mode 互斥；只读赢）。即便 cmd 残留 bypass（用户误配），
// profile 也必须剥离它。plan mode 在权限层阻断所有写（含 Bash echo>/tmp/x 这类
// 仓库外绝对路径写）——已对真 claude 实测验证。
func TestTier1ReadonlyAirtightProfile(t *testing.T) {
	raw := recordArgv(t, config.ModelRef{
		Provider: "claude",
		Cmd:      []string{"--dangerously-skip-permissions"},
		ReadOnly: true,
	})
	for _, want := range []string{"--permission-mode", "plan", "--disallowedTools", "Edit", "Write", "NotebookEdit"} {
		if !strings.Contains(raw, want) {
			t.Fatalf("只读 argv 缺 %q: %s", want, raw)
		}
	}
	if strings.Contains(raw, "--dangerously-skip-permissions") {
		t.Fatalf("只读 argv 不得含 bypass（与 plan mode 互斥）: %s", raw)
	}
}

// TestTier1ReadonlyExecuteWritable 钉死 execute 不受只读 profile 影响：ReadOnly=false
// 时 argv 保留 --dangerously-skip-permissions、不被注入 plan mode。
func TestTier1ReadonlyExecuteWritable(t *testing.T) {
	raw := recordArgv(t, config.ModelRef{
		Provider: "claude",
		Cmd:      []string{"--dangerously-skip-permissions"},
		ReadOnly: false,
	})
	if !strings.Contains(raw, "--dangerously-skip-permissions") {
		t.Fatalf("execute argv 须保留 bypass（需要写）: %s", raw)
	}
	if strings.Contains(raw, "--permission-mode") {
		t.Fatalf("execute argv 不应被注入 plan mode: %s", raw)
	}
}

// recordArgv 用记录 argv 的 fake claude binary 跑一次 NewAgent(ref).Call，返回它
// 收到的完整 argv 文本。自包含，不依赖其它 _test.go 的 helper。
func recordArgv(t *testing.T, ref config.ModelRef) string {
	t.Helper()
	argsFile := filepath.Join(t.TempDir(), "argv")
	bin := filepath.Join(t.TempDir(), "fake-claude")
	script := "#!/bin/sh\ncat >/dev/null\nfor a in \"$@\"; do echo \"$a\"; done > " + argsFile + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ref.Binary = bin
	a, err := NewAgent(ref)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	if _, _, err := AsClient(a).Call(context.Background(), "explore"); err != nil {
		t.Fatalf("Call: %v", err)
	}
	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read argv: %v", err)
	}
	return string(raw)
}

package cli

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWebCmdDoesNotAdvertiseURLOnBindFailure 是 issue「loop-eng web 运行后访问链接
// 无法打开 / ERR_CONNECTION_REFUSED」修复的 tier-1 验收：web 命令必须在 net.Listen
// 绑定成功后才打印访问 URL，端口被占用时不得打印「打开浏览器访问」广告行而应非零退出。
// 修复前该测试 FAIL（URL 在 bind 前已打印，进程随后 bind 失败退出）；修复后 PASS。
func TestWebCmdDoesNotAdvertiseURLOnBindFailure(t *testing.T) {
	// 占住一个空闲端口，使 web 命令的 bind 必然失败。
	occ, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("setup: 占用端口失败: %v", err)
	}
	t.Cleanup(func() { _ = occ.Close() })
	addr := fmt.Sprintf("127.0.0.1:%d", occ.Addr().(*net.TCPAddr).Port)

	// 最小仓库：.loop/config.yaml（合法 providers）使 mustLoad/mustOpenState 成功，
	// 执行流能走到 bind 步骤（与 TestWebServerIntegration 同样的脚手架；state.Open 自建 DB）。
	repo := t.TempDir()
	initGitRepo(t, repo)
	if err := os.MkdirAll(filepath.Join(repo, ".loop"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".loop", "config.yaml"), []byte(defaultConfig), 0644); err != nil {
		t.Fatal(err)
	}

	cmd := NewWebCmd()
	out := &strings.Builder{}
	cmd.SetOut(out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--repo", repo, "--addr", addr})

	if err := cmd.Execute(); err == nil {
		t.Fatal("期望 bind 失败返回非 nil 错误（端口被占用），实际返回 nil")
	}
	if strings.Contains(out.String(), "打开浏览器访问") {
		t.Fatalf("bind 失败却打印了访问 URL——必须先绑定再广告。stdout=\n%s", out.String())
	}
}
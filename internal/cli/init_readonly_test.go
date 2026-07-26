// internal/cli/init_readonly_test.go
package cli

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"loop-eng/internal/config"
)

// TestDefaultConfigReadonlyToolset 钉死默认 config 的只读工具集（#84 只读纵深）。
// config.Load 解析 defaultConfig 后：
//   - triage / plan / verify 的 Cmd 同时含 --dangerously-skip-permissions 与
//     --disallowedTools Edit Write NotebookEdit（写工具从 agent 上下文物理移除，
//     「只读」由权限层强制，非仅靠 prompt 自觉）；
//   - execute 的 Cmd 仍只有 --dangerously-skip-permissions、不含 --disallowedTools
//     （它需要写）；
//   - 四角色 provider 仍为 claude（开箱行为不变）。
//
// 未来误改 defaultConfig（如漏掉某个 token、误给 execute 加 --disallowedTools、
// 改了 provider）会让此测试变红。
func TestDefaultConfigReadonlyToolset(t *testing.T) {
	var cfg config.Config
	if err := yaml.Unmarshal([]byte(defaultConfig), &cfg); err != nil {
		t.Fatalf("parse defaultConfig: %v", err)
	}

	// 三只读角色必须带的 token 集（精确元素，非子串）。
	readonlyWant := []string{
		"--dangerously-skip-permissions",
		"--disallowedTools",
		"Edit",
		"Write",
		"NotebookEdit",
	}
	for _, r := range []struct {
		name string
		ref  config.ModelRef
	}{
		{"triage", cfg.Models.Triage},
		{"plan", cfg.Models.Plan},
		{"verify", cfg.Models.Verify},
	} {
		if r.ref.Provider != "claude" {
			t.Errorf("models.%s.provider: want claude, got %q（开箱行为须不变）", r.name, r.ref.Provider)
		}
		for _, want := range readonlyWant {
			if !cmdHas(r.ref.Cmd, want) {
				t.Errorf("models.%s.cmd 缺少 %q（只读工具集）: got %v", r.name, want, r.ref.Cmd)
			}
		}
	}

	// execute 保留全部写权限：含 --dangerously-skip-permissions，但绝不含
	// --disallowedTools / Edit / Write / NotebookEdit。
	ex := cfg.Models.Execute
	if ex.Provider != "claude" {
		t.Errorf("models.execute.provider: want claude, got %q（开箱行为须不变）", ex.Provider)
	}
	if !cmdHas(ex.Cmd, "--dangerously-skip-permissions") {
		t.Errorf("models.execute.cmd 缺少 --dangerously-skip-permissions: got %v", ex.Cmd)
	}
	for _, disallowed := range []string{"--disallowedTools", "Edit", "Write", "NotebookEdit"} {
		if cmdHas(ex.Cmd, disallowed) {
			t.Errorf("models.execute.cmd 不应含 %q（execute 需要写权限）: got %v", disallowed, ex.Cmd)
		}
	}
}

// TestDefaultConfigEmbedsReadonlyMarkers 文本层兜底：defaultConfig 字符串本身
// 含只读标记 + provider: claude。与 zz_*_tier1 文本断言同思路——双保险，防
// YAML 结构被误改后上层测试仍侥幸通过。
func TestDefaultConfigEmbedsReadonlyMarkers(t *testing.T) {
	for _, want := range []string{
		"--dangerously-skip-permissions",
		"--disallowedTools",
		"Edit",
		"Write",
		"NotebookEdit",
		"provider: claude",
	} {
		if !strings.Contains(defaultConfig, want) {
			t.Fatalf("defaultConfig 文本缺少标记 %q", want)
		}
	}
}

// cmdHas 报告 Cmd（[]string）是否含精确元素 want。
func cmdHas(cmd []string, want string) bool {
	for _, c := range cmd {
		if c == want {
			return true
		}
	}
	return false
}

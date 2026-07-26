// internal/cli/init_readonly_test.go
package cli

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"loop-eng/internal/config"
)

// TestDefaultConfigReadonlyToolset 钉死默认 config 的气密只读契约
// （#airtight：plan/triage/verify 即便走 Bash 也写不到主仓库）。config.Load 解析
// defaultConfig 后：
//   - triage / plan / verify 的 ReadOnly==true、Cmd 含 --disallowedTools Edit
//     Write NotebookEdit、Cmd 绝不含 --dangerously-skip-permissions（bypass 与
//     plan mode 互斥，只读赢——profile 会注入 --permission-mode plan，故 config
//     不再带 bypass）；
//   - execute 的 ReadOnly==false、Cmd 含 --dangerously-skip-permissions、不含
//     --disallowedTools / plan（它需要写，不受只读 profile 影响）；
//   - 四角色 provider 仍为 claude（开箱行为不变）。
//
// 未来误改 defaultConfig（如漏掉 readonly、给只读角色留 bypass、误给 execute 加
// --disallowedTools、改了 provider）会让此测试变红。
func TestDefaultConfigReadonlyToolset(t *testing.T) {
	var cfg config.Config
	if err := yaml.Unmarshal([]byte(defaultConfig), &cfg); err != nil {
		t.Fatalf("parse defaultConfig: %v", err)
	}

	// 三只读角色：ReadOnly=true、带写工具黑名单、绝不含 bypass。
	readonlyCmdWant := []string{"--disallowedTools", "Edit", "Write", "NotebookEdit"}
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
		if !r.ref.ReadOnly {
			t.Errorf("models.%s.readonly: want true（只读 profile 触发位）", r.name)
		}
		for _, want := range readonlyCmdWant {
			if !cmdHas(r.ref.Cmd, want) {
				t.Errorf("models.%s.cmd 缺少 %q（写工具黑名单）: got %v", r.name, want, r.ref.Cmd)
			}
		}
		if cmdHas(r.ref.Cmd, "--dangerously-skip-permissions") {
			t.Errorf("models.%s.cmd 不得含 --dangerously-skip-permissions（与 plan mode 互斥）: got %v", r.name, r.ref.Cmd)
		}
	}

	// execute 保留全部写权限：ReadOnly=false、含 bypass、绝不含写工具黑名单 / plan。
	ex := cfg.Models.Execute
	if ex.Provider != "claude" {
		t.Errorf("models.execute.provider: want claude, got %q（开箱行为须不变）", ex.Provider)
	}
	if ex.ReadOnly {
		t.Errorf("models.execute.readonly: want false（execute 需要写，不受只读 profile 影响）")
	}
	if !cmdHas(ex.Cmd, "--dangerously-skip-permissions") {
		t.Errorf("models.execute.cmd 缺少 --dangerously-skip-permissions: got %v", ex.Cmd)
	}
	for _, disallowed := range []string{"--disallowedTools", "Edit", "Write", "NotebookEdit", "plan", "--permission-mode"} {
		if cmdHas(ex.Cmd, disallowed) {
			t.Errorf("models.execute.cmd 不应含 %q（execute 不被注入只读 profile）: got %v", disallowed, ex.Cmd)
		}
	}
}

// TestDefaultConfigEmbedsReadonlyMarkers 文本层兜底：defaultConfig 字符串本身
// 含只读标记 + provider: claude。与 zz_*_tier1 文本断言同思路——双保险，防
// YAML 结构被误改后上层测试仍侥幸通过。
func TestDefaultConfigEmbedsReadonlyMarkers(t *testing.T) {
	for _, want := range []string{
		"--dangerously-skip-permissions", // execute 行仍带（execute 可写）
		"--disallowedTools",
		"Edit",
		"Write",
		"NotebookEdit",
		"readonly: true",
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

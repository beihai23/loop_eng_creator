package cli

// version_test.go —— 版本解析优先级与启动横幅的验收测试。

import (
	"runtime/debug"
	"strings"
	"testing"
	"time"
)

// TestResolveVersionPriority 核验四级优先级：注入版本 → vcs 短 hash(+dirty) →
// 模块版本 → (dev)。
func TestResolveVersionPriority(t *testing.T) {
	vcs := &debug.BuildInfo{
		Main: debug.Module{Version: "(devel)"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "c0ffee1234567890abcdef"},
			{Key: "vcs.modified", Value: "false"},
		},
	}
	if got := resolveVersion("v1.2.3", vcs); got != "v1.2.3" {
		t.Errorf("注入版本必须最高优先: %q", got)
	}
	if got := resolveVersion("", vcs); got != "c0ffee1" {
		t.Errorf("vcs.revision 应截到 7 位: %q", got)
	}
	dirty := &debug.BuildInfo{
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "abcdef0"},
			{Key: "vcs.modified", Value: "true"},
		},
	}
	if got := resolveVersion("", dirty); got != "abcdef0+dirty" {
		t.Errorf("工作树脏应带 +dirty: %q", got)
	}
	mod := &debug.BuildInfo{Main: debug.Module{Version: "v0.9.0"}}
	if got := resolveVersion("", mod); got != "v0.9.0" {
		t.Errorf("无 vcs 时应回退模块版本: %q", got)
	}
	if got := resolveVersion("  ", nil); got != "(dev)" {
		t.Errorf("全空应回退 (dev)（注入值 trim 后为空视同未注入）: %q", got)
	}
}

// TestBannerContainsIdentity 核验横幅的身份要素：字标、版本、一句话定位、
// 作者、版权（年份动态）。
func TestBannerContainsIdentity(t *testing.T) {
	b := banner()
	for _, want := range []string{
		"██╗", "loop-eng", "beihai23", "MIT",
		"写代码 → 自测 → 收尾",
		"© " + time.Now().Format("2006"),
	} {
		if !strings.Contains(b, want) {
			t.Errorf("banner 缺少要素 %q", want)
		}
	}
}

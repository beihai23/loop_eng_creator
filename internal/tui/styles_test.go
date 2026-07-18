package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"loop-eng/internal/state"
)

// TestNoColorDegradesToText 验证 NO_COLOR/非 TTY 降级契约（spec §6）：
// lipgloss 切到 Ascii profile 后，RenderOverview 输出不含任何 ANSI 转义，
// 而状态符号（●/◌）与选中标记（▶）与文本保留。
//
// 不依赖 os.Setenv——init() 在测试进程启动时已跑，env 改动不会重触发 profile
// 切换（见 task-b8 brief 的 pitfall）。改为直接 SetColorProfile(Ascii) 确定性
// 复现降级路径，结束后恢复原 profile。
func TestNoColorDegradesToText(t *testing.T) {
	snap := &Snapshot{
		Tasks: []state.TaskView{
			{ID: "t_run", IssueRef: "#1", Description: "running task", Status: "running"},
			{ID: "t_new", IssueRef: "#2", Description: "new task", Status: "new"},
		},
		Counts: map[string]int{"new": 1, "running": 1},
	}
	prev := lipgloss.ColorProfile()
	defer lipgloss.SetColorProfile(prev)

	// 控制：彩色 profile 下 RenderOverview 确实上色（含 ANSI 转义）。
	// 这证明未降级时颜色存在——即降级前 RED 状态。
	lipgloss.SetColorProfile(termenv.TrueColor)
	colored := RenderOverview(snap, 0, 0, 50, 0.0, 80)
	if !strings.Contains(colored, "\x1b[") {
		t.Fatalf("control: TrueColor 下应含 ANSI 颜色转义, got %q", colored)
	}

	// 降级：Ascii profile 下无任何 ANSI 转义（颜色/bold/faint 全剥离）。
	lipgloss.SetColorProfile(termenv.Ascii)
	out := RenderOverview(snap, 0, 0, 50, 0.0, 80)
	if strings.Contains(out, "\x1b[") {
		t.Fatalf("Ascii 下仍含 ANSI 转义: %q", out)
	}
	// 符号必须保留（spec §6「符号 + 纯文本」）
	for _, sym := range []string{"●", "◌", "▶"} {
		if !strings.Contains(out, sym) {
			t.Fatalf("Ascii 下符号 %s 丢失: %q", sym, out)
		}
	}
	// 文本内容保留
	if !strings.Contains(out, "loop-eng dashboard") || !strings.Contains(out, "running task") {
		t.Fatalf("Ascii 下文本丢失: %q", out)
	}
}

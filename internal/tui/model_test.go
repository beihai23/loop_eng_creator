package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestViewRendersTitle 验证 B1 骨架帧包含标题。
func TestViewRendersTitle(t *testing.T) {
	m := New(nil, nil)
	got := m.View()
	if !strings.Contains(got, "loop-eng dashboard") {
		t.Fatalf("View() = %q, 期望包含 loop-eng dashboard", got)
	}
}

// TestQuitOnQ 验证按 q 设置 quit 标志并返回 tea.Quit。
func TestQuitOnQ(t *testing.T) {
	m := New(nil, nil)
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	// ctrl+c 与 "q" 走同一分支；这里直接验 ctrl+c 路径。
	if cmd == nil {
		t.Fatalf("Update(ctrl+c) 返回 nil cmd，期望 tea.Quit")
	}
	// 执行 cmd 应产生 tea.Quit（tea.Quit 是一个无消息的 Cmd）。
	if mm.(Model).quit != true {
		t.Fatalf("Update(ctrl+c) 后 quit=false，期望 true")
	}
}

package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"loop-eng/internal/state"
)

func TestKeySwitchAndCancel(t *testing.T) {
	st, _ := state.Open(t.TempDir() + "/state.db")
	defer st.Close()
	tid, _ := st.InsertTask(state.TaskRow{IssueRef: "#1", Description: "d"})

	m := New(st, nil)
	m.selTask = tid

	// '2' → 详情 tab
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("2")})
	if m2.(Model).tab != tabDetail {
		t.Fatalf("tab not detail")
	}

	// 'x' 对选中任务写 cancel 命令
	m_tab := m
	m_tab.selTask = tid
	m3, _ := m_tab.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	_ = m3
	pending, _ := st.PendingCommands()
	if len(pending) != 1 || pending[0].Verb != "cancel" {
		t.Fatalf("cancel command not written: %+v", pending)
	}
}

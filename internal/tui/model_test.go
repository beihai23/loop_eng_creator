package tui

import (
	"strings"
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

// TestTabKeyAutoSelectsCursorRow 守住：在总览直接按 t（没先按 Enter）时，
// 自动把光标行选为 selTask，否则轨迹/详情会落到「（未选中任务）」。
func TestTabKeyAutoSelectsCursorRow(t *testing.T) {
	st, _ := state.Open(t.TempDir() + "/state.db")
	defer st.Close()
	tid, _ := st.InsertTask(state.TaskRow{IssueRef: "#1", Description: "d"})

	m := New(st, nil)
	m.selIdx = 0
	m.snap, _ = ReadSnapshot(st, nil) // 模拟 dataTick 已加载快照
	if m.selTask != "" {
		t.Fatalf("前置：selTask 应为空")
	}

	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})
	mm := m2.(Model)
	if mm.tab != tabTrace {
		t.Fatalf("tab 不是 trace")
	}
	if mm.selTask != tid {
		t.Fatalf("'t' 应自动选中光标行；selTask=%q want %q", mm.selTask, tid)
	}
}

func TestQuitOnQAndCtrlC(t *testing.T) {
	msgs := []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune("q")},
		{Type: tea.KeyCtrlC},
	}
	for i, msg := range msgs {
		m := New(nil, nil)
		m2, cmd := m.Update(msg)
		mm, ok := m2.(Model)
		if !ok || !mm.quit {
			t.Fatalf("msg %d (%s): quit not set", i, msg.String())
		}
		if cmd == nil {
			t.Fatalf("msg %d (%s): cmd nil, want tea.Quit", i, msg.String())
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Fatalf("msg %d (%s): cmd() not tea.QuitMsg", i, msg.String())
		}
	}
}

func TestViewDispatchesOverview(t *testing.T) {
	st, err := state.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// one new task so the snapshot is non-empty
	st.InsertTask(state.TaskRow{IssueRef: "#1", Description: "smoke"})

	m := New(st, nil)
	out := m.View()
	// overview renders the title + counts bar
	if !strings.Contains(out, "loop-eng dashboard") || !strings.Contains(out, "待处理") {
		t.Fatalf("overview dispatch output missing title/counts:\n%s", out)
	}
}

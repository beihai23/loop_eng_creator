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

// TestKeyResumeOnCancelled 锁定「在 cancelled 任务上按 r 会写 resume 命令」，
// 防止日后给 r 键加状态门槛把 cancelled 重新挡掉（cancel 后反悔的自助通道）。
// TUI 代码本身无需改动：model.go 的 r 键对 issueResume 无状态门槛。
func TestKeyResumeOnCancelled(t *testing.T) {
	st, _ := state.Open(t.TempDir() + "/state.db")
	defer st.Close()
	tid, _ := st.InsertTask(state.TaskRow{IssueRef: "#1", Description: "d"})
	_ = st.AppendTransition(tid, "new", "cancelled", "cancelled by TUI")

	m := New(st, nil)
	m.selTask = tid

	mr, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	_ = mr
	pending, _ := st.PendingCommands()
	if len(pending) != 1 || pending[0].Verb != "resume" || pending[0].TaskID != tid {
		t.Fatalf("resume command not written for cancelled task: %+v", pending)
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

// TestDetailScrollKeys 钉死详情 tab 的滚动：j/k 在详情 tab 滚动 detailScroll 而非移动
// 总览光标；偏移钳在 [0, 内容行数-height]；进入详情/回总览时归零。
func TestDetailScrollKeys(t *testing.T) {
	st, _ := state.Open(t.TempDir() + "/state.db")
	defer st.Close()
	tid, _ := st.InsertTask(state.TaskRow{IssueRef: "#1", Description: "d"})
	rid, _ := st.StartRun(tid)
	// 造一个 60 行的 plan prompt，保证内容高于窗口（height=10）。
	var lines []string
	for i := 0; i < 60; i++ {
		lines = append(lines, "prompt line")
	}
	_ = st.AppendStep(state.StepRow{RunID: rid, Seq: 11, Role: "plan", Status: "ok",
		InputJSON: strings.Join(lines, "\n")})

	key := func(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

	m := New(st, nil)
	m.selTask = tid
	m.tab = tabDetail
	m.height = 10

	// j 滚动（不移动总览光标）。
	m2, _ := m.Update(key("j"))
	mm := m2.(Model)
	if mm.detailScroll != 1 || mm.selIdx != 0 {
		t.Fatalf("j in detail: detailScroll=%d selIdx=%d, want 1/0", mm.detailScroll, mm.selIdx)
	}
	// k 回滚。
	m3, _ := mm.Update(key("k"))
	if m3.(Model).detailScroll != 0 {
		t.Fatalf("k: detailScroll=%d, want 0", m3.(Model).detailScroll)
	}
	// k 在顶部保持 0。
	m4, _ := m3.(Model).Update(key("k"))
	if m4.(Model).detailScroll != 0 {
		t.Fatalf("k at top: detailScroll=%d, want 0", m4.(Model).detailScroll)
	}
	// 连按 j 到底：钳在 maxOff，不「惯性过卷」。
	var cur tea.Model = m4
	for i := 0; i < 200; i++ {
		cur, _ = cur.(Model).Update(key("j"))
	}
	got := cur.(Model).detailScroll
	total := len(strings.Split(RenderDetail(st, nil, tid), "\n"))
	if want := total - 10; got != want {
		t.Fatalf("overscroll: detailScroll=%d, want clamped to %d (lines %d - height 10)", got, want, total)
	}
	// 重进详情（按 2）归零。
	re := cur.(Model)
	re.snap, _ = ReadSnapshot(st, nil)
	m5, _ := re.Update(key("2"))
	if m5.(Model).detailScroll != 0 {
		t.Fatalf("re-enter detail: detailScroll=%d, want 0", m5.(Model).detailScroll)
	}
}

// TestWindowLines 钉死窗口裁剪：offset 超出内容收缩后的范围时显示侧钳制（不渲染空白屏）；
// height<=0 返回全文。
func TestWindowLines(t *testing.T) {
	content := "a\nb\nc\nd\ne"
	if got := windowLines(content, 1, 2); got != "b\nc" {
		t.Fatalf("windowLines(1,2) = %q, want %q", got, "b\\nc")
	}
	// 内容 5 行、窗口 3 行：offset=9 钳到 2 → c\nd\ne
	if got := windowLines(content, 9, 3); got != "c\nd\ne" {
		t.Fatalf("windowLines(9,3) = %q, want %q", got, "c\\nd\\ne")
	}
	// height<=0（非 TTY）不裁剪。
	if got := windowLines(content, 3, 0); got != content {
		t.Fatalf("windowLines(h=0) = %q, want full content", got)
	}
}

package channel

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLocalListAndComment(t *testing.T) {
	dir := t.TempDir()
	inbox := filepath.Join(dir, "inbox")
	os.MkdirAll(inbox, 0755)
	os.WriteFile(filepath.Join(inbox, "1.md"), []byte("# 任务\nfix login\ntype: bugfix\n## 验收标准\n- [ ] login 200"), 0644)

	c := NewLocal(dir)
	tasks, err := c.ListNewTasks(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].Description != "fix login" {
		t.Fatalf("got %+v", tasks)
	}
	// CreatedAt is filled from the inbox file's mtime (drives FIFO by submission
	// time, not ingest order); empty would mean the channel forgot to set it.
	if tasks[0].CreatedAt == "" {
		t.Fatalf("CreatedAt empty: %+v", tasks[0])
	}
	if err := c.PostComment(nil, tasks[0].Ref, "战报 round1: done"); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "outbox", tasks[0].Ref+".md"))
	if string(body) == "" {
		t.Fatal("comment not written")
	}
}

// TestLocalParsesAgentOverride: the `agent:` frontmatter line opts a task into a
// different coding-agent provider (e.g. `agent: codex`). parseLocalTask must
// surface it on Task.Agent; absent → "". This is the channel half of the
// task-level agent override (the daemon/state half round-trips it via tasks.agent).
func TestLocalParsesAgentOverride(t *testing.T) {
	with := parseLocalTask("# 任务\ndo thing\nagent: codex\n- [ ] c")
	if with.Agent != "codex" {
		t.Fatalf("agent: codex must parse to Task.Agent=codex, got %q", with.Agent)
	}
	if with.Description != "do thing" {
		t.Fatalf("agent line must not clobber Description, got %q", with.Description)
	}

	without := parseLocalTask("# 任务\ndo thing\n- [ ] c")
	if without.Agent != "" {
		t.Fatalf("no agent line must yield empty Agent, got %q", without.Agent)
	}
}

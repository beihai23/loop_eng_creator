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
	if err := c.PostComment(nil, tasks[0].Ref, "战报 round1: done"); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "outbox", tasks[0].Ref+".md"))
	if string(body) == "" {
		t.Fatal("comment not written")
	}
}

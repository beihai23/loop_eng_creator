package channel

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Local struct {
	Root string
	// InboxDir is the inbox path relative to Root; empty falls back to "inbox"
	// (zero-value compatible — NewLocal callers and existing tests unchanged).
	InboxDir string
}

func NewLocal(root string) *Local { return &Local{Root: root} }

func (l *Local) ListNewTasks(_ context.Context) ([]Task, error) {
	sub := l.InboxDir
	if sub == "" {
		sub = "inbox"
	}
	inbox := filepath.Join(l.Root, sub)
	ents, err := os.ReadDir(inbox)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var tasks []Task
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(inbox, e.Name()))
		if err != nil {
			continue
		}
		t := parseLocalTask(string(raw))
		t.Ref = strings.TrimSuffix(e.Name(), ".md")
		// 提交时间用 inbox 文件的 mtime（回落 now）——驱动 FIFO 按提交时间排序，
		// 与 ingest 顺序解耦（和 github 通道用 issue.createdAt 对齐）。
		t.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		if fi, err := e.Info(); err == nil {
			t.CreatedAt = fi.ModTime().UTC().Format(time.RFC3339Nano)
		}
		tasks = append(tasks, t)
	}
	return tasks, nil
}

func parseLocalTask(raw string) Task {
	var t Task
	t.Body = raw // 全文保留：解析出的 desc/criteria 是蒸馏，原文（背景/约束）不丢
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "type:"):
			t.TaskType = strings.TrimSpace(strings.TrimPrefix(line, "type:"))
		case strings.HasPrefix(line, "- [ ]"):
			t.AcceptanceCriteria = append(t.AcceptanceCriteria, strings.TrimSpace(strings.TrimPrefix(line, "- [ ]")))
		case line != "" && !strings.HasPrefix(line, "#") && t.Description == "":
			t.Description = line
		}
	}
	return t
}

func (l *Local) ListReplies(_ context.Context, _ []string, _ time.Time) (map[string][]Reply, error) {
	return nil, nil
}

func (l *Local) PostComment(_ context.Context, ref, body string) error {
	dir := filepath.Join(l.Root, "outbox")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, ref+".md"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(body + "\n")
	return err
}

func (l *Local) UpdateStatus(_ context.Context, ref, status string) error {
	dir := filepath.Join(l.Root, "status")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, ref), []byte(status), 0644)
}

func (l *Local) CloseIssue(_ context.Context, _ string) error { return nil }

func (l *Local) GetTaskStates(_ context.Context, _ []string) (map[string]TaskState, error) {
	return nil, nil
}

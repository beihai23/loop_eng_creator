package channel

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

type Local struct{ Root string }

func NewLocal(root string) *Local { return &Local{Root: root} }

func (l *Local) ListNewTasks(_ context.Context) ([]Task, error) {
	inbox := filepath.Join(l.Root, "inbox")
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
		tasks = append(tasks, t)
	}
	return tasks, nil
}

func parseLocalTask(raw string) Task {
	var t Task
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

func (l *Local) ListReplies(_ context.Context, _ []string) (map[string][]Reply, error) {
	return nil, nil // M1 不需要回复（无 daemon）；M2/M3 实现
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

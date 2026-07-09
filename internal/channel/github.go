package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
)

// GitHub is a channel.Channel backed by the authenticated `gh` CLI (no SDK,
// no stored token — reuses the operator's `gh auth`). Issues with TaskLabel
// are tasks; comments are battle reports; status moves via labels loop:<status>.
type GitHub struct {
	Repo      string // owner/name
	TaskLabel string // e.g. "loop:task"
}

func NewGitHub(repo, taskLabel string) *GitHub {
	return &GitHub{Repo: repo, TaskLabel: taskLabel}
}

type ghIssue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
}

// parseIssuesJSON decodes `gh issue list --json` output into Tasks.
func parseIssuesJSON(raw []byte) ([]Task, error) {
	var issues []ghIssue
	if err := json.Unmarshal(raw, &issues); err != nil {
		return nil, fmt.Errorf("parse gh issues: %w", err)
	}
	var tasks []Task
	for _, is := range issues {
		t := parseLocalTask(is.Body) // 复用 M1 的 body 解析（## 任务/type:/- [ ]）
		t.Ref = strconv.Itoa(is.Number)
		if t.Description == "" {
			t.Description = is.Title // body 没解析出 desc 则回落 title
		}
		tasks = append(tasks, t)
	}
	return tasks, nil
}

func (g *GitHub) ListNewTasks(ctx context.Context) ([]Task, error) {
	out, err := g.gh(ctx, "issue", "list", "--repo", g.Repo,
		"--label", g.TaskLabel, "--state", "open",
		"--json", "number,title,body", "--limit", "50")
	if err != nil {
		return nil, err
	}
	return parseIssuesJSON(out)
}

func (g *GitHub) PostComment(ctx context.Context, ref, body string) error {
	_, err := g.gh(ctx, "issue", "comment", ref, "--repo", g.Repo, "--body", body)
	return err
}

func (g *GitHub) UpdateStatus(ctx context.Context, ref, status string) error {
	_, err := g.gh(ctx, "issue", "edit", ref, "--repo", g.Repo, "--add-label", "loop:"+status)
	return err
}

func (g *GitHub) ListReplies(ctx context.Context, refs []string) (map[string][]Reply, error) {
	// M2 单次 run-once 不需要回复（无 daemon/park）；返回空。M3 daemon 实现真回复拉取。
	return map[string][]Reply{}, nil
}

// gh runs a `gh` command and returns stdout. stderr is folded into the error.
func (g *GitHub) gh(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	var out, errBuf outBuf
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("gh %v: %w: %s", args, err, errBuf.String())
	}
	return out.Bytes(), nil
}

type outBuf struct{ b []byte }

func (b *outBuf) Write(p []byte) (int, error) { b.b = append(b.b, p...); return len(p), nil }
func (b *outBuf) Bytes() []byte               { return b.b }
func (b *outBuf) String() string              { return string(b.b) }

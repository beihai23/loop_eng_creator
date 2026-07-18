package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ghRetry is the number of attempts gh() makes per call. The GitHub API
// intermittently TLS-timeouts from some networks; retrying absorbs the blip so a
// single transient ListNewTasks/PostComment failure doesn't abort the whole run.
const ghRetry = 3

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
	Number    int    `json:"number"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	CreatedAt string `json:"createdAt"`
}

// ghComment is the slice of `gh issue view <ref> --json comments`; only body
// and createdAt are carried — Replies are body-only by design, but createdAt is
// used client-side to filter for new comments since the last daemon reply.
type ghComment struct {
	Body      string `json:"body"`
	CreatedAt string `json:"createdAt"`
}

type ghIssueComments struct {
	Comments []ghComment `json:"comments"`
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
		t.CreatedAt = is.CreatedAt // issue 提交时间（RFC3339），驱动 FIFO 按提交时间排序
		if t.Description == "" {
			t.Description = is.Title // body 没解析出 desc 则回落 title
		}
		tasks = append(tasks, t)
	}
	return tasks, nil
}

// parseIssueCommentsJSON decodes `gh issue view <ref> --json comments` output
// into Replies — one Reply per comment, body verbatim (other fields like
// author/createdAt are ignored). M3 daemon polls these to surface human-review
// responses on parked tasks. When since is non-zero, only comments created after
// that time are included (used to detect new human replies after the daemon's
// last comment).
func parseIssueCommentsJSON(raw []byte, since time.Time) ([]Reply, error) {
	var c ghIssueComments
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse gh issue comments: %w", err)
	}
	replies := make([]Reply, 0, len(c.Comments))
	for _, cm := range c.Comments {
		if !since.IsZero() {
			t, err := time.Parse(time.RFC3339, cm.CreatedAt)
			if err != nil || !t.After(since) {
				continue
			}
		}
		replies = append(replies, Reply{Body: cm.Body})
	}
	return replies, nil
}

func (g *GitHub) ListNewTasks(ctx context.Context) ([]Task, error) {
	out, err := g.gh(ctx, "issue", "list", "--repo", g.Repo,
		"--label", g.TaskLabel, "--state", "open",
		"--json", "number,title,body,createdAt", "--limit", "50")
	if err != nil {
		return nil, err
	}
	return parseIssuesJSON(out)
}

func (g *GitHub) PostComment(ctx context.Context, ref, body string) error {
	_, err := g.gh(ctx, "issue", "comment", ref, "--repo", g.Repo, "--body", body)
	return err
}

// statusLabelsToRemove 算出 UpdateStatus 需要先摘掉的旧状态标签：labels 中所有
// 带 "loop:" 前缀、既不等于 taskLabel（loop:task 是任务身份证，永不动）也不等于
// "loop:"+newStatus（正要打上的新标签）的标签。状态标签互斥——任意时刻一个 issue
// 最多一个 loop:<status>（修 #30 同时挂 loop:done + loop:blocked 的可信度问题）。
// 纯函数，便于 tier-1 直接钉互斥语义；签名固定，测试与之对齐。
func statusLabelsToRemove(labels []string, newStatus, taskLabel string) []string {
	keep := "loop:" + newStatus
	var out []string
	for _, l := range labels {
		if !strings.HasPrefix(l, "loop:") {
			continue // 非 loop 体系的标签（人打的、仓库自有的）一律不碰
		}
		if l == taskLabel || l == keep {
			continue
		}
		out = append(out, l)
	}
	return out
}

// UpdateStatus 把 issue 的状态标签换成 loop:<status>：先读现有标签，用
// statusLabelsToRemove 算出要摘的旧状态标签，--add-label 与 --remove-label 一次
// edit 完成（互斥：任意时刻最多一个 loop:<status>；loop:task 保留不动）。
//
// 容错（与写回容错一致，不翻转任务结局）：
//   - 读标签失败 → 退化为只加不摘（旧行为），打标成功优先于互斥洁癖；
//   - edit 带 --remove-label 失败（gh 对「已不在 issue 上的标签」报错，view→edit
//     之间标签被人摘掉即此竞态）→ 退化为只加不摘重试一次。
func (g *GitHub) UpdateStatus(ctx context.Context, ref, status string) error {
	add := "loop:" + status
	remove := g.currentStatusLabelsToRemove(ctx, ref, status)
	args := []string{"issue", "edit", ref, "--repo", g.Repo, "--add-label", add}
	for _, l := range remove {
		args = append(args, "--remove-label", l)
	}
	if _, err := g.gh(ctx, args...); err != nil {
		if len(remove) == 0 {
			return err
		}
		if _, fallbackErr := g.gh(ctx, "issue", "edit", ref, "--repo", g.Repo, "--add-label", add); fallbackErr != nil {
			return err // 回报原始错误（带 remove 上下文，更可诊断）
		}
	}
	return nil
}

// currentStatusLabelsToRemove 读 issue 现有标签并算出该摘的旧状态标签。读失败
// 返回 nil（调用方退化为只加不摘）——读标签是互斥的优化，不是打标的前置门槛。
func (g *GitHub) currentStatusLabelsToRemove(ctx context.Context, ref, status string) []string {
	raw, err := g.gh(ctx, "issue", "view", ref, "--repo", g.Repo, "--json", "labels")
	if err != nil {
		return nil
	}
	var v ghIssueView
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	labels := make([]string, 0, len(v.Labels))
	for _, l := range v.Labels {
		labels = append(labels, l.Name)
	}
	return statusLabelsToRemove(labels, status, g.TaskLabel)
}

func (g *GitHub) CloseIssue(ctx context.Context, ref string) error {
	_, err := g.gh(ctx, "issue", "close", ref, "--repo", g.Repo)
	return err
}

func (g *GitHub) ListReplies(ctx context.Context, refs []string, since time.Time) (map[string][]Reply, error) {
	out := make(map[string][]Reply, len(refs))
	for _, ref := range refs {
		raw, err := g.gh(ctx, "issue", "view", ref, "--repo", g.Repo, "--json", "comments")
		if err != nil {
			return nil, err
		}
		replies, err := parseIssueCommentsJSON(raw, since)
		if err != nil {
			return nil, err
		}
		out[ref] = replies
	}
	return out, nil
}

type ghIssueView struct {
	State  string         `json:"state"`
	Labels []ghIssueLabel `json:"labels"`
}

type ghIssueLabel struct {
	Name string `json:"name"`
}

func (g *GitHub) GetTaskStates(ctx context.Context, refs []string) (map[string]TaskState, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	out := make(map[string]TaskState, len(refs))
	for _, ref := range refs {
		raw, err := g.gh(ctx, "issue", "view", ref, "--repo", g.Repo, "--json", "state,labels")
		if err != nil {
			return nil, err
		}
		var v ghIssueView
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, fmt.Errorf("parse gh issue state: %w", err)
		}
		labels := make([]string, 0, len(v.Labels))
		for _, l := range v.Labels {
			labels = append(labels, l.Name)
		}
		out[ref] = TaskState{Ref: ref, IsOpen: v.State == "OPEN", Labels: labels}
	}
	return out, nil
}

// gh runs a `gh` command (retried on transient failure) and returns stdout.
// stderr is folded into the error. The GitHub API intermittently TLS-timeouts
// from some networks; retrying (ghRetry ×, 2s/4s backoff) absorbs those blips so
// a single ListNewTasks/PostComment failure doesn't abort the whole run.
func (g *GitHub) gh(ctx context.Context, args ...string) ([]byte, error) {
	var lastErr error
	for attempt := 1; attempt <= ghRetry; attempt++ {
		cmd := exec.CommandContext(ctx, "gh", args...)
		var out, errBuf outBuf
		cmd.Stdout = &out
		cmd.Stderr = &errBuf
		if err := cmd.Run(); err == nil {
			return out.Bytes(), nil
		} else {
			lastErr = fmt.Errorf("gh %v: %w: %s", args, err, errBuf.String())
		}
		if attempt < ghRetry {
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
		}
	}
	return nil, lastErr
}

type outBuf struct{ b []byte }

func (b *outBuf) Write(p []byte) (int, error) { b.b = append(b.b, p...); return len(p), nil }
func (b *outBuf) Bytes() []byte               { return b.b }
func (b *outBuf) String() string              { return string(b.b) }

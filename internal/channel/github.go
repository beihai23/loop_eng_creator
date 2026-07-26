package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ghRetry is the number of attempts gh() makes per call. The GitHub API
// intermittently TLS-timeouts from some networks; retrying absorbs the blip so a
// single transient ListNewTasks/PostComment failure doesn't abort the whole run.
const ghRetry = 3

// loopStatusNames is the loop:<status> family the engine moves tasks through —
// the channel-visible outcomes the daemon reports via UpdateStatus / report()
// (see internal/loop/subloop.go and internal/daemon/engine.go). EnsureLabels
// prepends "loop:" and creates each as a repo label so UpdateStatus's
// --add-label never 404s on a missing label (#46/#54). Add a status here when
// the engine grows a new channel-visible outcome.
var loopStatusNames = []string{
	"running",
	"needs-review",
	"needs-info",
	"needs-human-decision",
	"blocked",
	"done",
	"cancelled",
}

// GitHub is a channel.Channel backed by the authenticated `gh` CLI (no SDK,
// no stored token — reuses the operator's `gh auth`). Issues with TaskLabel
// are tasks; comments are battle reports; status moves via labels <prefix><status>.
type GitHub struct {
	Repo      string // owner/name
	TaskLabel string // e.g. "loop:task"
	// LabelPrefix 是状态标签族前缀（空 = "loop:"）。多实例共存时各实例用各自
	// 前缀（"ai:" → ai:running…）——互斥剥离只碰自己前缀的标签，不碰别家。
	LabelPrefix string

	// ghFunc, when non-nil, replaces the real `gh` exec for every call. It is
	// the test seam — production leaves it nil so gh() runs the real retried
	// exec (ghExec). Set by tests to record calls and return canned responses.
	ghFunc func(ctx context.Context, args ...string) ([]byte, error)

	// ensureOnce guards EnsureLabels so the (best-effort) label-create fan-out
	// runs at most once per GitHub instance — lazily from the first UpdateStatus.
	ensureOnce sync.Once
}

// prefix returns the effective status-label prefix ("loop:" when unset).
func (g *GitHub) prefix() string {
	if g.LabelPrefix != "" {
		return g.LabelPrefix
	}
	return "loop:"
}

// StatusLabel renders the repo label for one loop status (e.g. "loop:running",
// or "ai:running" under a custom prefix). It backs channel.StatusLabeler so the
// daemon never hardcodes the label family.
func (g *GitHub) StatusLabel(status string) string { return g.prefix() + status }

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
// 带本实例 prefix（loop: 或自定义前缀）、既不等于 taskLabel（任务身份证，永不动）
// 也不等于 prefix+newStatus（正要打上的新标签）的标签。状态标签互斥——任意时刻
// 一个 issue 最多一个 <prefix><status>（修 #30 同时挂两个状态标签的可信度问题）。
// 别家前缀（如另一实例的 loop:）一律不碰——多实例共存时互不误删（#72 评审发现
// 的边缘 bug：按硬编码 "loop:" 剥会误删别实例的任务身份证）。
// 纯函数，便于 tier-1 直接钉互斥语义；签名固定，测试与之对齐。
func statusLabelsToRemove(labels []string, newStatus, taskLabel, prefix string) []string {
	if prefix == "" {
		prefix = "loop:"
	}
	keep := prefix + newStatus
	var out []string
	for _, l := range labels {
		if !strings.HasPrefix(l, prefix) {
			continue // 非本实例前缀的标签（人打的、仓库自有的、别实例的）一律不碰
		}
		if l == taskLabel || l == keep {
			continue
		}
		out = append(out, l)
	}
	return out
}

// EnsureLabels idempotently creates the full loop:<status> label set (plus the
// task identity label) in the repo via `gh label create --force`. It is
// best-effort: each create is independent and errors are logged, not returned —
// a failing create is common when the operator lacks the "labels:write" scope,
// and in that case UpdateStatus still proceeds and logs its own failure. Runs at
// most once per GitHub instance (sync.Once); UpdateStatus calls it lazily so a
// missing label never 404s the add even when no one remembered to seed the repo.
func (g *GitHub) EnsureLabels(ctx context.Context) {
	g.ensureOnce.Do(func() {
		names := make([]string, 0, len(loopStatusNames)+1)
		for _, s := range loopStatusNames {
			names = append(names, g.prefix()+s)
		}
		if g.TaskLabel != "" {
			names = append(names, g.TaskLabel)
		}
		for _, name := range names {
			if _, err := g.gh(ctx, "label", "create", name, "--repo", g.Repo, "--force"); err != nil {
				log.Printf("channel/github: ensure label %q on %s failed: %v (continuing; UpdateStatus will still attempt)", name, g.Repo, err)
			}
		}
	})
}

// UpdateStatus 把 issue 的状态标签换成 loop:<status>。两条独立的 gh issue edit，
// 顺序执行：先 remove 旧状态标签，再 add 新标签——add 失败不回滚 remove，至少
// 旧状态标签能清掉（修 #54：旧版 add+remove 原子 edit，add 因标签缺失 404 时连带
// remove 也没执行，#54 一直挂着 loop:blocked）。
//
// 开头先 EnsureLabels（幂等、只跑一次），让 add-label 的目标标签存在；即便建标
// 没权限（降级），remove 仍照常先行。
//
// remove 的容错：view→edit 之间标签被人摘掉会让 remove 报「已不在 issue 上」，
// 此错误只记日志、不阻断 add（remove 是互斥优化，不是打标前置门槛）。
func (g *GitHub) UpdateStatus(ctx context.Context, ref, status string) error {
	g.EnsureLabels(ctx)

	add := g.prefix() + status
	remove := g.currentStatusLabelsToRemove(ctx, ref, status)

	// remove first, in its own edit. Best-effort: a stale/racy remove must not
	// block the add — the old status label is at least cleared either way.
	if len(remove) > 0 {
		rmArgs := []string{"issue", "edit", ref, "--repo", g.Repo}
		for _, l := range remove {
			rmArgs = append(rmArgs, "--remove-label", l)
		}
		if _, err := g.gh(ctx, rmArgs...); err != nil {
			log.Printf("channel/github: remove old status labels %v on %s failed: %v (continuing to add %s)", remove, ref, err, add)
		}
	}

	// add in its own, separate edit — independent of remove. A failure here
	// (e.g. label still missing because EnsureLabels lacked write scope) is
	// returned, but remove has already committed, so the old label is gone.
	_, err := g.gh(ctx, "issue", "edit", ref, "--repo", g.Repo, "--add-label", add)
	return err
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
	return statusLabelsToRemove(labels, status, g.TaskLabel, g.prefix())
}

func (g *GitHub) CloseIssue(ctx context.Context, ref string) error {
	_, err := g.gh(ctx, "issue", "close", ref, "--repo", g.Repo)
	return err
}

// IsPRMerged reports whether any PR opened from the given head branch has been
// merged into the base. It backs channel.MergeChecker so the daemon's reconcile
// step can auto-close an issue left OPEN pending merge once the branch's PR
// lands. `gh pr list --head <branch> --state merged` returns one row per merged
// PR for that head; len > 0 means the branch's work has been integrated. Errors
// (gh unavailable, no such branch) surface as (false, err) — reconcile logs and
// treats them as "not merged yet" (waits for the next tick rather than closing).
func (g *GitHub) IsPRMerged(ctx context.Context, branch string) (bool, error) {
	out, err := g.gh(ctx, "pr", "list", "--repo", g.Repo,
		"--head", branch, "--state", "merged", "--limit", "1", "--json", "number")
	if err != nil {
		return false, err
	}
	var prs []struct {
		Number int `json:"number"`
	}
	if err := json.Unmarshal(out, &prs); err != nil {
		return false, fmt.Errorf("parse gh pr list merged: %w", err)
	}
	return len(prs) > 0, nil
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

// gh runs a `gh` command and returns stdout (stderr folded into the error).
// If g.ghFunc is set (tests) it replaces the real exec; otherwise ghExec runs
// the real retried `gh`. Every GitHub method goes through here, so a test
// setting ghFunc intercepts all channel traffic without shelling out.
func (g *GitHub) gh(ctx context.Context, args ...string) ([]byte, error) {
	if g.ghFunc != nil {
		return g.ghFunc(ctx, args...)
	}
	return g.ghExec(ctx, args...)
}

// ghExec runs the real `gh` command (retried on transient failure) and returns
// stdout. stderr is folded into the error. The GitHub API intermittently
// TLS-timeouts from some networks; retrying (ghRetry ×, 2s/4s backoff) absorbs
// those blips so a single ListNewTasks/PostComment failure doesn't abort the
// whole run.
func (g *GitHub) ghExec(ctx context.Context, args ...string) ([]byte, error) {
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

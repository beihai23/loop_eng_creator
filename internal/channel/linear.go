package channel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Linear 通道的默认 GraphQL endpoint（单一端点，支持 introspection）。
const DefaultLinearEndpoint = "https://api.linear.app/graphql"

// LinearAPIKeyEnv 是 Linear personal API key 的环境变量名（#24 决定 A：
// key 走环境变量，不进 config.yaml）。与 LOOP_ENG_GITHUB_TOKEN 同族。
const LinearAPIKeyEnv = "LOOP_ENG_LINEAR_API_KEY"

// gqlRetry 是 gql() 对网络错误 / 5xx 的重试次数（与 github.go 的 ghRetry 同理：
// 吸收瞬时抖动，避免单次 ListNewTasks/PostComment 失败拖垮整轮 run）。
const gqlRetry = 3

// Linear 是 channel.Channel 的 Linear 实现：直连 Linear GraphQL API
// （Linear 没有 `gh` 那样的 CLI 对应物，故由 loop-eng 自持 key 发请求）。
// 映射依据 docs/superpowers/specs/linear-channel-mapping.md：
//   - 认证：header `Authorization: <API_KEY>`（原始 key，**无 Bearer 前缀**；
//     Bearer 是 OAuth 的写法，最常见的踩坑点）。
//   - Ref = issue 的 identifier（如 ENG-123）；commentCreate 等需要 UUID 的
//     mutation 由本通道内部 identifier→UUID 解析（带缓存）。
//   - 状态用 WorkflowState（kanban 列），不用 label：UpdateStatus/CloseIssue
//     都走 issueUpdate(input:{ stateId })——Linear 没有独立的 close mutation，
//     推进到 completed-type state 即关闭。
//   - loop status → stateId：先按 statusMap 里的 name 查 workflowStates
//     （运行时 name→id），查不到再按 state.type 兜底（done→completed、
//     running→started 等）。
//
// 字段同时保留导出/未导出两份（构造函数会同步写入）：导出字段供包外
// （如 cli 层）读取配置，未导出字段供包内测试直接替换 endpoint/key 指向
// mock server。运行时每处取值都优先未导出字段、回落导出字段、再回落默认值。
type Linear struct {
	// APIKey 是 personal API key；为空时回落环境变量 LOOP_ENG_LINEAR_API_KEY。
	APIKey string
	// Endpoint 是 GraphQL endpoint；为空时回落 DefaultLinearEndpoint。
	Endpoint string
	// ProjectID 是任务发现过滤用的 project UUID（ListNewTasks 的 filter 维度，
	// 替代 GitHub 的 task label）。
	ProjectID string
	// TeamID 可选；非空时 workflowStates 查询限定到该 team。
	TeamID string
	// StatusMap 是 loop status → Linear WorkflowState name 的可配映射
	// （config channel.linear.status_map）；运行时按 name 解析成 stateId。
	StatusMap map[string]string
	// HTTPClient 可选；为空用 http.DefaultClient（测试可注入）。
	HTTPClient *http.Client

	apiKey     string
	endpoint   string
	projectID  string
	teamID     string
	statusMap  map[string]string
	httpClient *http.Client

	mu         sync.Mutex
	uuidCache  map[string]string // identifier → UUID（commentCreate 等用）
	stateCache []linearState     // workflowStates 缓存（name/type → id 解析用）
	stateReady bool

	// repliesOff/statesOff rotate the per-tick ref cap (capRefs) across ticks so
	// a stable overflow list is covered, not starved (same tail cut every tick,
	// never polled). Mirrors GitHub.repliesOff/statesOff. Single-active engine.
	repliesOff, statesOff int
}

// linearState 是 WorkflowState 的三元组（kanban 列：自定义列名 + 语义类别 +
// 顺序；这里只需要 name/type→id 的解析）。
type linearState struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// NewLinear 构造 Linear 通道，参数按显式位置语义命名（不再按内容猜测）：
//
//		NewLinear(apiKey, endpoint, projectID, teamID, statusMap)
//
//	  - apiKey 为空时，每次请求回落读环境变量 LOOP_ENG_LINEAR_API_KEY。
//	  - endpoint 为空时，由 gqlEndpoint() 回落 DefaultLinearEndpoint。
//	  - statusMap 为 loop status → Linear WorkflowState name 的可配映射（可为 nil）。
//
// 导出与未导出字段同步写入：导出字段供包外（如 cli 层）读取配置，
// 未导出字段供包内测试直接替换 endpoint/key 指向 mock server。
func NewLinear(apiKey, endpoint, projectID, teamID string, statusMap map[string]string) *Linear {
	lc := &Linear{}
	lc.apiKey, lc.APIKey = apiKey, apiKey
	lc.endpoint, lc.Endpoint = endpoint, endpoint
	lc.projectID, lc.ProjectID = projectID, projectID
	lc.teamID, lc.TeamID = teamID, teamID
	lc.statusMap, lc.StatusMap = statusMap, statusMap
	return lc
}

// 编译期接口断言：Linear 必须完整实现冻结的六方法 Channel 接口。
var _ Channel = (*Linear)(nil)

// key 解析 API key：未导出字段 → 导出字段 → 环境变量 LOOP_ENG_LINEAR_API_KEY。
func (lc *Linear) key() string {
	if lc.apiKey != "" {
		return lc.apiKey
	}
	if lc.APIKey != "" {
		return lc.APIKey
	}
	return os.Getenv(LinearAPIKeyEnv)
}

func (lc *Linear) gqlEndpoint() string {
	if lc.endpoint != "" {
		return lc.endpoint
	}
	if lc.Endpoint != "" {
		return lc.Endpoint
	}
	return DefaultLinearEndpoint
}

func (lc *Linear) client() *http.Client {
	if lc.httpClient != nil {
		return lc.httpClient
	}
	if lc.HTTPClient != nil {
		return lc.HTTPClient
	}
	return http.DefaultClient
}

func (lc *Linear) project() string {
	if lc.projectID != "" {
		return lc.projectID
	}
	return lc.ProjectID
}

func (lc *Linear) team() string {
	if lc.teamID != "" {
		return lc.teamID
	}
	return lc.TeamID
}

// statusName 把 loop status 映射到 WorkflowState name（未导出 map 优先）。
func (lc *Linear) statusName(status string) string {
	if lc.statusMap != nil {
		if n, ok := lc.statusMap[status]; ok {
			return n
		}
	}
	if lc.StatusMap != nil {
		if n, ok := lc.StatusMap[status]; ok {
			return n
		}
	}
	return ""
}

// gqlRequest / gqlResponse 是 Linear GraphQL 端点的标准信封：
// 请求 { "query": ..., "variables": {...} }；响应 data + errors——
// HTTP 200 也可能带 errors（部分成功），必须先查 errors 再判成功。
type gqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

type gqlError struct {
	Message string `json:"message"`
}

type gqlResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []gqlError      `json:"errors"`
}

// gql 发一次 GraphQL 请求并把结果 data 解进 out（可为 nil）。
//
// 容忍式参数：query 之后可给 map[string]any（variables）与任意指针 out，
// 顺序不限、均可省略——以下调用等价可用：
//
//	lc.gql(ctx, query)
//	lc.gql(ctx, query, vars)
//	lc.gql(ctx, query, vars, &out)
//
// 错误处理（映射文档 §1）：网络错误/5xx 重试 gqlRetry 次；响应里的 GraphQL
// errors 数组折成 Go error（HTTP 200 也不例外）。
func (lc *Linear) gql(ctx context.Context, query string, rest ...any) error {
	var vars map[string]any
	var out any
	for _, a := range rest {
		switch v := a.(type) {
		case nil:
		case map[string]any:
			if vars == nil {
				vars = v
			}
		default:
			if out == nil {
				out = v
			}
		}
	}
	key := lc.key()
	if key == "" {
		return fmt.Errorf("linear: API key 未设置（%s）", LinearAPIKeyEnv)
	}
	body, err := json.Marshal(gqlRequest{Query: query, Variables: vars})
	if err != nil {
		return fmt.Errorf("linear: marshal request: %w", err)
	}
	var lastErr error
	for attempt := 1; attempt <= gqlRetry; attempt++ {
		var resp gqlResponse
		lastErr = lc.gqlOnce(ctx, body, key, &resp)
		if lastErr == nil {
			if out != nil && len(resp.Data) > 0 {
				if err := json.Unmarshal(resp.Data, out); err != nil {
					return fmt.Errorf("linear: decode data: %w", err)
				}
			}
			return nil
		}
		if !isRetryable(lastErr) {
			return lastErr
		}
		if attempt < gqlRetry {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * 150 * time.Millisecond):
			}
		}
	}
	return lastErr
}

// gqlOnce 发单次请求；GraphQL errors 折成 *gqlErrors（不可重试），
// 网络错误与 5xx 折成 *retryableError（可重试）。
func (lc *Linear) gqlOnce(ctx context.Context, body []byte, key string, resp *gqlResponse) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, lc.gqlEndpoint(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("linear: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// 认证：personal API key 走原始 Authorization header——**不加 Bearer 前缀**
	// （Bearer 是 OAuth access token 的写法；映射文档 §1 标的最常见踩坑点）。
	req.Header.Set("Authorization", key)
	hr, err := lc.client().Do(req)
	if err != nil {
		return &retryableError{fmt.Errorf("linear: POST %s: %w", lc.gqlEndpoint(), err)}
	}
	defer hr.Body.Close()
	var raw bytes.Buffer
	if _, err := raw.ReadFrom(hr.Body); err != nil {
		return &retryableError{fmt.Errorf("linear: read response: %w", err)}
	}
	if hr.StatusCode >= 500 {
		return &retryableError{fmt.Errorf("linear: HTTP %d: %s", hr.StatusCode, truncate(raw.String(), 200))}
	}
	if hr.StatusCode != http.StatusOK {
		return fmt.Errorf("linear: HTTP %d: %s", hr.StatusCode, truncate(raw.String(), 200))
	}
	if err := json.Unmarshal(raw.Bytes(), resp); err != nil {
		return fmt.Errorf("linear: decode response: %w", err)
	}
	if len(resp.Errors) > 0 {
		msgs := make([]string, 0, len(resp.Errors))
		for _, e := range resp.Errors {
			msgs = append(msgs, e.Message)
		}
		return fmt.Errorf("linear: graphql errors: %s", strings.Join(msgs, "; "))
	}
	return nil
}

// retryableError 标记可重试的错误（网络抖动 / 5xx）。
type retryableError struct{ err error }

func (e *retryableError) Error() string { return e.err.Error() }

func isRetryable(err error) bool {
	_, ok := err.(*retryableError)
	return ok
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ---------------------------------------------------------------------------
// 内部查询 helper
// ---------------------------------------------------------------------------

// resolveIssueUUID 把 identifier（ENG-123）解析成内部 UUID，带缓存。
// commentCreate.input.issueId 是否接受 identifier 简写「待人工核实」（映射文档
// §10.1），故统一喂 UUID 规避不确定性；issue(id:) 接受简写是官方明示的。
func (lc *Linear) resolveIssueUUID(ctx context.Context, ref string) (string, error) {
	lc.mu.Lock()
	if lc.uuidCache != nil {
		if id, ok := lc.uuidCache[ref]; ok {
			lc.mu.Unlock()
			return id, nil
		}
	}
	lc.mu.Unlock()
	const q = `query IssueID($ref: String!) { issue(id: $ref) { id } }`
	var out struct {
		Issue struct {
			ID string `json:"id"`
		} `json:"issue"`
	}
	if err := lc.gql(ctx, q, map[string]any{"ref": ref}, &out); err != nil {
		return "", err
	}
	if out.Issue.ID == "" {
		return "", fmt.Errorf("linear: issue %s 不存在", ref)
	}
	lc.mu.Lock()
	if lc.uuidCache == nil {
		lc.uuidCache = map[string]string{}
	}
	lc.uuidCache[ref] = out.Issue.ID
	lc.mu.Unlock()
	return out.Issue.ID, nil
}

// workflowStates 拉取（并缓存）team 的 WorkflowState 列表，供 status name/type
// → stateId 的运行时解析。team 配置非空时限定到该 team。
//
// TODO: introspection 核实 team(id:){ workflowStates{...} } 的嵌套路径与无
// team 时根查询 workflowStates 的作用域（映射文档 §10.4）。
func (lc *Linear) workflowStates(ctx context.Context) ([]linearState, error) {
	lc.mu.Lock()
	if lc.stateReady {
		defer lc.mu.Unlock()
		return lc.stateCache, nil
	}
	lc.mu.Unlock()
	var states []linearState
	if team := lc.team(); team != "" {
		const q = `query TeamStates($team: String!) {
  team(id: $team) { workflowStates { nodes { id name type } } }
}`
		var out struct {
			Team struct {
				WorkflowStates struct {
					Nodes []linearState `json:"nodes"`
				} `json:"workflowStates"`
			} `json:"team"`
		}
		if err := lc.gql(ctx, q, map[string]any{"team": team}, &out); err != nil {
			return nil, err
		}
		states = out.Team.WorkflowStates.Nodes
	} else {
		const q = `query WorkflowStates { workflowStates { nodes { id name type } } }`
		var out struct {
			WorkflowStates struct {
				Nodes []linearState `json:"nodes"`
			} `json:"workflowStates"`
		}
		if err := lc.gql(ctx, q, &out); err != nil {
			return nil, err
		}
		states = out.WorkflowStates.Nodes
	}
	lc.mu.Lock()
	lc.stateCache, lc.stateReady = states, true
	lc.mu.Unlock()
	return states, nil
}

// statusTypeFallback 是 loop status → WorkflowState.type 的兜底映射
// （statusMap 无条目或 name 未命中时用）。type 枚举：backlog | unstarted |
// started | completed | canceled（开 Triage 时多 triage）。
var statusTypeFallback = map[string]string{
	"done":         "completed",
	"cancelled":    "canceled",
	"canceled":     "canceled",
	"running":      "started",
	"needs-review": "started",
	"blocked":      "unstarted",
	"backlog":      "backlog",
	"todo":         "unstarted",
}

// resolveStateID 把 loop status 解析成 Linear stateId：先按 statusMap 给的
// name 在 workflowStates 里查（运行时 name→id，大小写不敏感），查不到再按
// state.type 兜底（映射文档 §5.2 的隐式映射）。
func (lc *Linear) resolveStateID(ctx context.Context, status string) (string, error) {
	states, err := lc.workflowStates(ctx)
	if err != nil {
		return "", err
	}
	target := lc.statusName(status)
	name := target
	if name == "" {
		name = status // 无映射条目时也先按 status 本名试一次 name 命中
	}
	for _, s := range states {
		if strings.EqualFold(s.Name, name) {
			return s.ID, nil
		}
	}
	typ := statusTypeFallback[status]
	if typ == "" {
		typ = status // status 本身可能就是一个 type 名
	}
	for _, s := range states {
		if s.Type == typ {
			return s.ID, nil
		}
	}
	if target != "" {
		return "", fmt.Errorf("linear: 找不到 status %q（映射 name %q / type %q）对应的 WorkflowState", status, target, typ)
	}
	return "", fmt.Errorf("linear: 找不到 status %q（name/type %q）对应的 WorkflowState", status, typ)
}

// issueUpdateState 是 UpdateStatus/CloseIssue 的共用骨架：
// issueUpdate(id: ref, input: { stateId }) —— issueUpdate 的 id 官方明示
// 接受 identifier 简写，无需 UUID 解析。Linear 没有独立的 close mutation，
// 关闭 = 推进到 completed-type state（completedAt 由服务端写）。
func (lc *Linear) issueUpdateState(ctx context.Context, ref, stateID string) error {
	const m = `mutation UpdateState($ref: String!, $state: String!) {
  issueUpdate(id: $ref, input: { stateId: $state }) { success }
}`
	var out struct {
		IssueUpdate struct {
			Success bool `json:"success"`
		} `json:"issueUpdate"`
	}
	if err := lc.gql(ctx, m, map[string]any{"ref": ref, "state": stateID}, &out); err != nil {
		return err
	}
	if !out.IssueUpdate.Success {
		return fmt.Errorf("linear: issueUpdate %s → state %s 未成功", ref, stateID)
	}
	return nil
}

// ---------------------------------------------------------------------------
// channel.Channel 六方法
// ---------------------------------------------------------------------------

// ListNewTasks 按 project 过滤拉「未终态」issue（形态 A，映射文档 §3）：
// state.type ∉ {completed, canceled} 等价 GitHub 的 --state open。
// description 复用 parseLocalTask 解析（## 任务 / type: / - [ ]），解析不到
// desc 回落 title；Ref = identifier。
//
// TODO: introspection 核实 project filter 是否也接受 name（映射文档 §3 提到
// project:{ name:{ eq } } 更稳但非官方明示形态）；当前按 id 过滤。
func (lc *Linear) ListNewTasks(ctx context.Context) ([]Task, error) {
	const q = `query ListNewTasks($project: String!) {
  issues(filter: {
    project: { id: { eq: $project } }
    state: { type: { nin: ["completed", "canceled"] } }
  }) {
    nodes { identifier title description createdAt state { id name type } }
  }
}`
	var out struct {
		Issues struct {
			Nodes []struct {
				Identifier  string `json:"identifier"`
				Title       string `json:"title"`
				Description string `json:"description"`
				CreatedAt   string `json:"createdAt"`
			} `json:"nodes"`
		} `json:"issues"`
	}
	if err := lc.gql(ctx, q, map[string]any{"project": lc.project()}, &out); err != nil {
		return nil, err
	}
	var tasks []Task
	for _, n := range out.Issues.Nodes {
		t := parseLocalTask(n.Description) // 复用 M1 的 body 解析（同 github 通道）
		t.Ref = n.Identifier
		t.CreatedAt = n.CreatedAt
		t.Title = n.Title // Linear issue 标题：PR 标题的源头（区别于 Description=正文首行）
		if t.Description == "" {
			t.Description = n.Title
		}
		tasks = append(tasks, t)
	}
	return tasks, nil
}

// ListReplies fetches comments for ALL refs in a single GraphQL request using
// aliased issue() fields (r0..rN-1) instead of one request per ref — N refs now
// cost one round-trip (plus gql's existing retry/ctx passthrough covers the
// whole batch), so a busy daemon tick no longer makes N serial calls that each
// block dispatch. refs are capped to maxRefsPerTick first (overflow logged, not
// silently dropped). The daemon calls this with since=zero and re-filters per
// task; when since is non-zero this channel still filters client-side (mapping
// doc §4.1, isomorphic with parseIssueCommentsJSON) so existing subloop callers
// are unaffected. Each Reply carries CreatedAt so callers can re-filter.
func (lc *Linear) ListReplies(ctx context.Context, refs []string, since time.Time) (map[string][]Reply, error) {
	refs, lc.repliesOff = capRefs("linear.ListReplies", refs, lc.repliesOff)
	var b strings.Builder
	b.WriteString("query { ")
	for i, ref := range refs {
		fmt.Fprintf(&b, "r%d: issue(id: %q) { comments { nodes { body createdAt } } } ", i, ref)
	}
	b.WriteString("}")
	var data map[string]json.RawMessage
	if err := lc.gql(ctx, b.String(), &data); err != nil {
		return nil, err
	}
	result := make(map[string][]Reply, len(refs))
	for i, ref := range refs {
		raw, ok := data["r"+strconv.Itoa(i)]
		if !ok {
			continue // alias absent (ref errored) — leave this ref unpopulated
		}
		var issue struct {
			Comments struct {
				Nodes []struct {
					Body      string `json:"body"`
					CreatedAt string `json:"createdAt"`
				} `json:"nodes"`
			} `json:"comments"`
		}
		if err := json.Unmarshal(raw, &issue); err != nil {
			return nil, fmt.Errorf("linear: decode comments for %s: %w", ref, err)
		}
		replies := make([]Reply, 0, len(issue.Comments.Nodes))
		for _, c := range issue.Comments.Nodes {
			if !since.IsZero() {
				t, err := time.Parse(time.RFC3339, c.CreatedAt)
				if err != nil || !t.After(since) {
					continue
				}
			}
			replies = append(replies, Reply{Body: c.Body, CreatedAt: c.CreatedAt})
		}
		result[ref] = replies
	}
	return result, nil
}

// PostComment 发评论：先把 identifier 解析成 UUID（commentCreate.issueId 喂
// UUID，规避「是否接受简写」的不确定性，映射文档 §4.2/§10.1），再 commentCreate。
func (lc *Linear) PostComment(ctx context.Context, ref, body string) error {
	uuid, err := lc.resolveIssueUUID(ctx, ref)
	if err != nil {
		return err
	}
	const m = `mutation Comment($issue: String!, $body: String!) {
  commentCreate(input: { issueId: $issue, body: $body }) {
    success
    comment { id url }
  }
}`
	var out struct {
		CommentCreate struct {
			Success bool `json:"success"`
		} `json:"commentCreate"`
	}
	if err := lc.gql(ctx, m, map[string]any{"issue": uuid, "body": body}, &out); err != nil {
		return err
	}
	if !out.CommentCreate.Success {
		return fmt.Errorf("linear: commentCreate %s 未成功", ref)
	}
	return nil
}

// UpdateStatus 把 loop status 映射到 WorkflowState 并 issueUpdate(stateId)。
// 这是与 GitHub 通道的最大差异：GitHub 贴 loop:<status> label，Linear 改
// state（kanban 列），不污染团队的 label 体系（映射文档 §5.2）。
func (lc *Linear) UpdateStatus(ctx context.Context, ref, status string) error {
	stateID, err := lc.resolveStateID(ctx, status)
	if err != nil {
		return err
	}
	return lc.issueUpdateState(ctx, ref, stateID)
}

// CloseIssue 把 issue 推进到 completed-type state——Linear 无独立 close
// mutation（映射文档 §5.3），等价于 UpdateStatus(ref, "done")。
func (lc *Linear) CloseIssue(ctx context.Context, ref string) error {
	stateID, err := lc.resolveStateID(ctx, "done")
	if err != nil {
		return err
	}
	return lc.issueUpdateState(ctx, ref, stateID)
}

// GetTaskStates fetches state + archivedAt for ALL refs in a single GraphQL
// request using aliased issue() fields (r0..rN-1) instead of one request per
// ref — N terminal refs (a reconcile batch) now cost one round-trip, so a
// network blip no longer makes N serial calls that block dispatch. refs are
// capped to maxRefsPerTick first (overflow logged, not silently dropped). The
// alias key r{i} zips back to refs[i]; per-ref IsOpen/Labels follow the same
// rules as before: IsOpen = archivedAt==nil && state.type ∉ {completed, canceled};
// Labels = [state.name] (Linear's "status marker" is the kanban column, §7).
func (lc *Linear) GetTaskStates(ctx context.Context, refs []string) (map[string]TaskState, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	refs, lc.statesOff = capRefs("linear.GetTaskStates", refs, lc.statesOff)
	var b strings.Builder
	b.WriteString("query { ")
	for i, ref := range refs {
		fmt.Fprintf(&b, "r%d: issue(id: %q) { state { id name type } archivedAt } ", i, ref)
	}
	b.WriteString("}")
	var data map[string]json.RawMessage
	if err := lc.gql(ctx, b.String(), &data); err != nil {
		return nil, err
	}
	out := make(map[string]TaskState, len(refs))
	for i, ref := range refs {
		raw, ok := data["r"+strconv.Itoa(i)]
		if !ok {
			continue // alias absent (ref errored) — leave this ref unpopulated
		}
		var issue struct {
			State      linearState `json:"state"`
			ArchivedAt *string     `json:"archivedAt"`
		}
		if err := json.Unmarshal(raw, &issue); err != nil {
			return nil, fmt.Errorf("linear: decode state for %s: %w", ref, err)
		}
		open := issue.ArchivedAt == nil && issue.State.Type != "completed" && issue.State.Type != "canceled"
		var labels []string
		if issue.State.Name != "" {
			labels = []string{issue.State.Name}
		}
		out[ref] = TaskState{Ref: ref, IsOpen: open, Labels: labels}
	}
	return out, nil
}

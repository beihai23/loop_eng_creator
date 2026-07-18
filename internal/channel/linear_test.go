package channel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// linearStubReq 记录一次打到 mock GraphQL server 的请求（信封 + 认证头）。
type linearStubReq struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
	Auth      string
}

// newLinearStub 起一个 httptest GraphQL mock：respond 按请求内容给 data
// 载荷（或带 errors 的完整信封）。返回的 Linear 已指向该 server。
// helper 名特意取得偏门——verify 的 tier-1 会往本目录落一个自动生成的
// linear_tier1_test.go，避免与它撞符号。
func newLinearStub(t *testing.T, respond func(q linearStubReq) any) (*Linear, *[]linearStubReq) {
	t.Helper()
	reqs := &[]linearStubReq{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q linearStubReq
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			t.Errorf("stub: decode request: %v", err)
		}
		q.Auth = r.Header.Get("Authorization")
		*reqs = append(*reqs, q)
		w.Header().Set("Content-Type", "application/json")
		out := respond(q)
		raw, err := json.Marshal(out)
		if err != nil {
			t.Errorf("stub: marshal response: %v", err)
		}
		_, _ = w.Write(raw)
	}))
	t.Cleanup(srv.Close)
	return NewLinear("lin_api_stubkey", srv.URL, "proj-uuid-1", "team-uuid-1", nil), reqs
}

// stubData 包一层 {"data": ...} 信封。
func stubData(data any) map[string]any { return map[string]any{"data": data} }

func TestLinearRawAuthNoBearer(t *testing.T) {
	lc, reqs := newLinearStub(t, func(q linearStubReq) any {
		return stubData(map[string]any{"issues": map[string]any{"nodes": []any{}}})
	})
	if _, err := lc.ListNewTasks(context.Background()); err != nil {
		t.Fatalf("ListNewTasks: %v", err)
	}
	if len(*reqs) == 0 {
		t.Fatal("no request recorded")
	}
	got := (*reqs)[0].Auth
	if got != "lin_api_stubkey" {
		t.Fatalf("Authorization = %q, want raw key %q（无 Bearer 前缀）", got, "lin_api_stubkey")
	}
	if strings.HasPrefix(got, "Bearer ") {
		t.Fatalf("Authorization 带了 Bearer 前缀（personal API key 应用原始 header）: %q", got)
	}
}

func TestLinearEnvKeyAuth(t *testing.T) {
	t.Setenv(LinearAPIKeyEnv, "lin_api_envkey")
	lc, reqs := newLinearStub(t, func(q linearStubReq) any {
		return stubData(map[string]any{"issues": map[string]any{"nodes": []any{}}})
	})
	// 构造时不给 key（NewLinear 第一个参数为空），应回落环境变量。
	lc.apiKey, lc.APIKey = "", ""
	if _, err := lc.ListNewTasks(context.Background()); err != nil {
		t.Fatalf("ListNewTasks: %v", err)
	}
	if got := (*reqs)[len(*reqs)-1].Auth; got != "lin_api_envkey" {
		t.Fatalf("Authorization = %q, want env key", got)
	}
}

func TestLinearGQLErrorsFoldedToError(t *testing.T) {
	lc, _ := newLinearStub(t, func(q linearStubReq) any {
		return map[string]any{
			"errors": []map[string]any{{"message": "Argument Validation Error"}, {"message": "field X unknown"}},
		}
	})
	err := lc.gql(context.Background(), "query { viewer { id } }")
	if err == nil {
		t.Fatal("gql: GraphQL errors 应折成 Go error，得到 nil")
	}
	if !strings.Contains(err.Error(), "Argument Validation Error") {
		t.Fatalf("error 应含 errors[].message: %v", err)
	}
	// 方法层同样要透出（HTTP 200 带 errors 也算失败）。
	if _, err := lc.ListNewTasks(context.Background()); err == nil {
		t.Fatal("ListNewTasks: GraphQL errors 应折成 Go error，得到 nil")
	}
}

func TestLinearListNewTasks(t *testing.T) {
	lc, reqs := newLinearStub(t, func(q linearStubReq) any {
		if !strings.Contains(q.Query, "issues(") {
			t.Errorf("ListNewTasks 应走 issues(filter:) 根查询, got: %s", q.Query)
		}
		return stubData(map[string]any{"issues": map[string]any{"nodes": []map[string]any{
			{
				"identifier":  "ENG-1",
				"title":       "fallback title",
				"description": "实现 Linear 通道\ntype: feature\n- [ ] ac one\n- [ ] ac two",
				"createdAt":   "2026-07-17T10:00:00Z",
			},
			{"identifier": "ENG-2", "title": "only title", "description": "", "createdAt": ""},
		}}})
	})
	tasks, err := lc.ListNewTasks(context.Background())
	if err != nil {
		t.Fatalf("ListNewTasks: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("got %d tasks, want 2", len(tasks))
	}
	t0 := tasks[0]
	if t0.Ref != "ENG-1" {
		t.Fatalf("Ref = %q, want identifier ENG-1", t0.Ref)
	}
	if t0.Description != "实现 Linear 通道" || t0.TaskType != "feature" {
		t.Fatalf("parsed task = %+v", t0)
	}
	if len(t0.AcceptanceCriteria) != 2 || t0.AcceptanceCriteria[0] != "ac one" {
		t.Fatalf("AC = %v", t0.AcceptanceCriteria)
	}
	if t0.CreatedAt != "2026-07-17T10:00:00Z" {
		t.Fatalf("CreatedAt = %q", t0.CreatedAt)
	}
	if tasks[1].Description != "only title" {
		t.Fatalf("description 为空应回落 title, got %q", tasks[1].Description)
	}
	// project 过滤：variables 里应带 project。
	if (*reqs)[0].Variables["project"] != "proj-uuid-1" {
		t.Fatalf("variables.project = %v", (*reqs)[0].Variables["project"])
	}
}

func TestLinearListReplies(t *testing.T) {
	lc, _ := newLinearStub(t, func(q linearStubReq) any {
		return stubData(map[string]any{"issue": map[string]any{"comments": map[string]any{"nodes": []map[string]any{
			{"body": "old", "createdAt": "2026-07-01T00:00:00Z"},
			{"body": "new reply", "createdAt": "2026-07-17T12:00:00Z"},
		}}}})
	})
	since := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	got, err := lc.ListReplies(context.Background(), []string{"ENG-1"}, since)
	if err != nil {
		t.Fatalf("ListReplies: %v", err)
	}
	reps := got["ENG-1"]
	if len(reps) != 1 || reps[0].Body != "new reply" {
		t.Fatalf("since 过滤后 replies = %+v, want 仅 new reply", reps)
	}
}

func TestLinearPostCommentResolvesUUID(t *testing.T) {
	lc, reqs := newLinearStub(t, func(q linearStubReq) any {
		switch {
		case strings.Contains(q.Query, "commentCreate"):
			return stubData(map[string]any{"commentCreate": map[string]any{
				"success": true, "comment": map[string]any{"id": "c1", "url": "https://linear.app/c1"}}})
		case strings.Contains(q.Query, "issue("):
			return stubData(map[string]any{"issue": map[string]any{"id": "uuid-eng-1"}})
		}
		t.Errorf("unexpected query: %s", q.Query)
		return stubData(map[string]any{})
	})
	if err := lc.PostComment(context.Background(), "ENG-1", "战报 body"); err != nil {
		t.Fatalf("PostComment: %v", err)
	}
	if len(*reqs) != 2 {
		t.Fatalf("应先发 identifier→UUID 解析再 commentCreate, got %d requests", len(*reqs))
	}
	vars := (*reqs)[1].Variables
	if vars["issue"] != "uuid-eng-1" {
		t.Fatalf("commentCreate.issueId 应喂 UUID, got %v", vars["issue"])
	}
	if vars["body"] != "战报 body" {
		t.Fatalf("body = %v", vars["body"])
	}
}

func TestLinearUpdateStatusByNameAndTypeFallback(t *testing.T) {
	lc, reqs := newLinearStub(t, func(q linearStubReq) any {
		switch {
		case strings.Contains(q.Query, "issueUpdate"):
			return stubData(map[string]any{"issueUpdate": map[string]any{"success": true}})
		case strings.Contains(q.Query, "workflowStates"):
			return stubData(map[string]any{"team": map[string]any{"workflowStates": map[string]any{"nodes": []map[string]any{
				{"id": "st-todo", "name": "Todo", "type": "unstarted"},
				{"id": "st-doing", "name": "In Progress", "type": "started"},
				{"id": "st-done", "name": "Done", "type": "completed"},
			}}}})
		}
		t.Errorf("unexpected query: %s", q.Query)
		return stubData(map[string]any{})
	})
	lc.statusMap = map[string]string{"running": "In Progress"}
	lc.StatusMap = lc.statusMap

	// 按 name（status_map → 运行时 name→id）。
	if err := lc.UpdateStatus(context.Background(), "ENG-1", "running"); err != nil {
		t.Fatalf("UpdateStatus running: %v", err)
	}
	last := (*reqs)[len(*reqs)-1]
	if last.Variables["state"] != "st-doing" {
		t.Fatalf("name 映射 stateId = %v, want st-doing", last.Variables["state"])
	}
	if last.Variables["ref"] != "ENG-1" {
		t.Fatalf("issueUpdate id 应用 identifier, got %v", last.Variables["ref"])
	}

	// 按 type 兜底（status_map 无条目：blocked → unstarted）。
	if err := lc.UpdateStatus(context.Background(), "ENG-1", "blocked"); err != nil {
		t.Fatalf("UpdateStatus blocked: %v", err)
	}
	if got := (*reqs)[len(*reqs)-1].Variables["state"]; got != "st-todo" {
		t.Fatalf("type 兜底 stateId = %v, want st-todo", got)
	}
}

func TestLinearCloseIssue(t *testing.T) {
	lc, reqs := newLinearStub(t, func(q linearStubReq) any {
		switch {
		case strings.Contains(q.Query, "issueUpdate"):
			return stubData(map[string]any{"issueUpdate": map[string]any{"success": true}})
		case strings.Contains(q.Query, "workflowStates"):
			return stubData(map[string]any{"team": map[string]any{"workflowStates": map[string]any{"nodes": []map[string]any{
				{"id": "st-doing", "name": "In Progress", "type": "started"},
				{"id": "st-done", "name": "Done", "type": "completed"},
			}}}})
		}
		t.Errorf("unexpected query: %s", q.Query)
		return stubData(map[string]any{})
	})
	if err := lc.CloseIssue(context.Background(), "ENG-9"); err != nil {
		t.Fatalf("CloseIssue: %v", err)
	}
	last := (*reqs)[len(*reqs)-1]
	if !strings.Contains(last.Query, "issueUpdate") {
		t.Fatalf("Linear 无独立 close mutation，应走 issueUpdate: %s", last.Query)
	}
	if last.Variables["state"] != "st-done" {
		t.Fatalf("CloseIssue 应推进到 completed-type state, got %v", last.Variables["state"])
	}
}

func TestLinearGetTaskStates(t *testing.T) {
	lc, _ := newLinearStub(t, func(q linearStubReq) any {
		if q.Variables["ref"] == "ENG-1" {
			return stubData(map[string]any{"issue": map[string]any{
				"state":      map[string]any{"id": "st-doing", "name": "In Progress", "type": "started"},
				"archivedAt": nil,
			}})
		}
		return stubData(map[string]any{"issue": map[string]any{
			"state":      map[string]any{"id": "st-done", "name": "Done", "type": "completed"},
			"archivedAt": nil,
		}})
	})
	got, err := lc.GetTaskStates(context.Background(), []string{"ENG-1", "ENG-2"})
	if err != nil {
		t.Fatalf("GetTaskStates: %v", err)
	}
	if !got["ENG-1"].IsOpen {
		t.Fatal("started-type 应 IsOpen=true")
	}
	if len(got["ENG-1"].Labels) != 1 || got["ENG-1"].Labels[0] != "In Progress" {
		t.Fatalf("Labels 应为 [state.name], got %v", got["ENG-1"].Labels)
	}
	if got["ENG-2"].IsOpen {
		t.Fatal("completed-type 应 IsOpen=false")
	}
}

func TestLinearHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)
	lc := NewLinear("k", srv.URL, "p", "", nil)
	if _, err := lc.ListNewTasks(context.Background()); err == nil {
		t.Fatal("5xx 应报错")
	}
}

// 编译期接口断言（运行期再确认一次，防止断言被误删）。
func TestLinearChannelConformance(t *testing.T) {
	var _ Channel = NewLinear("k", "", "p", "", nil)
}

package channel

import (
	"context"
	"strings"
	"testing"
)

// tier1Preflighter 钉住 Preflight 的签名：必须是 (ctx context.Context)([]PreflightIssue, error)。
// 上一轮 executor 把它做成单返回值 []PreflightIssue，导致 tier-1 编译失败。
// 这两个编译期断言与 executor 是否另定义 Preflighter 接口无关——只要
// *GitHub / *Linear 的 Preflight 方法签名漂移（返回值数量/类型不对），这里直接编译失败。
type tier1Preflighter interface {
	Preflight(ctx context.Context) ([]PreflightIssue, error)
}

var _ tier1Preflighter = (*GitHub)(nil)
var _ tier1Preflighter = (*Linear)(nil)

func issueMentionsTier1(issues []PreflightIssue, substr string) bool {
	for _, is := range issues {
		if strings.Contains(is.Message, substr) {
			return true
		}
	}
	return false
}

// GitHub：缺 loop:running → 缺失清单含 "loop:running"。
func TestTier1PreflightGitHubMissingLabel(t *testing.T) {
	g := &GitHub{
		Repo: "owner/repo", TaskLabel: "loop:task",
		ghFunc: func(ctx context.Context, args ...string) ([]byte, error) {
			// 故意只回 3 个标签，缺 loop:running 等其余 status 标签。
			return []byte(`[{"name":"loop:task"},{"name":"loop:done"},{"name":"loop:cancelled"}]`), nil
		},
	}
	issues, err := g.Preflight(context.Background())
	if err != nil {
		t.Fatalf("Preflight 不应在检查本身成功时报错: %v", err)
	}
	if !issueMentionsTier1(issues, "loop:running") {
		t.Fatalf("缺 loop:running 应进缺失清单, got %+v", issues)
	}
}

// GitHub：所需标签齐全 → nil issues + nil err。
func TestTier1PreflightGitHubComplete(t *testing.T) {
	names := []string{"loop:task"}
	for _, s := range loopStatusNames {
		names = append(names, "loop:"+s)
	}
	var b strings.Builder
	b.WriteByte('[')
	for i, n := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"name":"`)
		b.WriteString(n)
		b.WriteString(`"}`)
	}
	b.WriteByte(']')
	g := &GitHub{
		Repo: "owner/repo", TaskLabel: "loop:task",
		ghFunc: func(ctx context.Context, args ...string) ([]byte, error) {
			return []byte(b.String()), nil
		},
	}
	issues, err := g.Preflight(context.Background())
	if err != nil || len(issues) != 0 {
		t.Fatalf("齐全应 nil issues + nil err, got issues=%+v err=%v", issues, err)
	}
}

// Linear：project 不存在 → 缺失清单含 project 标识（proj-uuid-1）。
func TestTier1PreflightLinearMissingProject(t *testing.T) {
	lc, _ := newLinearStub(t, func(q linearStubReq) any {
		switch {
		case strings.Contains(q.Query, "viewer"):
			return stubData(map[string]any{"viewer": map[string]any{"id": "u1", "name": "op"}})
		case strings.Contains(q.Query, "project"):
			return stubData(map[string]any{"project": nil}) // project 不存在
		case strings.Contains(q.Query, "workflowStates"):
			return stubData(map[string]any{"team": map[string]any{"workflowStates": map[string]any{"nodes": []any{}}}})
		}
		t.Errorf("unexpected query: %s", q.Query)
		return stubData(map[string]any{})
	})
	issues, err := lc.Preflight(context.Background())
	if err != nil {
		t.Fatalf("Preflight 不应在检查本身成功时报错: %v", err)
	}
	if !issueMentionsTier1(issues, "proj-uuid-1") {
		t.Fatalf("缺 project 应进缺失清单（消息含 project 标识 proj-uuid-1）, got %+v", issues)
	}
}

// Linear：status_map 目标 name 不可解析 → 缺失清单含该 name。
func TestTier1PreflightLinearUnresolvableStatusMap(t *testing.T) {
	lc, _ := newLinearStub(t, func(q linearStubReq) any {
		switch {
		case strings.Contains(q.Query, "viewer"):
			return stubData(map[string]any{"viewer": map[string]any{"id": "u1", "name": "op"}})
		case strings.Contains(q.Query, "project"):
			return stubData(map[string]any{"project": map[string]any{"id": "proj-uuid-1", "name": "Eng"}})
		case strings.Contains(q.Query, "workflowStates"):
			return stubData(map[string]any{"team": map[string]any{"workflowStates": map[string]any{"nodes": []map[string]any{
				{"id": "st-doing", "name": "In Progress", "type": "started"},
			}}}})
		}
		t.Errorf("unexpected query: %s", q.Query)
		return stubData(map[string]any{})
	})
	lc.statusMap = map[string]string{"running": "Nonexistent Column"}
	lc.StatusMap = lc.statusMap
	issues, err := lc.Preflight(context.Background())
	if err != nil {
		t.Fatalf("Preflight 不应在检查本身成功时报错: %v", err)
	}
	if !issueMentionsTier1(issues, "Nonexistent Column") {
		t.Fatalf("不可解析 status_map 目标应进缺失清单, got %+v", issues)
	}
}

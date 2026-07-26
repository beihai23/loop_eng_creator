package channel

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// TestTier1RefPollLinearGetTaskStatesBatches 钉死 Linear 批量契约：
// GetTaskStates 对 N refs 只发 1 次 GraphQL（别名 r0..rN-1 issue() 字段），而非 N 次。
func TestTier1RefPollLinearGetTaskStatesBatches(t *testing.T) {
	lc, reqs := newLinearStub(t, func(q linearStubReq) any {
		return stubData(map[string]any{
			"r0": map[string]any{"state": map[string]any{"id": "s1", "name": "In Progress", "type": "started"}, "archivedAt": nil},
			"r1": map[string]any{"state": map[string]any{"id": "s2", "name": "Done", "type": "completed"}, "archivedAt": nil},
			"r2": map[string]any{"state": map[string]any{"id": "s3", "name": "Todo", "type": "unstarted"}, "archivedAt": nil},
		})
	})
	got, err := lc.GetTaskStates(context.Background(), []string{"ENG-1", "ENG-2", "ENG-3"})
	if err != nil {
		t.Fatalf("GetTaskStates: %v", err)
	}
	if len(*reqs) != 1 {
		t.Fatalf("批量 GetTaskStates 对 3 refs 应只发 1 次请求, got %d", len(*reqs))
	}
	if len(got) != 3 {
		t.Fatalf("want 3 states, got %d", len(got))
	}
	if !got["ENG-1"].IsOpen {
		t.Errorf("ENG-1(started) 应 IsOpen=true")
	}
	if got["ENG-2"].IsOpen {
		t.Errorf("ENG-2(completed) 应 IsOpen=false")
	}
}

// TestTier1RefPollLinearListRepliesBatches 钉死 ListReplies 同款批量契约 + r{i}↔refs[i] zip。
func TestTier1RefPollLinearListRepliesBatches(t *testing.T) {
	lc, reqs := newLinearStub(t, func(q linearStubReq) any {
		return stubData(map[string]any{
			"r0": map[string]any{"comments": map[string]any{"nodes": []map[string]any{{"body": "reply-a", "createdAt": "2026-07-20T00:00:00Z"}}}},
			"r1": map[string]any{"comments": map[string]any{"nodes": []map[string]any{{"body": "reply-b", "createdAt": "2026-07-20T00:00:00Z"}}}},
		})
	})
	got, err := lc.ListReplies(context.Background(), []string{"ENG-1", "ENG-2"}, time.Time{})
	if err != nil {
		t.Fatalf("ListReplies: %v", err)
	}
	if len(*reqs) != 1 {
		t.Fatalf("批量 ListReplies 对 2 refs 应只发 1 次请求, got %d", len(*reqs))
	}
	if len(got["ENG-1"]) != 1 || got["ENG-1"][0].Body != "reply-a" {
		t.Fatalf("ENG-1 replies = %+v", got["ENG-1"])
	}
	if len(got["ENG-2"]) != 1 || got["ENG-2"][0].Body != "reply-b" {
		t.Fatalf("ENG-2 replies = %+v", got["ENG-2"])
	}
}

// TestTier1RefPollGitHubCapsRefs 钉死每 tick refs 上限：超过 maxRefsPerTick 即截断（记日志），不全部拉取。
func TestTier1RefPollGitHubCapsRefs(t *testing.T) {
	var calls int32
	g := &GitHub{Repo: "owner/repo", TaskLabel: "loop:task", ghFunc: func(ctx context.Context, args ...string) ([]byte, error) {
		atomic.AddInt32(&calls, 1)
		return []byte(`{"state":"OPEN","labels":[]}`), nil
	}}
	n := maxRefsPerTick + 5
	refs := make([]string, n)
	for i := range refs {
		refs[i] = strconv.Itoa(i + 1)
	}
	got, err := g.GetTaskStates(context.Background(), refs)
	if err != nil {
		t.Fatalf("GetTaskStates: %v", err)
	}
	if int(calls) != maxRefsPerTick {
		t.Fatalf("gh 调用数 = %d, want 被封顶到 maxRefsPerTick=%d", calls, maxRefsPerTick)
	}
	if len(got) != maxRefsPerTick {
		t.Fatalf("返回 states = %d, want 封顶到 maxRefsPerTick=%d", len(got), maxRefsPerTick)
	}
}

// TestTier1RefPollGitHubConcurrent 钉死有界并发池契约：N refs 被并发拉取（≥2 同时在飞），不再串行。
func TestTier1RefPollGitHubConcurrent(t *testing.T) {
	var inFlight, maxInFlight int32
	g := &GitHub{Repo: "owner/repo", TaskLabel: "loop:task", ghFunc: func(ctx context.Context, args ...string) ([]byte, error) {
		n := atomic.AddInt32(&inFlight, 1)
		if n > atomic.LoadInt32(&maxInFlight) {
			atomic.StoreInt32(&maxInFlight, n)
		}
		time.Sleep(15 * time.Millisecond) // 强制重叠窗口
		atomic.AddInt32(&inFlight, -1)
		return []byte(`{"comments":[]}`), nil
	}}
	refs := make([]string, 8)
	for i := range refs {
		refs[i] = strconv.Itoa(i + 1)
	}
	if _, err := g.ListReplies(context.Background(), refs, time.Time{}); err != nil {
		t.Fatalf("ListReplies: %v", err)
	}
	if got := atomic.LoadInt32(&maxInFlight); got < 2 {
		t.Fatalf("ListReplies 串行了: 最大并发 gh 调用 = %d, want ≥2（有界池）", got)
	}
}
#!/usr/bin/env bash
set -euo pipefail
cat > internal/channel/status_label_mutex_tier1_test.go <<'GOEOF'
package channel

import (
	"reflect"
	"testing"
)

// 合同：statusLabelsToRemove(labels []string, newStatus, taskLabel string) []string
// 返回 labels 中带 loop: 前缀、!= taskLabel、!= "loop:"+newStatus 的标签，保持输入顺序。
func TestTier1StatusLabelMutex(t *testing.T) {
	// 连续打两个状态后只剩最新的：移除集含所有旧 loop: 状态标签
	got := statusLabelsToRemove([]string{"loop:task", "loop:done", "loop:blocked", "bug"}, "running", "loop:task")
	want := []string{"loop:done", "loop:blocked"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("remove set = %v, want %v", got, want)
	}
	// loop:task 永不动；新状态标签不移除（幂等重打）
	if got := statusLabelsToRemove([]string{"loop:task", "loop:running"}, "running", "loop:task"); len(got) != 0 {
		t.Fatalf("idempotent re-mark must remove nothing, got %v", got)
	}
	// 非 loop: 前缀标签不受互斥影响
	if got := statusLabelsToRemove([]string{"bug", "loop:task"}, "done", "loop:task"); len(got) != 0 {
		t.Fatalf("non-status labels must be kept, got %v", got)
	}
	// 空输入 / 无 loop: 标签 → 空
	if got := statusLabelsToRemove(nil, "done", "loop:task"); len(got) != 0 {
		t.Fatalf("nil labels must yield empty, got %v", got)
	}
}
GOEOF
cat > internal/daemon/dispatch_running_tier1_test.go <<'GOEOF'
package daemon

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"loop-eng/internal/channel"
	"loop-eng/internal/state"
)

// recordingChannel 复用 scriptedChannel 的 ingest 脚本，记录 UpdateStatus 调用序列。
type recordingChannel struct {
	scriptedChannel
	statusCalls []string // "ref=status" 按序
}

func (c *recordingChannel) UpdateStatus(ctx context.Context, ref, status string) error {
	c.statusCalls = append(c.statusCalls, fmt.Sprintf("%s=%s", ref, status))
	return nil
}

// 合同：tick 派发（new→running）时先打 loop:running 再调 RunTask。
func TestTier1DispatchMarksRunning(t *testing.T) {
	st := newTestStore(t)
	ch := &recordingChannel{scriptedChannel: scriptedChannel{batches: [][]channel.Task{
		{{Ref: "42", Description: "t", TaskType: "feat"}},
	}}}
	eng := &Engine{
		Channel: ch, Store: st, Interval: time.Second,
		RunTask: func(ctx context.Context, task state.TaskRow) (string, string, error) {
			for _, c := range ch.statusCalls {
				if c == "42=running" {
					return "done", "", nil
				}
			}
			t.Fatalf("running mark must precede RunTask, UpdateStatus calls so far: %v", ch.statusCalls)
			return "", "", nil
		},
	}
	if err := eng.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(ch.statusCalls) == 0 || ch.statusCalls[0] != "42=running" {
		t.Fatalf("first UpdateStatus must be 42=running (dispatch mark), got %v", ch.statusCalls)
	}
}

// failingStatusChannel 的 UpdateStatus 永远失败，验证打标失败不翻转任务结局。
type failingStatusChannel struct {
	scriptedChannel
}

func (c *failingStatusChannel) UpdateStatus(ctx context.Context, ref, status string) error {
	return errors.New("gh down")
}

func TestTier1RunningMarkFailureTolerated(t *testing.T) {
	st := newTestStore(t)
	ch := &failingStatusChannel{scriptedChannel{batches: [][]channel.Task{
		{{Ref: "42", Description: "t", TaskType: "feat"}},
	}}}
	eng := &Engine{
		Channel: ch, Store: st, Interval: time.Second,
		RunTask: func(ctx context.Context, task state.TaskRow) (string, string, error) {
			return "done", "", nil
		},
	}
	if err := eng.tick(context.Background()); err != nil {
		t.Fatalf("tick must succeed despite UpdateStatus failure: %v", err)
	}
	statuses, err := st.ListStatuses()
	if err != nil {
		t.Fatalf("list statuses: %v", err)
	}
	if len(statuses) != 1 || statuses[0].Status != "done" {
		t.Fatalf("task must still end done despite mark failure, got %+v", statuses)
	}
}
GOEOF
go test -count=1 -run 'TestTier1StatusLabelMutex' ./internal/channel/
go test -count=1 -run 'TestTier1DispatchMarksRunning|TestTier1RunningMarkFailureTolerated' ./internal/daemon/

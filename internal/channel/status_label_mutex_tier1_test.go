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

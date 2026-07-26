package channel

import (
	"reflect"
	"testing"
)

// TestStatusLabelsToRemoveMutex 钉死状态标签互斥语义：连续打两个状态标签后，
// 旧的 loop:<status> 全部进移除集，只剩最新的；loop:task（任务身份证）与非
// loop 前缀的标签永不动；正要保留的新标签不在移除集（幂等重打不摘自己）。
func TestStatusLabelsToRemoveMutex(t *testing.T) {
	labels := []string{"loop:task", "loop:done", "loop:blocked", "bug", "loop:running"}

	// 连续打标的第二轮：第一轮 done（移除 blocked），第二轮 running（移除 done）——
	// 任一时刻只剩最新一个状态标签。
	got := statusLabelsToRemove(labels, "running", "loop:task", "loop:")
	want := []string{"loop:done", "loop:blocked"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("statusLabelsToRemove = %v, want %v", got, want)
	}
}

// TestStatusLabelsToRemoveKeepsTaskLabelAndNonLoop：loop:task 与非 loop: 前缀的
// 标签绝不进移除集（人打的标签、任务身份证不动）。
func TestStatusLabelsToRemoveKeepsTaskLabelAndNonLoop(t *testing.T) {
	got := statusLabelsToRemove([]string{"loop:task", "enhancement", "loop:needs-info"}, "done", "loop:task", "loop:")
	want := []string{"loop:needs-info"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("statusLabelsToRemove = %v, want %v", got, want)
	}
}

// TestStatusLabelsToRemoveIdempotent：重打当前状态 → 移除集为空（不摘自己）；
// 无任何 loop: 状态标签时同样为空。
func TestStatusLabelsToRemoveIdempotent(t *testing.T) {
	if got := statusLabelsToRemove([]string{"loop:task", "loop:done"}, "done", "loop:task", "loop:"); len(got) != 0 {
		t.Fatalf("re-adding current status must remove nothing, got %v", got)
	}
	if got := statusLabelsToRemove([]string{"loop:task", "bug"}, "running", "loop:task", "loop:"); len(got) != 0 {
		t.Fatalf("no status labels → nothing to remove, got %v", got)
	}
	if got := statusLabelsToRemove(nil, "running", "loop:task", "loop:"); len(got) != 0 {
		t.Fatalf("nil labels → nothing to remove, got %v", got)
	}
}

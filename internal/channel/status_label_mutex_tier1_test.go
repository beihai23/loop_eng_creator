package channel

import (
	"reflect"
	"testing"
)

// 合同：statusLabelsToRemove(labels []string, newStatus, taskLabel, prefix string) []string
// 返回 labels 中带 loop: 前缀、!= taskLabel、!= "loop:"+newStatus 的标签，保持输入顺序。
func TestTier1StatusLabelMutex(t *testing.T) {
	// 连续打两个状态后只剩最新的：移除集含所有旧 loop: 状态标签
	got := statusLabelsToRemove([]string{"loop:task", "loop:done", "loop:blocked", "bug"}, "running", "loop:task", "loop:")
	want := []string{"loop:done", "loop:blocked"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("remove set = %v, want %v", got, want)
	}
	// loop:task 永不动；新状态标签不移除（幂等重打）
	if got := statusLabelsToRemove([]string{"loop:task", "loop:running"}, "running", "loop:task", "loop:"); len(got) != 0 {
		t.Fatalf("idempotent re-mark must remove nothing, got %v", got)
	}
	// 非 loop: 前缀标签不受互斥影响
	if got := statusLabelsToRemove([]string{"bug", "loop:task"}, "done", "loop:task", "loop:"); len(got) != 0 {
		t.Fatalf("non-status labels must be kept, got %v", got)
	}
	// 空输入 / 无 loop: 标签 → 空
	if got := statusLabelsToRemove(nil, "done", "loop:task", "loop:"); len(got) != 0 {
		t.Fatalf("nil labels must yield empty, got %v", got)
	}
}

// TestTier1StatusLabelMutexCustomPrefix：自定义前缀实例只剥自己前缀的旧状态标签——
// 别家前缀（loop:task 及 loop:*）一律不碰（多实例共存互不误删，#72 评审发现的
// 边缘 bug：硬编码 "loop:" 剥离会误删别实例的任务身份证）。
func TestTier1StatusLabelMutexCustomPrefix(t *testing.T) {
	labels := []string{"ai:task", "ai:done", "ai:blocked", "loop:task", "loop:done", "bug"}
	got := statusLabelsToRemove(labels, "running", "ai:task", "ai:")
	want := []string{"ai:done", "ai:blocked"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("custom prefix remove set = %v, want %v（loop:* 与 bug 必须保留）", got, want)
	}
	// 空前缀回落 loop:（零值 GitHub 与存量测试行为不变）
	if got := statusLabelsToRemove([]string{"loop:done"}, "running", "loop:task", ""); !reflect.DeepEqual(got, []string{"loop:done"}) {
		t.Fatalf("empty prefix must fall back to loop:, got %v", got)
	}
}

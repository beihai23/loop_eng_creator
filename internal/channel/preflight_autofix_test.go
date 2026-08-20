package channel

import "testing"

// TestPreflightIssueAutoFixable 钉住自愈分类：--fix-preflight 能补的是
// EnsureStatusMarkers 会创建的那两类标记（GitHub 缺标签 / Linear 缺状态列）；
// auth 与 missing-project 是操作者的事，flag 帮不上。CLI 层的提示行据此决定
// 是否建议 --fix-preflight —— 分类漂移会让提示建议一个没用的命令。
func TestPreflightIssueAutoFixable(t *testing.T) {
	cases := []struct {
		code string
		want bool
	}{
		{"missing-label", true},
		{"unresolvable-status", true},
		{"auth", false},
		{"missing-project", false},
		{"", false},
		{"something-new", false}, // 未知码保守视为不可自愈：宁可让人工清单多一项
	}
	for _, tc := range cases {
		if got := (PreflightIssue{Code: tc.code}).AutoFixable(); got != tc.want {
			t.Errorf("AutoFixable(code=%q) = %v, want %v", tc.code, got, tc.want)
		}
	}
}

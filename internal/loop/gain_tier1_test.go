package loop

import "testing"

// TestTier1FailureSignature 钉死 failureSignature/zeroGain 契约（本任务的核心确定性杠杆）。
func TestTier1FailureSignature(t *testing.T) {
	// #71 振荡形态：仅数字不同 → 必须归一为同一签名（重试零增益）
	a := failureSignature("签名不匹配：execute 返回 4 个值，plan 冻结 3 个值")
	b := failureSignature("签名不匹配：execute 返回 2 个值，plan 冻结 3 个值")
	if a == "" || a != b {
		t.Fatalf("振荡形态必须归一为同一签名: a=%q b=%q", a, b)
	}
	// run_b154ff82 形态：逐字相同 → 同签名
	if failureSignature("plan error: claude -p: context canceled") !=
		failureSignature("plan error: claude -p: context canceled") {
		t.Fatal("逐字相同必须同签名")
	}
	// #46 形态：不同符号 → 必须不同签名（正常重试不被误伤）
	if failureSignature("undefined: Foo") == failureSignature("undefined: Bar") {
		t.Fatal("不同符号必须不同签名，否则误伤正常重试")
	}
	// 时间戳/路径/行号差异不构成新信息
	if failureSignature("fail 2024-01-02T03:04:05 /a/b.go:42 x") !=
		failureSignature("fail 2024-01-02T03:04:06 /c/d.go:99 x") {
		t.Fatal("时间戳/路径/行号差异必须归一为同签名")
	}
	if failureSignature("   ") != "" {
		t.Fatalf("空输入必须返回空签名, got %q", failureSignature("   "))
	}
}

func TestTier1ZeroGain(t *testing.T) {
	if zeroGain("", "s") { // 首轮无前置 → 不触发
		t.Fatal("zeroGain(prev=\"\",cur=\"s\") must be false")
	}
	if zeroGain("s", "") { // 当前空 → 不触发
		t.Fatal("zeroGain(prev=\"s\",cur=\"\") must be false")
	}
	if !zeroGain("s", "s") { // 同签名 → 触发
		t.Fatal("zeroGain(prev=\"s\",cur=\"s\") must be true")
	}
}

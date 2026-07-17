package loop

import (
	"strings"
	"testing"

	"loop-eng/internal/channel"
	"loop-eng/internal/verify"
)

func TestTier1VerifyFailComment(t *testing.T) {
	task := channel.Task{Ref: "#26"}
	res := verify.VerifyResult{
		Passed:          false,
		Detail:          "build failed: signature mismatch in call to Foo",
		FailingCriteria: []string{"测试调用必须与函数实际签名一致"},
	}
	body := verifyFailComment(task, 3, res)
	if !strings.Contains(body, "signature mismatch in call to Foo") {
		t.Fatalf("body missing 驳回理由 (res.Detail): %q", body)
	}
	if !strings.Contains(body, "测试调用必须与函数实际签名一致") {
		t.Fatalf("body missing 改进建议 (res.FailingCriteria): %q", body)
	}
	if !strings.Contains(body, "第 3 轮") {
		t.Fatalf("body missing round marker 第 3 轮: %q", body)
	}
}
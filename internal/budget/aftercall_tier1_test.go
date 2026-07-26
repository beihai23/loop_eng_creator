package budget

import (
	"errors"
	"testing"

	"loop-eng/internal/model"
)

// TestTier1AfterCallEnforcesPerCall 钉死本任务新增的 post-call 契约：
// AfterCall 现返回 error——单次真实用量(TokensIn+TokensOut) > PerCall → ErrPerCall；
// ==PerCall 不判违规（严格 >，与 BeforeCall 的 estimate>PerCall 边界一致）。
func TestTier1AfterCallEnforcesPerCall(t *testing.T) {
	e := New(100, 1000, 3)
	// 用量 == PerCall（100）→ 不违规
	if err := e.AfterCall(model.Usage{TokensIn: 60, TokensOut: 40}); err != nil {
		t.Fatalf("usage==PerCall must pass (strict >), got %v", err)
	}
	// 用量 > PerCall（110）→ ErrPerCall（post-call 执法）
	err := e.AfterCall(model.Usage{TokensIn: 60, TokensOut: 50})
	if !errors.Is(err, ErrPerCall) {
		t.Fatalf("usage>PerCall want ErrPerCall, got %v", err)
	}
	// 正常调用（用量 < PerCall）→ nil，不误杀
	if err := e.AfterCall(model.Usage{TokensIn: 10, TokensOut: 5}); err != nil {
		t.Fatalf("normal call (<PerCall) must not be flagged, got %v", err)
	}
}
package budget

import (
	"errors"
	"testing"

	"loop-eng/internal/model"
)

func TestPerCallCapSemantics(t *testing.T) {
	e := New(100, 1000, 3)
	// 单次调用估算超过 per_call 即拒
	if err := e.BeforeCall(150); !errors.Is(err, ErrPerCall) {
		t.Fatalf("want ErrPerCall, got %v", err)
	}
}

func TestPerTaskCap(t *testing.T) {
	e := New(1000, 100, 3)
	e.AfterCall(model.Usage{TokensIn: 60, TokensOut: 30}) // 90 spent
	if err := e.BeforeCall(50); !errors.Is(err, ErrPerTask) {
		t.Fatalf("want ErrPerTask (90+50>100), got %v", err)
	}
}

func TestRetry(t *testing.T) {
	e := New(1000, 1000, 3)
	if !e.ShouldRetry(1) || !e.ShouldRetry(3) || e.ShouldRetry(4) {
		t.Fatal("retry boundary wrong")
	}
}

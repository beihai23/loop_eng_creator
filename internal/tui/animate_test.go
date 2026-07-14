package tui

import (
	"math"
	"testing"
)

func TestBreathBrightness(t *testing.T) {
	// phase 3π/2 → 最低（暗），phase π/2 → 最高（亮）
	dark := breathBrightness(3 * math.Pi / 2)
	bright := breathBrightness(math.Pi / 2)
	if dark >= bright {
		t.Fatalf("dark=%v should be < bright=%v", dark, bright)
	}
	// 范围 [0,1]
	if dark < 0 || bright > 1 {
		t.Fatalf("out of [0,1]: %v %v", dark, bright)
	}
}

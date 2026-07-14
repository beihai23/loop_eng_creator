package tui

import "math"

// breathBrightness 把动画相位映射到 [0,1] 明度（正弦呼吸）。π/2→1（亮），3π/2→0（暗），周期 2π。纯函数，注入 phase 可断言。
func breathBrightness(phase float64) float64 {
	return (math.Sin(phase) + 1) / 2
}

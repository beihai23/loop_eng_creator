package tui

import "math"

// breathBrightness 把动画相位映射到 [0,1] 明度（正弦呼吸）。0→0（暗），
// π/2→1（亮），周期 2π。纯函数，注入 t 可断言。
func breathBrightness(phase float64) float64 {
	return (math.Sin(phase) + 1) / 2
}

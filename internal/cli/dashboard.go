package cli

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
	"loop-eng/internal/tui"
)

// NewDashboardCmd builds `loop-eng dashboard`: 交互式 TUI 看板（spec §3/§5）。
// 读 state.db 展示三 Tab + 颜色/呼吸灯 + resume/cancel。
func NewDashboardCmd() *cobra.Command {
	var repo string
	cmd := &cobra.Command{
		Use:   "dashboard",
		Short: "交互式 TUI 看板（三 Tab + 颜色 + 呼吸灯 + resume/cancel）",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg := mustLoad(repo)
			st := mustOpenState(repo)
			defer st.Close()
			// 非 TTY（管道/重定向）不进 alt-screen，避免刷屏噪声；NO_COLOR/非 TTY
			// 时 tui 包 init() 已把颜色降级为纯文本（spec §6）。
			opts := []tea.ProgramOption{}
			if tui.IsTTY() {
				opts = append(opts, tea.WithAltScreen())
			}
			p := tea.NewProgram(tui.New(st, cfg), opts...)
			_, err := p.Run()
			return err
		},
	}
	cmd.Flags().StringVar(&repo, "repo", ".", "仓库路径")
	return cmd
}

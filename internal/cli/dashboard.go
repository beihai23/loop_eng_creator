package cli

import (
	"github.com/spf13/cobra"
	tea "github.com/charmbracelet/bubbletea"
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
			p := tea.NewProgram(tui.New(st, cfg), tea.WithAltScreen())
			_, err := p.Run()
			return err
		},
	}
	cmd.Flags().StringVar(&repo, "repo", ".", "仓库路径")
	return cmd
}

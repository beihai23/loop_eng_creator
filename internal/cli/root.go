// internal/cli/root.go
package cli

import (
	"os"

	"github.com/spf13/cobra"
)

func NewRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "loop-eng",
		Short: "loop engineering 工具",
	}
	cmd.AddCommand(NewInitCmd())
	cmd.AddCommand(NewRunOnceCmd())
	cmd.AddCommand(NewStatusCmd())
	cmd.AddCommand(NewReplayCmd())
	cmd.AddCommand(NewSkillCmd())
	cmd.AddCommand(NewTaskCmd())
	cmd.AddCommand(NewDaemonCmd())
	cmd.AddCommand(NewDashboardCmd())
	return cmd
}

func Execute() {
	if err := NewRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

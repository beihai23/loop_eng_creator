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
		// Version 让 cobra 自动挂 --version：打印 "loop-eng version <v>"。
		// 版本解析见 version.go（ldflags 注入 → git 短 hash 回退）。
		Version: buildVersion(),
	}
	cmd.AddCommand(NewInitCmd())
	cmd.AddCommand(NewConfigCmd())
	cmd.AddCommand(NewRunOnceCmd())
	cmd.AddCommand(NewStatusCmd())
	cmd.AddCommand(NewReplayCmd())
	cmd.AddCommand(NewSkillCmd())
	cmd.AddCommand(NewTaskCmd())
	cmd.AddCommand(NewDaemonCmd())
	cmd.AddCommand(NewDoctorCmd())
	cmd.AddCommand(NewDashboardCmd())
	cmd.AddCommand(NewWebCmd())
	cmd.AddCommand(NewCleanCmd())
	return cmd
}

func Execute() {
	if err := NewRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

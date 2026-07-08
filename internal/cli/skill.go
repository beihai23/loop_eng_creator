// internal/cli/skill.go
package cli

import (
	"fmt"

	"github.com/spf13/cobra"
	"loop-eng/internal/skill"
)

// NewSkillCmd builds `loop-eng skill` with the `test` subcommand.
//
// `skill test` prints the built-in skill.Defaults (name vVersion). This is
// the M1 placeholder; the full skill edit/regression surface lands in M3+
// (spec §11).
func NewSkillCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skill",
		Short: "skill 相关命令（M1 仅 test 子命令）",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "test",
		Short: "列出内置 skill（M1 占位；完整回归集见 spec §11）",
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			for _, e := range skill.Defaults {
				fmt.Fprintf(out, "%s v%s\n", e.Name, e.Version)
			}
			return nil
		},
	})
	return cmd
}

// internal/cli/replay.go
package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// NewReplayCmd builds `loop-eng replay`: prints a run's steps in seq order
// (seq role status per line). Read-only view over the steps table via
// state.Store.Replay.
func NewReplayCmd() *cobra.Command {
	var repo, run string
	cmd := &cobra.Command{
		Use:   "replay",
		Short: "按 seq 回放某次 run 的 steps",
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			st := mustOpenState(repo)
			defer st.Close()
			steps, err := st.Replay(run)
			if err != nil {
				return err
			}
			for _, s := range steps {
				fmt.Fprintf(out, "%d %s %s\n", s.Seq, s.Role, s.Status)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&repo, "repo", ".", "仓库路径")
	cmd.Flags().StringVar(&run, "run", "", "run id（必填）")
	return cmd
}

// internal/cli/clean.go
package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"loop-eng/internal/loop"
)

// NewCleanCmd builds `loop-eng clean`: the operator-facing trigger of the
// worktree scene GC (loop.GCWorktrees). State-driven (task_status decides what
// stays), with a grace window (default 48h) that never touches young trees and
// a TTL (default 7d) for blocked failure-scene caches. The daemon runs the same
// GC every tick; this command is for manual inspection (--dry-run) and cleanup.
func NewCleanCmd() *cobra.Command {
	var repo string
	var dryRun bool
	var grace, sceneTTL time.Duration
	cmd := &cobra.Command{
		Use:   "clean",
		Short: "清理 .loop/worktrees 的现场缓存（状态驱动 GC，48h 宽限期）",
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			st := mustOpenState(repo)
			defer st.Close()
			pol := loop.GCPolicy{Now: time.Now(), Grace: grace, SceneTTL: sceneTTL, DryRun: dryRun}
			acts, err := loop.GCWorktrees(repo, st, pol)
			if err != nil {
				return err
			}
			if len(acts) == 0 {
				fmt.Fprintln(out, "no worktrees under .loop/worktrees")
				return nil
			}
			for _, a := range acts {
				verb := "keep"
				if a.Delete && dryRun {
					verb = "would-delete"
				} else if a.Delete {
					verb = "deleted"
				}
				fmt.Fprintf(out, "%-13s %s (%s)\n", verb, a.Name, a.Reason)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&repo, "repo", ".", "仓库路径")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "只打印判定结果，不实际删除")
	cmd.Flags().DurationVar(&grace, "grace", loop.DefaultGCGrace, "宽限期：此年龄内的树一律保留（防误删活树）")
	cmd.Flags().DurationVar(&sceneTTL, "scene-ttl", loop.DefaultGCSceneTTL, "blocked 失败现场缓存的保留时长")
	return cmd
}

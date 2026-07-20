// internal/cli/doctor.go
package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"loop-eng/internal/channel"
)

// NewDoctorCmd builds `loop-eng doctor`: an on-demand channel readiness check
// (preflight). It runs the same Preflight the daemon runs at startup, but only
// reports — it never starts the engine. Output is a ready report (✅ ready /
// ❌ checklist of missing prerequisites) so the operator can fix gaps before
// booting the daemon (#65). Exits non-zero when not ready so CI/scripts can gate.
func NewDoctorCmd() *cobra.Command {
	var repo, channelFlag string
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "channel 就绪性校验（preflight）：列出缺失的前置依赖，不启动 daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := mustLoad(repo)
			if channelFlag != "" {
				cfg.Channel.Provider = channelFlag
			}
			ch, err := buildChannel(cfg, repo)
			if err != nil {
				return err
			}
			prov := cfg.Channel.Provider
			if prov == "" {
				prov = "local"
			}

			pf, ok := ch.(channel.Preflighter)
			if !ok {
				// Local and any provider without prerequisites — always ready.
				fmt.Printf("channel=%s：无前置依赖，就绪 ✅\n", prov)
				return nil
			}

			fmt.Printf("channel=%s：开始就绪性校验（preflight）...\n", prov)
			issues, err := pf.Preflight(context.Background())
			if err != nil {
				fmt.Printf("❌ 校验未能完成：%v\n", err)
				return fmt.Errorf("preflight: %w", err)
			}
			if len(issues) == 0 {
				fmt.Printf("✅ 就绪：所有前置依赖已配置\n")
				return nil
			}
			fmt.Printf("❌ 未就绪：缺 %d 项前置依赖\n", len(issues))
			fmt.Print(formatPreflightIssues(issues))
			return fmt.Errorf("channel 未就绪：%d 项前置依赖缺失", len(issues))
		},
	}
	cmd.Flags().StringVar(&repo, "repo", ".", "仓库路径")
	cmd.Flags().StringVar(&channelFlag, "channel", "", "local | github | linear（空=用 cfg.Channel.Provider）")
	return cmd
}

// runPreflight is the daemon's startup gate. It runs the channel's Preflight
// (when the channel implements it) and returns a checklist-formatted error when
// prerequisites are missing, so the daemon refuses to start — fail fast at boot
// instead of crashing mid-run (e.g. a missing loop:running label only surfacing
// when UpdateStatus hits it, #54/#65). Channels without prerequisites (Local)
// are always ready.
func runPreflight(ctx context.Context, ch channel.Channel) error {
	pf, ok := ch.(channel.Preflighter)
	if !ok {
		return nil
	}
	issues, err := pf.Preflight(ctx)
	if err != nil {
		return fmt.Errorf("preflight: %w", err)
	}
	if len(issues) == 0 {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "channel 未就绪：缺 %d 项前置依赖（`loop-eng doctor` 查看详情）：\n", len(issues))
	b.WriteString(formatPreflightIssues(issues))
	return errors.New(b.String())
}

// formatPreflightIssues renders the issue list as a indented checklist
// (code + message), one bullet per line. Shared by the daemon's startup error
// and the doctor report so the two stay in lockstep.
func formatPreflightIssues(issues []channel.PreflightIssue) string {
	var b strings.Builder
	for _, is := range issues {
		fmt.Fprintf(&b, "  - [%s] %s\n", is.Code, is.Message)
	}
	return b.String()
}

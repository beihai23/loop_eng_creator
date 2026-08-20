// internal/cli/doctor.go
package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"loop-eng/internal/channel"
	"loop-eng/internal/config"
	"loop-eng/internal/model"
)

// NewDoctorCmd builds `loop-eng doctor`: an on-demand readiness check. It runs
// TWO preflights and only reports — it never starts the engine:
//   - channel preflight (the daemon's #65 startup gate): GitHub labels, Linear
//     project, etc.
//   - provider preflight: each configured model role's coding-agent binary must
//     be on PATH and its auth in place (claude binary, codex binary+CODEX_API_KEY,
//     …) — spec §8.10 agent provider neutrality. A misconfigured provider would
//     crash mid-run (e.g. binary missing only surfacing when Execute shells out),
//     so doctor surfaces it before boot.
//
// Output is a ready report (✅ ready / ❌ checklist of missing prerequisites) so
// the operator can fix gaps before booting the daemon. Exits non-zero when not
// ready so CI/scripts can gate.
func NewDoctorCmd() *cobra.Command {
	var repo, channelFlag string
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "就绪性校验：channel + coding-agent provider 的前置依赖，不启动 daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(filepath.Join(repo, ".loop", "config.yaml"))
			if err != nil {
				fmt.Printf("❌ config load：%v\n", err)
				return err
			}
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

			// ---- channel preflight ----
			var chanIssues []channel.PreflightIssue
			if pf, ok := ch.(channel.Preflighter); ok {
				issues, err := pf.Preflight(context.Background())
				if err != nil {
					fmt.Printf("❌ channel 校验未能完成：%v\n", err)
					return fmt.Errorf("preflight: %w", err)
				}
				chanIssues = issues
			}

			// ---- provider preflight (coding-agent binary + auth per role) ----
			provIssues := providerPreflight(cfg)

			total := len(chanIssues) + len(provIssues)
			if total == 0 {
				fmt.Printf("channel=%s：✅ 就绪（channel 与 provider 前置依赖均已配置）\n", prov)
				return nil
			}
			fmt.Printf("channel=%s：❌ 未就绪，缺 %d 项前置依赖（channel %d + provider %d）\n",
				prov, total, len(chanIssues), len(provIssues))
			fmt.Print(formatPreflightIssues(chanIssues))
			if hint := fixPreflightHint(chanIssues); hint != "" {
				fmt.Printf("提示：%s\n", hint)
			}
			for _, msg := range provIssues {
				fmt.Printf("  - %s\n", msg)
			}
			return fmt.Errorf("未就绪：%d 项前置依赖缺失", total)
		},
	}
	cmd.Flags().StringVar(&repo, "repo", ".", "仓库路径")
	cmd.Flags().StringVar(&channelFlag, "channel", "", "local | github | linear（空=用 cfg.Channel.Provider）")
	return cmd
}

// providerPreflight validates every configured model role's coding-agent: each
// config.ModelRef must resolve to a registered Agent (NewAgent) AND that agent's
// Check (binary on PATH + auth) must pass. Returns one human-readable issue
// string per failure (role-prefixed). Used by `loop-eng doctor` so a missing
// binary or credential surfaces before the daemon shells out mid-run.
func providerPreflight(cfg *config.Config) []string {
	var issues []string
	for _, r := range []struct {
		role string
		ref  config.ModelRef
	}{
		{"triage", cfg.Models.Triage},
		{"plan", cfg.Models.Plan},
		{"execute", cfg.Models.Execute},
		{"verify", cfg.Models.Verify},
	} {
		a, err := model.NewAgent(r.ref)
		if err != nil {
			issues = append(issues, fmt.Sprintf("[provider] models.%s: %v", r.role, err))
			continue
		}
		if err := a.Check(context.Background()); err != nil {
			issues = append(issues, fmt.Sprintf("[provider] models.%s (%s): %v", r.role, a.Provider(), err))
		}
	}
	return issues
}

// runPreflight is the daemon's startup gate. It runs the channel's Preflight
// (when the channel implements it) and returns a checklist-formatted error when
// prerequisites are missing, so the daemon refuses to start — fail fast at boot
// instead of crashing mid-run (e.g. a missing loop:running label only surfacing
// when UpdateStatus hits it, #54/#65). Channels without prerequisites (Local)
// are always ready. When any missing item is auto-fixable, the error appends
// the `daemon --fix-preflight` hint so the operator learns the one-flag path.
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
	if hint := fixPreflightHint(issues); hint != "" {
		fmt.Fprintf(&b, "提示：%s\n", hint)
	}
	return errors.New(b.String())
}

// autoFixPreflight is `daemon --fix-preflight`'s provisioning pass: run the
// channel's StatusEnsurer (GitHub loop:<status> labels / Linear WorkflowStates)
// BEFORE the preflight gate, so auto-provisionable prerequisites exist by the
// time Preflight lists the residual. Best-effort by contract (GitHub logs
// create failures and returns nil; Linear returns an error its caller logs) —
// preflight stays the hard gate either way. Channels without status markers
// (Local) are a no-op nil.
func autoFixPreflight(ctx context.Context, ch channel.Channel) error {
	se, ok := ch.(channel.StatusEnsurer)
	if !ok {
		return nil
	}
	return se.EnsureStatusMarkers(ctx)
}

// fixPreflightHint returns the `daemon --fix-preflight` suggestion for the
// issue list, or "" when no issue is auto-fixable — suggesting the flag for an
// auth/missing-project gap it can't heal would just waste an operator round.
func fixPreflightHint(issues []channel.PreflightIssue) string {
	for _, is := range issues {
		if is.AutoFixable() {
			return "可自动补齐的项（缺失标签/状态列）——执行 `loop-eng daemon --fix-preflight` 自动修复后重启"
		}
	}
	return ""
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

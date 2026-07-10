// internal/cli/daemon.go
package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"loop-eng/internal/budget"
	"loop-eng/internal/channel"
	"loop-eng/internal/daemon"
	"loop-eng/internal/loop"
	"loop-eng/internal/state"
	"loop-eng/internal/verify"
)

// NewDaemonCmd builds `loop-eng daemon`: the resident loop engine (spec §8.3).
// It polls the ticket channel every --poll-interval, ingests new tasks, dispatches
// them one at a time (single-active, synchronous — spec §12: no concurrency), and
// parks/resumes tier-3 needs-review tasks. The daemon never blocks on a human
// (spec principle 7): tier-3 parks → releases the active slot → polls replies → resumes.
func NewDaemonCmd() *cobra.Command {
	var repo, channelFlag, models string
	var pollInterval, cooldown time.Duration
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "常驻 loop 引擎（轮询工单 + 单活跃子 loop + park/resume）",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := mustLoad(repo)
			st := mustOpenState(repo)
			defer st.Close()

			if channelFlag != "" {
				cfg.Channel.Provider = channelFlag
			}
			ch, err := buildChannel(cfg, repo)
			if err != nil {
				return err
			}

			// RunTask: construct a fresh SubLoop per dispatched task + run it.
			// SubLoop does the full plan→execute→verify→writeback (incl. channel
			// comment + status mark via report()). PreinsertedTaskID = the daemon's
			// already-ingested task ID (avoids duplicate InsertTask).
			runTask := func(ctx context.Context, task state.TaskRow) (string, string, error) {
				bz := budget.New(cfg.Budget.PerCallTokens, cfg.Budget.PerTaskTokens, cfg.Budget.MaxRetries)
				exec, plan, verifySkill, _ := buildModels(cfg, models, bz)

				dets := make([]verify.Deterministic, 0, len(cfg.Verify.Deterministic))
				for _, d := range cfg.Verify.Deterministic {
					dets = append(dets, verify.Deterministic{Label: d.Label, Cmd: d.Cmd})
				}

				sl := &loop.SubLoop{
					Repo:                repo,
					Store:               st,
					Budget:              bz,
					Execute:             exec,
					Plan:                plan,
					VerifyDeterministic: dets,
					VerifyLLM:           verify.LLM{Skill: verifySkill},
					Tier3Human:          cfg.Verify.Tier3Human,
					Channel:             ch,
					PreinsertedTaskID:   task.ID,
				}

				ct := channel.Task{
					Ref:                task.IssueRef,
					Description:        task.Description,
					TaskType:           task.TaskType,
					AcceptanceCriteria: task.Criteria,
				}
				out, err := sl.Run(ctx, ct)
				if err != nil {
					fmt.Fprintf(os.Stderr, "[daemon] task %s error: %v\n", task.ID, err)
					return "error", "", err
				}
				// Land done work on main: SubLoop committed the execute output on the
				// worktree's branch; FF-merge it here + clean up the worktree. A land
				// failure (non-FF) leaves the branch intact — the work is safe, only
				// the integration is deferred. Never flips the done outcome.
				if out.Status == "done" && out.Branch != "" {
					if lerr := land(repo, out.Worktree, out.Branch); lerr != nil {
						fmt.Fprintf(os.Stderr, "[daemon] task %s land failed (work safe on branch %s): %v\n", task.ID, out.Branch, lerr)
					} else {
						fmt.Printf("[daemon] task %s landed on main\n", task.ID)
					}
				}
				fmt.Printf("[daemon] task %s → %s\n", task.ID, out.Status)
				return out.Status, out.Detail, nil
			}

			interval := pollInterval
			if cfg.Daemon.PollInterval > 0 {
				interval = cfg.Daemon.PollInterval
			}
			eng := &daemon.Engine{
				Channel:  ch,
				Store:    st,
				Interval: interval,
				Cooldown: cooldown,
				RunTask:  runTask,
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
			go func() {
				<-sigCh
				fmt.Fprintln(os.Stderr, "[daemon] shutting down...")
				cancel()
			}()

			fmt.Printf("[daemon] starting (poll=%s, channel=%s)\n", pollInterval, cfg.Channel.Provider)
			return eng.Run(ctx)
		},
	}
	cmd.Flags().StringVar(&repo, "repo", ".", "仓库路径")
	cmd.Flags().StringVar(&channelFlag, "channel", "", "local | github（空=用 cfg.Channel.Provider）")
	cmd.Flags().StringVar(&models, "models", "real", "real | fake")
	cmd.Flags().DurationVar(&pollInterval, "poll-interval", 60*time.Second, "轮询间隔")
	cmd.Flags().DurationVar(&cooldown, "cooldown", 5*time.Minute, "瞬时基础设施阻塞（如上游 529 限流）后的派发冷却时长")
	return cmd
}

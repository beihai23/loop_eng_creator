// internal/cli/status.go
package cli

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"loop-eng/internal/state"
)

// watchInterval is the default --watch refresh cadence. A standard-library
// time.Ticker drives the refresh (spec §8.7 observability: a live view of the
// active sub-loop + every task status, no new external deps). 2s reads as live
// without hammering state.db.
const watchInterval = 2 * time.Second

// NewStatusCmd builds `loop-eng status`: prints task statuses from state.db.
//
// 裁决 F: status does NOT touch the Store's private db field. The list path
// goes through state.Store.ListStatuses (added in state.go for this task);
// the single-task path uses GetTask; the --watch live view adds Store.InFlight
// (spec §8.7 observability + §8.3 single-active). All public Store methods.
func NewStatusCmd() *cobra.Command {
	var repo, task string
	var watch bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "打印任务态（list 或 --task <id> 单条；--watch 实时视图）",
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			st := mustOpenState(repo)
			defer st.Close()
			if watch {
				return runWatch(st, out, watchInterval, watchSignals())
			}
			if task != "" {
				t, err := st.GetTask(task)
				if err != nil {
					return err
				}
				fmt.Fprintf(out, "%s %s %s\n", t.ID, t.TaskType, t.Description)
				return nil
			}
			rows, err := st.ListStatuses()
			if err != nil {
				return err
			}
			for _, r := range rows {
				fmt.Fprintf(out, "%s %s\n", r.ID, r.Status)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&repo, "repo", ".", "仓库路径")
	cmd.Flags().StringVar(&task, "task", "", "查看指定任务（id）")
	cmd.Flags().BoolVar(&watch, "watch", false, "实时视图：顶部活跃 task+phase，定期刷新（Ctrl-C 退出）")
	return cmd
}

// watchSignals returns a channel that receives the first SIGINT/SIGTERM — the
// signals `loop-eng status --watch` exits on (spec §8.7 live view: Ctrl-C exits;
// the watch never self-exits). Mirrors daemon.go's shutdown wiring.
func watchSignals() <-chan os.Signal {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	return sigCh
}

// runWatch is the --watch refresh loop (spec §8.7). It renders one frame
// immediately, then every interval, reading the active sub-loop (Store.InFlight,
// spec §8.3 single-active) + every task status. It never exits on its own; it
// returns nil when sigCh fires (Ctrl-C). The frame is cleared between refreshes
// with an ANSI clear+home so the view reads as one updating block, not a scroll.
//
// sigCh is injected (not built here) so a test can pre-feed a signal and assert
// the loop exits cleanly without racing real time. The pure frame rendering
// lives in renderWatchView; runWatch is the timing/signal glue.
func runWatch(st *state.Store, out io.Writer, interval time.Duration, sigCh <-chan os.Signal) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	render := func() {
		in, _, _ := st.InFlight()        // best-effort: empty InFlight on read error
		rows, _ := st.ListStatuses()     // best-effort: empty list on read error
		fmt.Fprint(out, "\033[H\033[2J") // clear screen + cursor home
		fmt.Fprint(out, renderWatchView(in, rows))
	}
	render()
	for {
		select {
		case <-ticker.C:
			render()
		case <-sigCh:
			return nil
		}
	}
}

// renderWatchView renders one --watch frame (spec §8.7): the active sub-loop
// (task + phase) on top, every task status below. Pure — given an InFlight read
// and the status rows it returns the frame text, so tests inject a fake
// InFlight + rows and assert on the string without touching time or signals.
// An empty Phase (dispatched but no step landed yet) renders as "running".
func renderWatchView(in state.InFlight, rows []state.StatusRow) string {
	var b strings.Builder
	b.WriteString("loop-eng status — watch（Ctrl-C 退出）\n")
	b.WriteString("\n")
	b.WriteString("活跃子 loop:\n")
	if in.TaskID == "" {
		b.WriteString("  （无活跃任务）\n")
	} else {
		phase := in.Phase
		if phase == "" {
			phase = "running"
		}
		fmt.Fprintf(&b, "  %s  phase=%s\n", in.TaskID, phase)
	}
	b.WriteString("\n")
	b.WriteString("任务态:\n")
	if len(rows) == 0 {
		b.WriteString("  （无）\n")
	}
	for _, r := range rows {
		fmt.Fprintf(&b, "  %s %s\n", r.ID, r.Status)
	}
	return b.String()
}

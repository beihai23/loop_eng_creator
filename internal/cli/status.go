// internal/cli/status.go
package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// NewStatusCmd builds `loop-eng status`: prints task statuses from state.db.
//
// 裁决 F: status does NOT touch the Store's private db field. The list path
// goes through state.Store.ListStatuses (added in state.go for this task);
// the single-task path uses GetTask. Both are public Store methods.
func NewStatusCmd() *cobra.Command {
	var repo, task string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "打印任务态（list 或 --task <id> 单条）",
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			st := mustOpenState(repo)
			defer st.Close()
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
	return cmd
}

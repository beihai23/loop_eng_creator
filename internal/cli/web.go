package cli

import (
	"fmt"

	"github.com/spf13/cobra"
	"loop-eng/internal/web"
)

// NewWebCmd builds `loop-eng web`: a read-only HTTP dashboard over the state DB,
// the web counterpart of `loop-eng dashboard`. The SPA (go:embed vanilla JS) is
// self-contained — no npm, no CDN — and the single control endpoint (POST
// /command) writes resume/cancel rows the daemon drains, exactly like the TUI.
//
// --addr defaults to 127.0.0.1:7474 and binds loopback only: the resume/cancel
// control channel must not be exposed beyond the developer's machine. --repo
// defaults to "." (same convention as every other loop-eng command).
func NewWebCmd() *cobra.Command {
	var addr, repo string
	cmd := &cobra.Command{
		Use:   "web",
		Short: "Web UI 仪表盘（只读概览/详情/时间线 + resume/cancel 控制通道）",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg := mustLoad(repo)
			st := mustOpenState(repo)
			defer st.Close()
			srv := web.New(st, cfg)
			fmt.Fprintf(cmd.OutOrStdout(), "loop-eng web 监听 %s\n", addr)
			fmt.Fprintf(cmd.OutOrStdout(), "打开浏览器访问: http://%s\n", addr)
			return srv.ListenAndServe(addr)
		},
	}
	cmd.Flags().StringVar(&addr, "addr", "127.0.0.1:7474", "监听地址（默认仅绑本机：resume/cancel 控制通道不对外暴露）")
	cmd.Flags().StringVar(&repo, "repo", ".", "仓库路径")
	return cmd
}

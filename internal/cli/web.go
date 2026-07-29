package cli

import (
	"fmt"
	"net"

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
		// SilenceUsage: bind 失败（端口被占用等）只报一行清晰错误，不再 dump 完整
		// Usage 干扰用户——usage 对「端口冲突」毫无帮助。
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg := mustLoad(repo)
			st := mustOpenState(repo)
			defer st.Close()
			srv := web.New(st, cfg)
			// 先绑定，再广告：net.Listen 成功的那一刻地址才真正在监听，杜绝打印
			// 任何随后 bind 失败的不可达链接（ERR_CONNECTION_REFUSED 的根因）。
			ln, err := net.Listen("tcp", addr)
			if err != nil {
				return fmt.Errorf("无法监听 %s: %w（端口可能被占用；可用 --addr 指定其他端口，例如 --addr 127.0.0.1:7475）", addr, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "loop-eng web 监听 %s\n", addr)
			fmt.Fprintf(cmd.OutOrStdout(), "打开浏览器访问: http://%s\n", addr)
			return srv.Serve(ln)
		},
	}
	cmd.Flags().StringVar(&addr, "addr", "127.0.0.1:7474", "监听地址（默认仅绑本机：resume/cancel 控制通道不对外暴露）")
	cmd.Flags().StringVar(&repo, "repo", ".", "仓库路径")
	return cmd
}

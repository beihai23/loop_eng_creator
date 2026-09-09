// internal/cli/version.go
package cli

import (
	"fmt"
	"runtime/debug"
	"strings"
	"time"
)

// version 是发行版本号：release 构建经 -ldflags 注入（见 Makefile 的 VERSION）。
// 空 = 未注入（go build / go install 直出）：resolveVersion 回退到 go build 自动
// 嵌入的 VCS 信息（git 仓库内构建默认带 vcs.revision / vcs.modified）。
var version = ""

// resolveVersion 解析生效版本，优先级：
//  1. ldflags 注入的版本号（release 构建唯一真源）；
//  2. vcs.revision 短 hash（7 位）+ "+dirty"（工作树有未提交改动）；
//  3. 模块版本（go install 依赖路径下非 (devel)）；
//  4. "(dev)"（三种都拿不到：非 git 环境的源码包构建）。
//
// 纯函数（注入值与 BuildInfo 都是入参），决定论可测；buildVersion 只是取真实输入。
func resolveVersion(injected string, bi *debug.BuildInfo) string {
	if v := strings.TrimSpace(injected); v != "" {
		return v
	}
	if bi != nil {
		var rev, dirty string
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				if s.Value == "true" {
					dirty = "+dirty"
				}
			}
		}
		if rev != "" {
			if len(rev) > 7 {
				rev = rev[:7]
			}
			return rev + dirty
		}
		if v := bi.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}
	return "(dev)"
}

// buildVersion 解析当前二进制的生效版本。
func buildVersion() string {
	bi, _ := debug.ReadBuildInfo()
	return resolveVersion(version, bi)
}

// logo 是启动横幅的字标（ANSI Shadow 风格 "loop"）。
const logo = `██╗      ██████╗  ██████╗ ██████╗
██║     ██╔═══██╗██╔═══██╗██╔══██╗
██║     ██║   ██║██║   ██║██████╔╝
██║     ██║   ██║██║   ██║██╔═══╝
███████╗╚██████╔╝╚██████╔╝██║
╚══════╝ ╚═════╝  ╚═════╝ ╚═╝`

// banner 渲染 daemon 启动横幅：字标 + 版本 + 一句话定位 + 作者/版权。
func banner() string {
	return logo + "\n" +
		fmt.Sprintf("loop-eng %s — 让一个 LLM 自己跑「写代码 → 自测 → 收尾」的闭环\n", buildVersion()) +
		fmt.Sprintf("author beihai23 · MIT license · © %d beihai23\n", time.Now().Year())
}

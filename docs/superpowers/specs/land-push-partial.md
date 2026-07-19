# land push 失败：重试 + LAND PARTIAL 契约（#57，2026-07-19）

## 问题

verify 通过后 land 阶段 `git push origin <branch>` 失败（实证：SSH 经 VPN TUN 网关
198.18.x.x 瞬时抖动，exit 128），daemon 静默 fallback 到本地 FF-merge，照样标 done、
照发 DONE 评论——代码只到本地 main、没上 origin、无 PR，operator 完全无感知；且本地
main 领先 origin 制造分叉（下次 `git pull --ff-only` 失败、强推又把这份带上去）。

## 行为契约

1. **push 重试**：`createPR` 的 push 对网络类错误重试 3 次、2s/4s 退避（镜像
   `channel/github.go` 的 ghRetry 策略）。
2. **push 最终失败 → LAND PARTIAL，不静默 done**：
   - **不 FF-merge 到本地 main**（merge 进 push 不上去的 main 就是制造分叉）；
   - **保留 branch + worktree**（不 Discard），供 operator 手动 push；
   - done detail 追加含字面量 `LAND PARTIAL` 的标记串（branch 名 + 手动恢复指令
     `git push -u origin <branch>` + push 错误摘要）；
   - issue 上追加一条含 `LAND PARTIAL` 的评论（DONE 战报由 SubLoop.report 在 land
     之前发出，partial 只能以追加评论到达 operator）。
3. **非 push 类 PR 失败**（push 已成功、`gh pr create` 挂了；无 remote/gh 不可用）：
   保持既有本地 FF-merge 兜底，行为不回归。

## 钉死的 Go 签名（internal/cli/land.go，逐字，不得改名/改签名）

```go
var ErrPushFailed = errors.New("git push failed")

// err 为小写匹配以下子串之一即 true：connection closed / connection reset /
// connection refused / connection timed out / could not resolve hostname /
// temporary failure in name resolution / network is unreachable /
// operation timed out / failed to connect / ssh: connect to host。
// "permission denied" / "no configured push destination" / "non-fast-forward" 必须 false。
func isRetryablePushError(err error) bool

// 最多 3 次尝试；第 i 次（1-based）失败且 i<3 且 isRetryablePushError(err) 时
// sleep(time.Duration(i)*2*time.Second)（2s、4s 退避）后重试；否则返回最后错误。
func pushWithRetry(push func() error, sleep func(time.Duration)) error

// errors.Is(prErr, ErrPushFailed)：logf 一行告警、不调 land()（不 merge、不 Discard），
// 返回含 LAND PARTIAL + branch + 手动恢复指令 + push 错误摘要的标记串；
// 否则调 land()：成功返回 ""，失败 logf 并返回 "[land failed: ...]"。
func landFallback(repo, wt, branch string, prErr error, logf func(format string, args ...any)) string

// createPR 的 push 改为 pushWithRetry(runGit push, time.Sleep)；失败返回
// fmt.Errorf("%w: %v", ErrPushFailed, err)。gh pr create / Discard 逻辑不变。
func createPR(repo, ghRepo, branch, wt, title, body string) (string, error)

// landFallback + push 失败时 best-effort ch.PostComment(issueRef, extra)（DONE 战报
// 已先发，partial 以追加评论到达；评论失败只 logf，不翻转结果）。daemon runTask 与
// run-once 共用；返回的 extra 非空时由调用方追加到 out.Detail。
func handlePRFailure(ctx context.Context, ch channel.Channel, issueRef, repo, wt, branch string, prErr error, logf func(format string, args ...any)) string
```

`land()` 本身不改（非 FF 仍报错并保留 worktree+branch——工作永不丢）。

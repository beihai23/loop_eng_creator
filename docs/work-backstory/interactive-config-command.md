---
arc: interactive-config-command
started: f2efa95
status: active
commits: []
---

# 交互式 `loop-eng config` 命令（合并 init）

## Intent
新增交互式 `loop-eng config` 子命令：一条命令搞定脚手架（原 init 的活）+ channel
provider 引导配置（local / github / linear），`init` 降级为向后兼容别名。动机是
 onboarding 断点：原来 init 只生成默认 local 配置，接 GitHub/Linear 要手改 YAML 并
查字段名；linear 的 API key 还有「不进 config.yaml」（#24 决定 A）的安全约束，手改
极易踩坑。

## Process
- 前 3 轮 verify 全驳回，根因同一类：tier-1 验收脚本由 planner 逐轮「想象」签名
  生成（见 [[tier1-contract-is-planner-imagined]] 记忆），每轮签名校验面都在漂——
  第 1 轮要 `ghInstallInstructions()` 无参，第 2 轮变 `(string)`；第 3 轮
  `Channel` 结构体要平铺的 `Project/StatusMap/Inbox` 字段。实现端追不上漂移。
- 本轮（重跑）planner 学乖了：plan 输出里**钉死全部签名**（「executor 必须一字不差
  实现」），且把 tier-1 测试体直接落进 state.db 的 steps.output_json。execute 阶段
  从 `.loop/state.db` 捞出本轮 verify_script 原文（`internal/cli/config_tier1_test.go`），
  照合约实现——**先读 DB 里的 plan 输出再动手**是这类任务的最短路径。
- 结构决策：linear 配置用**嵌套块** `channel.linear.{project,team,status_map}`
  （plan 钉死；与 spec `linear-channel-mapping.md` §9 形态一致），不是第 3 轮测试
  想象的平铺字段——说明 tier-1 合约以**本轮 plan 输出**为准，历史驳回评论里的签名
  仅供参考趋势，不可当作目标。
- `Save(path, c)` 不在写时 validate（Load 是唯一校验门），理由：Save/Load round-trip
  是 tier-1 断言面，写时校验会把「写得出但读不回」的半坏状态藏到更晚。
- 交互零新依赖（bufio 行读）。关键坑：**一个 `bufio.Reader` 贯穿所有 prompt**——
  每个 prompt 新建 reader 会把后续 stdin 缓冲吞掉（plan 明确点名此约束）。
- EOF 语义是 init 兼容性的命门：脚本/CI 里 stdin 立即 EOF → provider 选择返回 ""
  → 只脚手架+保持默认配置、退出 0，与旧 init 等价（多一行 deprecation 到 stderr）。
  `go test` 下测试二进制 stdin 是 /dev/null，旧 init 测试天然走这条路，不会挂。
- gh 检测（`ghAvailable`/`ghAuthed`）只影响提示、不阻塞、不消费 stdin——否则
  脚本化 stdin 驱动 github 流程时会错位。
- linear key 两条出路：①打印 `export LOOP_ENG_LINEAR_API_KEY=<key>` 让用户加 shell rc；
  ②写 `.loop/linear.key`（0600）。`.loop/` 整体已在 .gitignore（裁决 H），无需单独
  gitignore key 文件。key 在任何路径下都不进 config.yaml，tier-1 有专门断言。
- linear channel 本体不在本任务范围：`buildChannel` 只把 `case "linear"` 从笼统的
  unknown provider 换成明确的「尚未实现（见 #30）」错误。
- local provider 的 inbox 配置落 `channel.inbox` → `channel.Local.InboxDir`（空串回落
  "inbox"，零值兼容，`NewLocal` 签名不动）。

## Decisions
- tier-1 合约以 state.db 里本轮 plan 输出的 verify_script 为唯一准绳；开工先捞。
- linear 配置嵌套块 + `Save` 不写时校验 + 单 bufio.Reader 贯穿 + EOF=保持现状。
- `config_tier1_test.go` 照仓内既有惯例（tui/state 已有多份 *_tier1_test.go）提交进库。

## Lessons
- planner 想象的签名逐轮漂移时，别猜——plan 输出（steps.output_json）里有钉死的合约
  甚至测试全文；`sqlite3 .loop/state.db` 直接读。
- 交互 CLI 要能被管道 stdin 完整驱动（测试与 CI 都依赖），所有输出走注入的 io.Writer，
  禁止 fmt.Print* 直写 stdout。

## Related
- spec: docs/superpowers/specs/linear-channel-mapping.md（§9 config 形态）
- 记忆: tier1-contract-is-planner-imagined
- 文件: internal/cli/config.go（新）、internal/cli/init.go（scaffoldLoop 抽出）、
  internal/config/config.go（Channel.Inbox/LinearChannel/Save）、
  internal/channel/local.go（InboxDir）、internal/cli/run_once.go（buildChannel）

---
arc: dashboard-initial-prompt
started: 86a6161bca64e191d08ce15c875841417c74643a
status: resolved
commits: [8189015]
---

# dashboard 详情页展示任务最近一次启动的初始提示词

## Intent
用户诉求：dashboard 查看任务详情时，能看到该任务最近一次启动（最新 run）时得到的初始提示词。
现状盘点（2026-07-18）：
- execute prompt 已落盘：SubLoop 每个 attempt 的 execute step `input_json` = 完整 execPrompt（含战报/issue 评论）。
- plan prompt 未落盘：plan step 只记 status/error，`input_json` 为空——这是 trace 的一个缺口。
- 详情页 `RenderDetail` 已能解析「最新 run」（优先 active run，否则 RunsOfTask 末尾）。

初始预期：补齐 plan prompt 落盘（导出 skill.render 为 RenderPrompt，SubLoop 渲染后写进 plan step
InputJSON，不动 Skill.Run 冻结签名）；state 加按 run 取「首轮 plan/execute 输入」的读方法；
详情页加「初始提示词」小节展示两者；TUI 详情 tab 支持 j/k/↑/↓ 滚动看全文。

## Process
- 需求澄清（用户拍板）：两个 prompt 都显示（plan + execute），不要二选一；长提示词用滚动查看全文，
  不做截断。
- 关键发现：execute prompt 本来就落盘（每个 attempt 的 execute step `input_json`），plan prompt
  从未落盘——plan step 只记 status/error。所以「两个都显示」必须先补 plan 的落盘，否则详情页
  永远只能显示一半。
- 落盘方式的选择：没有改 `Skill.Run` 的签名（冻结接口，spec §8.5 原则 6），而是把
  `skill.render` 导出为 `RenderPrompt`，SubLoop 用同一模板+输入渲染一份写进 plan step 的
  InputJSON。代价是渲染两次（纯内存 template render，无模型调用），收益是不动冻结签名。
- 滚动的坑：bubbletea 的 `View()` 是值接收者，显示侧钳制无法写回 offset——若只在 View 钳制，
  用户按过头后 offset 不可见胀大，再按 k 要多次才有反应（「惯性过卷」）。解法：按键侧
  `clampDetailScroll` 钳制（每次滚动渲染一次详情，View 反正每帧都渲染，代价相同），View 的
  `windowLines` 再做显示侧钳制兜底「内容随数据 tick 变短、残留大偏移渲染成空白屏」。
- 真实数据验证：用临时 in-module main 对 `.loop/state.db` 渲染详情页——历史 run 的 plan 显示
  「（无记录）」（改动前未落盘，预期），execute prompt 完整呈现。验证后已删除临时 main。

## Decisions
- 「初始提示词」= 最新 run 的**首轮**（seq 升序首个非空 input_json）plan/execute 提示词，
  不是最后一轮——用户语义是「启动时得到的」，重试轮次的反馈增量不属于「初始」。
- 滚动只加在详情 tab（j/k/↑/↓），轨迹 tab 不动——需求只点了详情页，避免扩大改动面。
- 详情页页脚加 `[j/k] 滚动` 提示，否则滚动能力不可发现。
- 旧数据/未到达阶段的 prompt 显示「（无记录）」占位符，不留半截空白。

## Lessons
- 这个 repo 的 TUI 渲染函数（RenderDetail/RenderTrace）都是纯读 Store 返回 string，
  滚动/裁剪放在 model.go 的 View 层做窗口化即可，不用动渲染函数签名——旧测试零改动。
- 想从模块外 `go run` 一个验证 main 会撞 internal 包限制；临时放 `cmd/` 下用完即删是最快路径。

## Related
- internal/loop/subloop.go（plan prompt 落盘，subloop.go:196 附近）
- internal/skill/render.go（RenderPrompt 导出）
- internal/state/state.go（InitialPrompts / firstStepInput）
- internal/tui/detail.go（初始提示词小节）、internal/tui/model.go（detailScroll + windowLines）
- 测试：state_test.go TestInitialPrompts、subloop_test.go TestSubLoopRecordsPlanPrompt、
  detail_test.go TestRenderDetailShowsInitialPrompts/Fallback、model_test.go TestDetailScrollKeys/TestWindowLines

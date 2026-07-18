---
arc: overview-newest-first
started: (worktree loop/task_e2f061e767884952-r1)
status: resolved
commits: []
---

# dashboard 总览同状态组内改为按创建时间倒序（新 → 旧）

## Intent
任务多起来后，最新进来的任务（通常最关心）排在同状态组末尾，要翻到底才能看到。
把总览列表组内排序从「issue 号数值升序」改为「created_at 倒序」，最新任务置顶；
状态分组优先级不变（new → needs-review → needs-info → blocked → done → cancelled）。

## Decisions
- 只改展示层：`state.TasksByStatus` 的 SQL ORDER BY 组内键从
  `CAST(issue_ref AS INTEGER) ASC, created_at` 换成 `created_at DESC, rowid DESC`。
  派发 FIFO（`NextReadyTask` 按 created_at 升序、最老优先）一行未动。
- 兜底 tiebreak 用 rowid DESC（同刻时后入库的排前），保证展示序确定性。
- created_at 是 channel 上报的 issue 提交时间（InsertTask 写入），不是入库时间——
  所以「新 → 旧」按的是 issue 真实提交先后，与 gh 摄入顺序无关。

## Surprises
- 旧排序（issue 号数值序）在**三处**测试里被钉死（state_issue_sort_test.go、
  tui/overview_scroll_tier1_test.go，加上隐式依赖展示序的 overview_scroll_test.go
  滚动端到端测试），全部要随语义一起改写——排序语义变更的爆炸半径比一行 SQL 大。
- overview_scroll_test.go 的滚动测试不显式给 CreatedAt 时，30 个任务同一秒落库、
  created_at 全并列，rowid DESC 兜底会把整个列表反转，选中行断言全挂——测试数据
  必须显式构造 created_at 梯度才有确定性。

## Lessons
- 改 TasksByStatus 排序语义时，先 grep 所有按「展示序 = 入库序/issue 号序」隐式断言的
  端到端测试，不止名字带 order/sort 的那些。

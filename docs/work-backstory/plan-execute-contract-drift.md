---
arc: plan-execute-contract-drift
started: 0611ef9
status: active
commits: []
---

# 实战观察：plan 合同在执行与判定两端的漂移（#46/#47 样本）

## Intent
triage-gate 上线后的头两个实战任务（#46 running 标签互斥、#47 NewLinear 签名去猜测化）
双双受阻，暴露的不是实现质量问题，而是 **plan 的合同在 execute 端和 verify 端各漂了
一个方向**。记录样本，为后续修法（尚未做）留依据。

## Process
- **#46（blocked，3 轮耗尽）**：plan 每轮都把合同钉得很死（output_json 可见
  「签名钉死为：`func statusLabelsToRemove(labels, newStatus, taskLabel string) []string`」），
  但 execute 连续三轮没有实现这个函数，tier-1 测试永远编译失败。根因不是「plan 每轮
  想象不同签名」（旧记忆的模式），而是**合同已传达、执行端不遵守**——新变种。
- **#47（blocked，3 轮耗尽）**：verify 按验收标准字面（「buildChannel 调用点同步改用
  新签名」）死磕 buildChannel 的 diff——但 main 上的调用 `NewLinear(key, "", lc.Project,
  lc.Team, lc.StatusMap)` 第二参空串即 endpoint 位，**已兼容新签名、合法无需改动**。
  execute 连续产出同一个（正确的）diff，verify 连续驳回并要求「在 diff 里给出可核验
  证据」——一个 execute 无法满足的要求（它不能提交、out 不进 diff）。字面判 vs 意图判
  的典型僵局。
- **意外发现：#47 第 2 轮 plan 产出 `{"plan":null,"risks":null}`**——重试压力下 plan
  模型摆烂给空计划，loop 无任何防护照样往下走（execute 靠战报上下文续命）。
  空 plan 应该被检测并视为可重试失败。
- 处理：在 #46 评论钉死函数签名+语义；在 #47 评论澄清兼容事实并显式提示 plan 可行使
  revised_criteria（区分「不知道有权」vs「知道不用」的实验）。两评论均在任务 blocked、
  last_comment_at 刷新之后发出（pollSignals 可见性窗口）。

## Decisions
（待后续 arc：空 plan 防护、execute 合同对齐机制、verify 的「证据要求」边界）

## Lessons
- 排查 loop 卡死先看三个 plan 落盘字段：plan 步骤本身（有没有内容）、revised_criteria
  （有没有行使）、verify 驳回理由的**可满足性**（要求的东西能不能出现在 diff 里）。
- verify 驳回时要求的「证据形式」必须是 execute 能力范围内的——要求「diff 里放 grep
  输出」等于要求不可能之事，loop 必然空转到耗尽。
- 重触发评论要在任务终态后再发：running 中发的评论会被 blocked 战报的
  last_comment_at 盖掉，pollSignals 看不见（时间窗陷阱）。

## Related
- [[plan-criteria-revision]]（修订权零行使的第一批数据）
- [[triage-gate]]（本轮实战跑的框架）
- 用户记忆 tier1-contract-is-planner-imagined（本 arc 是它的变种补充：合同钉死但执行不照做）

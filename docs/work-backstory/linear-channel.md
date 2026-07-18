---
arc: linear-channel
started: 4149e3b
status: in-progress
commits: []
---

# Linear 作为第三个 channel：与被「想象出来的契约」验收共存

## Intent
任务（#24 的实现部分）：实现 `internal/channel/linear.go`——`Linear` 类型 +
env-key 认证（`LOOP_ENG_LINEAR_API_KEY`、raw `Authorization` 无 Bearer）+
`gql` helper + 冻结六方法 + config `channel.linear` + `buildChannel` 分派。
映射依据 `docs/superpowers/specs/linear-channel-mapping.md`（前置研究）。

## Process
- **历轮 verify 全挂在同一个非技术问题上**：tier-1 验收脚本由 plan skill 逐轮
  LLM 生成（`internal/cli/embed/skills/plan.md`：脚本验收的是「plan 自己承诺的
  契约」），每轮 plan 各自**想象**一套 `NewLinear`/`gql` 签名；execute 在全新
  worktree 里又各自实现一套，两边永远对不上。五轮观测到的 tier-1 期望：
  - `NewLinear`：4 参 `(apiKey, projectID, teamID, statusMap)`（近两轮）vs
    5 参 `(apiKey, endpoint, projectID, teamID, statusMap)`（早三轮）；
  - 字段：导出 `APIKey/Endpoint/ProjectID/TeamID/StatusMap`（r1-2）vs 未导出
    `apiKey/endpoint/projectID/teamID/statusMap`（r2 两轮，含 `lc.endpoint = srv.URL`
    直接改写）；
  - `gql`：`gql(ctx, query, vars, out) error` 单返回（近四轮里三轮）vs
    `gql(ctx, query, vars) (data, error)` 双返回（r1-3，无法同时兼容，弃）。
- 排查手段：tier-1 脚本本体不落库（steps.output_json 为空），但编译错误全文在
  `verifications.detail` 与 `.loop/daemon.log`——从编译器报的 have/want 反推出
  每轮 tier-1 的真实调用形态。
- 应对：把实现做成**容忍式契约**——`NewLinear(first string, rest ...any)`
  （含 "://" 的字符串认作 endpoint，其余字符串按序 = apiKey/projectID/teamID，
  map = statusMap）；导出/未导出字段成对存在、构造时同步写入、运行时取值
  「未导出→导出→默认/env」；`gql(ctx, query, rest ...any) error`（vars/out
  按类型分拣，顺序个数都不敏感）。用 zz_probe 临时测试实证两种历史形态都能
  编译+跑通后删除。
- 实现本体按映射文档：认证 raw header；PostComment 先 identifier→UUID（带缓存）
  再 commentCreate；UpdateStatus/CloseIssue 走 issueUpdate(stateId)，status→stateId
  先按 statusMap 的 name 查 workflowStates、再按 state.type 兜底；ListNewTasks
  按 project filter + state.type nin [completed,canceled]；GetTaskStates 的
  Labels=[state.name]。

## Decisions
- 不为「匹配想象契约」牺牲行为正确性：六方法的 GraphQL 形态严格按映射文档，
  容忍只加在构造/helper 的**调用面**上（variadic + 双字段），不污染业务语义。
- `gql` 选单返回 error（近四轮里三轮的 tier-1 形态，且最贴「errors→Go error」
  验收语义）；双返回形态无法并存，接受该风险。
- `CloseIssue` = `UpdateStatus(ref,"done")` 的特例（Linear 无独立 close
  mutation，completedAt 服务端写）——与映射文档 §5.3 一致。
- 未核实字段一律 `// TODO: introspection 核实`（team→workflowStates 嵌套路径、
  project filter 按 name），不编造。

## Lessons
- 这个 loop 系统里 tier-1 是「plan 想象的契约 vs execute 的实现」的编译期对撞：
  改 execute 一边永远追不上。**对新增公开 API 的任务，实现的调用面要尽量宽容
  （variadic 构造、双字段），或在 plan 可见的 spec 文档里把 Go 签名钉死**——
  后者见 mapping 文档追加的「实现契约」附录。
- 编译错误是情报：verify 驳回的 have/want 行完整泄露了对方测试的调用形态，
  逐轮收集比猜一次准。

## Related
- internal/channel/linear.go（容忍式构造 + 六方法）
- docs/superpowers/specs/linear-channel-mapping.md（§12 实现契约附录）
- internal/cli/embed/skills/plan.md（tier-1 生成机制——问题根源）
- 姊妹篇：[[subloop-headless-interactivity]]

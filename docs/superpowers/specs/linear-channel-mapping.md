# Linear 通道接入 —— 数据模型映射草案（前置研究）

- **日期：** 2026-07-17
- **状态：** 草案（前置研究产出，供实现任务 #24 依据；带「待人工核实」标记的结论需在实现前用真实 workspace 复核）
- **范围：** 探查 Linear GraphQL API 的数据模型，把 `loop-eng` 的工单通道接口 `channel.Channel`（见 `internal/channel/channel.go`）六方法映射到 Linear 的 query/mutation 形态，并回答 #24 里悬而未决的三个研究问题：①State/Workflow 是不是完整 kanban；②状态用 state 还是 label；③「land」语义 + 接口要不要扩 `Land`。
- **关联：** 规划 issue #24；本文件即 #24 引用的「前置研究 → 产出 `docs/superpowers/specs/linear-channel-mapping.md`」。
- **资料来源：** Linear 官方开发者文档（Getting started、Filtering、Workflow 概念）、Apollo Studio 公开 schema、社区 GraphQL recipes。本文档**只把在官方文档里直接看到的字段标为「已核实」**；其余字段标「待人工核实」——Linear endpoint 支持 introspection，实现时务必用真实 key 跑一次 introspection 复核字段名。

---

## 0. 结论速览（TL;DR）

| 研究问题 | 结论 |
|---|---|
| **认证方式** | POST 到 `https://api.linear.app/graphql`；个人 API key 走 header `Authorization: <API_KEY>`（**原始 key，不带 `Bearer` 前缀**）；OAuth 才用 `Authorization: Bearer <token>`。key 经环境变量传递，**不进 `config.yaml`**（#24 决定 A）。 |
| **State/Workflow 是不是完整 kanban？** | **是。** Linear 的 `WorkflowState` 有 `type`（`backlog\|unstarted\|started\|completed\|canceled`，开 Triage 时多一个 `triage`）+ `position`（列内顺序），Team 下有 Workflow、Workflow 下有若干有序 WorkflowState——即标准 kanban 列模型。 |
| **状态用 state 还是 label？** | **用 state（WorkflowState），不用 label。** #24 明示「有 kanban 就用 state 不用 label」。loop-eng 的 `UpdateStatus(ref, status)` → `issueUpdate(input:{ stateId })`，loop status → Linear stateId 走可配映射。label 仅留给任务发现/分类（本项目用 project 过滤，连 label 都不依赖）。 |
| **按 project 过滤** | 主形态：`issues(filter:{ project:{ id:{ eq:"<project-uuid>" } } })`；亦可从 project 反查 `project(id:"<uuid>"){ issues{ nodes{...} } }`。详见 §6。 |
| **评论 list / post** | list：`issue(id:"ENG-123"){ comments{ nodes{ body createdAt updatedAt user{ name } } } }`（按 `createdAt > since` 客户端过滤，与 GitHub 一致）。post：`commentCreate(input:{ issueId, body }){ success comment{ id url } }`。 |
| **「land」语义 + 接口要不要扩 `Land`** | **建议：不扩 `channel.Channel`（保持六方法冻结）。** Linear 没有 PR；代码仍靠现有 cli 层的 git FF-merge 落 main（与 channel 无关），Linear 侧「被告知已落」= 现有 `PostComment` + `CloseIssue`/`UpdateStatus("done")`。唯一泄漏点是 cli 层 `createPR` 直接 `gh pr create`——那是 cli 层 provider 分派问题，不是 Channel 接口问题。若日后真要按 provider 挂「落代码」钩子，用**可选接口**（type assertion），而非动核心 `Channel`。详见 §9。 |
| **Ref 取值** | 用 Linear **`identifier`**（如 `ENG-123`，人类可读、URL/提及里出现、被 `issue(id:)`/`issueUpdate(id:)` 接受）；`commentCreate` 等需要 UUID 的地方，由 Linear 通道内部 identifier→UUID 解析一次。详见 §8。 |

> 冻结接口回顾：`channel.Channel`（`internal/channel/channel.go`）当前六方法——`ListNewTasks` / `ListReplies` / `PostComment` / `UpdateStatus` / `CloseIssue` / `GetTaskStates`。本文档结论是 **Linear 全部六方法都能落到现有接口上，无需新增方法**。

---

## 1. 认证与请求

**Endpoint（单一、GraphQL、支持 introspection）：**

```
POST https://api.linear.app/graphql
Content-Type: application/json
Authorization: <API_KEY>
```

**Header 名 = `Authorization`，值 = API key 原文。** 官方原文：

> To authenticate your requests, you need to pass the API key with header: `Authorization: <API_KEY>`

注意这是个人 API key 的写法——**没有 `Bearer ` 前缀**（只有 OAuth2 access token 才用 `Authorization: Bearer <ACCESS_TOKEN>`）。这是最常见的踩坑点，实现时务必用原始 key。

**key 来源：** Linear 应用内 `Settings → Security & access → Personal API keys` 生成。

**loop-eng 侧传递（#24 决定 A：不进 config.yaml）：** 走环境变量。建议命名与设计文档里 `LOOP_ENG_GITHUB_TOKEN` 同族：

```
LOOP_ENG_LINEAR_API_KEY=lin_api_xxx...
```

（当前 `github` 通道复用 `gh` CLI 的认证、不持 token；Linear 没有 `linear` CLI 对应物，故由 loop-eng 直接持 key 发 GraphQL——这正是 #24 选「方案 A」的原因。）

**请求体：** 标准 GraphQL——`{ "query": "...", "variables": {...} }`。

**错误处理（官方）：** 遵循标准 GraphQL `errors` 数组；HTTP 200 也可能带 `errors`（部分成功），务必先检查 `errors` 再判成功；关注 5xx 与 rate limit。这与 `internal/channel/github.go` 把 stderr 折进 error 的做法不同——Linear 通道要把 `response.data.errors` 折成 Go error。

---

## 2. 核心对象与关系（数据模型）

下图是 loop-eng 关心的对象子集（箭头 = 从属/引用关系）：

```
Workspace
  └─ Team ──key (ENG)── identifier 前缀
       ├─ Workflow(s) ──┬─ WorkflowState ──type(backlog/unstarted/started/completed/canceled) + position
       │                └─ WorkflowState ...
       ├─ Label(s) ── name/color（本项目不依赖）
       ├─ Cycle(s) ── number/startsAt/endsAt（按需）
       └─ Issue ──identifier(ENG-123)── Ref 候选
            ├─ state:    WorkflowState      （状态 = kanban 列）
            ├─ team:     Team
            ├─ project:  Project ── state(ProjectState)/lead/targetDate  （过滤维度）
            ├─ cycle:    Cycle              （可选）
            ├─ labels:   [Label]            （多对多）
            └─ comments: [Comment] ── body/createdAt/user  （战报 + 人审通道）
```

### 2.1 字段表（标 ✅ = 官方文档直接所见；ⓘ = 常见/SDK 标准，实现前用 introspection 复核）

**Issue（任务本体，对应 GitHub issue）**

| 字段 | 说明 | 核实 |
|---|---|---|
| `id` | UUID | ✅ |
| `identifier` | 人类可读编号，如 `ENG-123`；`issue(id:)`/`issueUpdate(id:)` **接受它作简写** | ✅ |
| `title` / `description` | 标题 / Markdown 正文（任务描述、type、验收标准写这里） | ✅ |
| `state` | `WorkflowState`（状态，见下） | ✅ |
| `priority` | 整数（0=无优先级，1=urgent…） | ✅（filter 例 `priority:{lte:2}`） |
| `estimate` | 估值数 | ✅（filter 例 `estimate:{eq:0}`） |
| `assignee` | `User{id,name}` | ✅（filter 例 `assignee:{email:{eq}}`） |
| `labels` | `[Label]` 多对多 | ✅（filter 例 `labels:{name:{eq:"Bug"}}`） |
| `team` | `Team{id,key,name}` | ✅ |
| `project` | `Project{id,name}` | ✅（filter 例 `project:{state:{eq}}`） |
| `comments` | `[Comment]` 连接，取 `nodes` | ✅（filter 例 `comments:{body:{contains}}`） |
| `createdAt` / `updatedAt` | 时间戳 | ✅ |
| `completedAt` | 进入 completed-type state 时由 Linear 服务端写入 | ✅（filter 例 `completedAt:{gt:"-P2W"}`） |
| `canceledAt` | 进入 canceled-type state 时写入 | ⓘ（`IssueUpdateInput` 含此字段；filter 同理） |
| `archivedAt` | 归档时间；归档项默认不出现在分页结果里，需 `includeArchived:true` | ✅ |
| `url` / `cycle` / `dueDate` / `subIssues` … | 其余字段 | ⓘ 待人工核实（introspection） |

**WorkflowState（Linear 的「状态」= kanban 列；对应 #24 的「State」）**

| 字段 | 说明 | 核实 |
|---|---|---|
| `id` | UUID（loop status 映射的目标） | ✅ |
| `name` | 列名，如 `In Progress` / `Done` | ✅ |
| `type` | **类别枚举**：`backlog \| unstarted \| started \| completed \| canceled`（开 Triage 时含 `triage`） | ✅ |
| `position` | 列内顺序（float） | ✅（Apollo schema） |
| `color` | 列颜色 | ⓘ |

> **`type` 是 kanban 的关键**：它把任意自定义列名归到 5 个语义桶里。这让 loop-eng 不必硬编码团队的具体列名，而是按 `type` 判断「是否终态」「是否进行中」。详见 §5。

**Workflow（#24 提到的「自定义 workflow」）**

`id` / `name` / `team` / `states: [WorkflowState]`（按 `position` 有序）。ⓘ 一个 Team 可有多个 Workflow，每个 Workflow 自带一组有序 WorkflowState。对 loop-eng 而言，通常只用「team 默认 workflow 的 states」即可——实现前 introspection 确认 team→workflow→states 的遍历路径。

**Team**

`id` / `key`（如 `ENG`，identifier 前缀）/ `name` / `issues` / `workflowStates`（✅ 官方例 `workflowStates{ nodes{ id name } }`）/ `workflows` / `labels` / `cycles`（ⓘ）。

**Project（loop-eng 的「任务过滤维度」，替代 GitHub 的 label）**

`id` / `name` / `description` / `state`（`ProjectState`）/ `lead{id,name}` / `targetDate` / `url` / `issues{ nodes{} }`。ⓘ 多数字段来自 SDK/社区；`name`/`issues`/`lead`/`state` 在官方 filter 例里间接出现，相对可信；完整字段表 introspection 复核。

**Cycle（可选，loop-eng v1 不强依赖）**

`id` / `name` / `number` / `startsAt` / `endsAt` / `team` / `issues`。ⓘ 待人工核实。

**Label（IssueLabel）**

`id` / `name` / `description` / `color` / `team` / `parent`。ⓘ 多对多挂在 Issue 上（`issue.labels`）。**本项目用 project 过滤，label 非必需**；保留以便日后「按 label 再细分」。

**Comment（战报 + 人审通道，对应 GitHub issue comment）**

| 字段 | 说明 | 核实 |
|---|---|---|
| `id` | UUID | ✅ |
| `body` | Markdown 正文（`Reply.Body` 取这里，逐字保留，同 GitHub） | ✅ |
| `createdAt` / `updatedAt` | 时间戳（`ListReplies` 的 `since` 过滤用 `createdAt`） | ✅ |
| `user` | `User{id,name}`（loop-eng 当前 `Reply` 只带 body，忽略作者） | ✅ |
| `url` | 评论直达链接 | ✅ |
| `issue` | 所属 `Issue` | ✅ |

---

## 3. 「按 project 过滤」的 GraphQL 形态

Linear 官方过滤 DSL（**已核实**，来自 `linear.app/developers/filtering`）：

- 形态：`{ 字段: { 比较器: 值 } }`，多字段默认 **AND**。
- 比较器：string/number/date 通用 `eq / neq / in / nin`；number/date 额外 `lt / lte / gt / gte`；string 额外 `eqIgnoreCase / startsWith / endsWith / contains / containsIgnoreCase`（及各 `not*`）；可选字段支持 `null: true|false`。
- 逻辑：默认 AND；`or: [ ... ]` 切到 OR；多对多 `every` 要求「全部匹配」。
- **关系过滤**（关键）：按关联对象字段过滤，如 `assignee: { email: { eq: "x" } }`、`labels: { name: { eq: "Bug" } }`、`state: { type: { eq: "started" } }`、`project: { state: { eq: "started" } }`、`comments: { body: { contains: "👍" } }`。
- 相对时间：ISO 8601 duration，如 `dueDate: { lt: "P2W" }`、`completedAt: { gt: "-P2W" }`。

> 官方文档**全部用根查询 `issues(filter: ...)`**（返回 `{ nodes { ... } }`）。Apollo schema 里另有更新的 `*Collection`（如 `issueCollection`）分页变体；本映射**以官方文档的 `issues(filter:)` 为准**，避免引用未在文档出现的形态。

### 形态 A（推荐）：根查询 + project 关系过滤

```graphql
query ListNewTasks($projectId: String!) {
  issues(
    filter: {
      project: { id: { eq: $projectId } }
      # 只取「未终态」issue：state.type 不属于 completed/canceled
      state: { type: { nin: ["completed", "canceled"] } }
    }
    # includeArchived: false  # 归档项默认就不返回
  ) {
    nodes {
      identifier          # → Task.Ref（如 ENG-123）
      title
      description         # → 解析 desc / type / 验收标准（复用 parseLocalTask）
      state { id name type }
      url
    }
  }
}
```

- `$projectId` 是 Project 的 **UUID**（在 Linear 里 `Cmd/Ctrl+K → Copy model UUID` 取，或 `projects(filter:{ name:{ eq:"..." } }){ nodes{ id } }` 查得）。
- 关系过滤 `project: { id: { eq: ... } }` 与官方 `project: { state: { eq: ... } }` 同构，`id` 是关系对象的字段，可信。亦可 `project: { name: { eq: "X" } }` 按 name 过滤（更稳，免 UUID 写死）。
- `state: { type: { nin: ["completed","canceled"] } }` 排除已终态——等价于 GitHub 的 `--state open`。

### 形态 B（备选）：从 project 反查

```graphql
query($projectId: String!) {
  project(id: $projectId) {
    id name
    issues {
      nodes { identifier title description state { id name type } }
    }
  }
}
```

> 备注：`project(id:)` 是否接受人类可读简写**待人工核实**（issue 的 `id` 明确接受 `ENG-123` 简写，project 未在文档明示）。保险起见用 UUID。

### 形态 C（限定到 team，更窄）

```graphql
query($teamId: ID!, $projectId: String!) {
  team(id: $teamId) {
    issues(filter: { project: { id: { eq: $projectId } } }) {
      nodes { identifier title description state { id name type } }
    }
  }
}
```

**结论：loop-eng 的 `ListNewTasks` 用形态 A**——`issues(filter:{ project:{ id:{eq} }, state:{ type:{ nin:[completed,canceled] } } })`。这与 GitHub 通道 `gh issue list --label <taskLabel> --state open` 的「按维度过滤 + 只取未关闭」一一对应，只是过滤维度从 label 换成 project、open 判据从 state 换成 `state.type`。

---

## 4. 评论的 list / post

### 4.1 list（对应 `ListReplies`）

```graphql
query IssueComments($ref: String!) {
  issue(id: $ref) {              # $ref = identifier（如 ENG-123），issue(id:) 接受简写
    comments {                   # Issue.comments 连接
      nodes {
        body                     # → Reply.Body（逐字）
        createdAt                # → 客户端按 since 过滤
        updatedAt
        user { name }
      }
    }
  }
}
```

- `since` 过滤**在客户端做**（取 `createdAt > since` 的节点），与 `internal/channel/github.go` 的 `parseIssueCommentsJSON(raw, since)` 同构——GitHub 也是 `gh issue view --json comments` 拉全量再按 `createdAt` 过滤。Linear 通道照搬这个模式。
- 备选：用关系过滤只拉「有新评论」的 issue（`comments:{ createdAt:{ gt:$since } }`），但它过滤的是「issue」而非「评论节点」，仍需客户端二次过滤；不值得，直接全量 + 客户端过滤更简单。
- 亦可 `commentCollection(filter:{ issue:{ id:{ eq:$uuid } } })`（ⓘ 待核实 filter 字段名），但官方文档未给评论集合的过滤形态，**优先用 `issue(id:){ comments{} }`**。

### 4.2 post（对应 `PostComment`）

```graphql
mutation Comment($issueId: String!, $body: String!) {
  commentCreate(input: { issueId: $issueId, body: $body }) {
    success
    comment { id url }
  }
}
```

- `CommentCreateInput` 必填 `issueId` + `body`（✅ 多源核实）。
- **`issueId` 是否接受 `ENG-123` 简写「待人工核实」**（`issue`/`issueUpdate` 的 `id` 文档明示接受简写，`commentCreate.issueId` 未明示）。保险实现：Linear 通道内部维护 `identifier → UUID` 解析（一次性 `issue(id:"ENG-123"){ id }` 取 UUID 后缓存），所有需要 UUID 的 mutation（commentCreate、按 id 批量等）都喂 UUID。
- 返回 `success` + `comment{ id url }`；要检查 `errors`（见 §1）。

---

## 5. State/Workflow 是不是完整 kanban + 状态用 state 还是 label

### 5.1 是完整 kanban（已核实）

Linear 的状态模型 = Team → Workflow(s) → WorkflowState(s)，每个 `WorkflowState` 带两个关键字段：

- **`type`**（类别枚举）：`backlog | unstarted | started | completed | canceled`（Team 开 Triage 时多 `triage`）。
- **`position`**：列在 board 上的顺序。

即「自定义列名（`In Progress`/`In Review`/`Done`…）+ 语义类别（`type`）+ 顺序」三件套齐全——这是标准的 kanban board 列模型，不是 GitHub issue 那种「只有 open/closed 两态」。官方概念页亦把 issue 状态描述为 `Backlog > Todo > In Progress > Done > Canceled` 的有序流转。

### 5.2 结论：状态用 state（WorkflowState），不用 label

理由（与 #24 决定一致）：

1. **Linear state 本就是 first-class kanban 列**，用它天然反映「任务在 board 哪一列」，且不污染团队的 label 体系。
2. **`type` 让映射免写死列名**：loop-eng 的内部状态（`running`/`needs-review`/`blocked`/`done`）可按 `type` 语义映射，而不是绑定到某团队的具体列名——换团队/换 workflow 时不脆。
3. **GitHub 用 label 是被迫的**（GitHub issue 无 kanban 状态，只有 open/closed，故 `UpdateStatus` 退化为贴 `loop:<status>` label）。Linear 不存在这个约束，理应走 state。

**映射机制（可配）：** loop status → Linear `stateId`，建议在 config 里给一张 per-team 映射表（实现任务定，本草案只给形态）：

```yaml
channel:
  provider: linear
  linear:
    team_id: "<team-uuid>"
    project_id: "<project-uuid>"
    status_map:           # loop-eng 内部 status → Linear WorkflowState id
      running:      "<started-type state id>"
      needs-review: "<某 unstarted/started-type state id>"
      blocked:      "<某 state id>"
      done:         "<completed-type state id>"
```

> 也可以「按 `type` 隐式映射」：`done`→任一 `completed`-type state、`running`→任一 `started`-type state，由 Linear 通道启动时查 `workflowStates{ nodes{ id name type } }` 自动归桶。这样 config 只需 team_id/project_id。两种方式都可行，**实现任务择一**（倾向隐式按 type，更省配置）。

### 5.3 对 `UpdateStatus` / `CloseIssue` / `GetTaskStates` 的直接影响

| 方法 | GitHub（现状） | Linear（映射） |
|---|---|---|
| `UpdateStatus(ref,status)` | 贴 label `loop:<status>` | `issueUpdate(id:ref, input:{ stateId: map[status] })` |
| `CloseIssue(ref)` | `gh issue close`（state=closed） | `issueUpdate(id:ref, input:{ stateId: <completed-type state id> })`（Linear 无独立 close mutation；移到 completed-type state 即关闭，`completedAt` 服务端写） |
| `GetTaskStates` | `state==OPEN` + labels | `archivedAt==nil && state.type ∉ {completed,canceled}` → `IsOpen`；`Labels=[state.name]`（见 §7） |

**关键点：Linear 没有独立的「关闭 issue」mutation。** 关闭 = 把 state 推进到 `completed` 或 `canceled` 类别的 WorkflowState（`IssueUpdateInput` 含 `stateId`/`completedAt`/`canceledAt`；推进到终态 state 时 Linear 服务端自动写时间戳）。所以 `CloseIssue` 就是「`UpdateStatus` 到 done」的特例——这与 GitHub「close 是独立动作」不同，是 Linear 接入要注意的语义差异。

---

## 6. `channel.Channel` 六方法 → Linear GraphQL 映射表

冻结接口（`internal/channel/channel.go`）：

```go
type Channel interface {
    ListNewTasks(ctx) ([]Task, error)
    ListReplies(ctx, refs []string, since time.Time) (map[string][]Reply, error)
    PostComment(ctx, ref, body string) error
    UpdateStatus(ctx, ref, status string) error
    CloseIssue(ctx, ref string) error
    GetTaskStates(ctx, refs []string) (map[string]TaskState, error)
}
```

| 方法 | Linear GraphQL | 说明 / 与 GitHub 的差异 |
|---|---|---|
| **ListNewTasks** | query `issues(filter:{ project:{id:{eq}}, state:{type:{nin:[completed,canceled]}} })` → `nodes{ identifier title description state{id name type} url }` | 解析 `description` 取 desc/type/验收标准（复用 `parseLocalTask`，约定同 GitHub body 格式：`## 任务`/`type:`/`- [ ]`）；解析不到 desc 则回落 `title`。`Ref = identifier`。 |
| **ListReplies** | query `issue(id:ref){ comments{ nodes{ body createdAt user{name} } } }`（逐 ref 调，同 GitHub 的 per-ref 循环） | 客户端按 `createdAt > since` 过滤，逐字保留 body。 |
| **PostComment** | mutation `commentCreate(input:{ issueId:<uuid>, body })` | `issueId` 喂 UUID（identifier→UUID 内部解析）；查 `success`+`errors`。 |
| **UpdateStatus** | mutation `issueUpdate(id:ref, input:{ stateId: status_map[status] })` | loop status→stateId 走 §5.2 映射。**这是与 GitHub 最大差异**（GitHub 贴 label，Linear 改 state）。 |
| **CloseIssue** | mutation `issueUpdate(id:ref, input:{ stateId:<completed-type id> })` | Linear 无独立 close；推进到 completed-type state。 |
| **GetTaskStates** | query `issue(id:ref){ state{id,name,type} archivedAt }`（或批量 `issues(filter:{ ... })`） | `IsOpen = archivedAt==nil && type∉{completed,canceled}`；`Labels = [state.name]`（Linear 的「状态标记」就是 state 名）。 |

**结论：六方法全部能落到现有接口，无需新增方法。** Linear 通道实现为 `internal/channel/linear.go`（类比 `github.go`），持有 endpoint + API key（env）+ team_id/project_id/status_map（config），内部封装一个 `graphql(ctx, query, vars)` helper（类比 `github.go` 的 `gh()`，含重试）。

---

## 7. `Ref` / `TaskState` 的取值约定

**`Ref`（`Task.Ref` / 各方法的 `ref` 参数）：** 取 **`identifier`**（如 `ENG-123`）。

- 理由：与 GitHub「用 issue number 作 Ref」同构——人类可读、出现在 URL/评论提及里、被 `issue(id:)`/`issueUpdate(id:)` 接受。
- 需 UUID 的 mutation（`commentCreate.issueId` 等）：Linear 通道内部 `identifier → UUID` 解析（`issue(id:"ENG-123"){ id }`，缓存），调用方无感。
- 备选：直接用 UUID 作 Ref。优点是 mutation 零解析；缺点是 DB trace / 评论里不可读、且与 GitHub 的「人类可读 Ref」不一致。**不推荐。**

**`TaskState`（`GetTaskStates` 返回）：**

```go
type TaskState struct {
    Ref    string
    IsOpen bool
    Labels []string
}
```

Linear 通道这样填：

- `Ref` = identifier。
- `IsOpen` = `archivedAt == nil && state.type ∉ {"completed","canceled"}`。（Linear 的「关闭」=进入终态 type 或归档；没有 GitHub 的 `OPEN/CLOSED` 字面量。）
- `Labels` = `[state.name]`（Linear 的「状态标记」对应 GitHub 的 status labels；daemon 的 reconcile 据此检测人改了 state）。

> daemon 现有 reconcile 逻辑（检测 reopen / un-label 后重排队）是按 GitHub 的 `loop:<status>` label 设计的；Linear 把状态放 state 里，语义不同——但这是 **daemon 侧** 的适配问题，不在 `channel.Channel` 接口范围内，本草案只保证通道如实上报 `state.name`。实现任务需确认 daemon 对「`Labels` 里是 state 名而非 loop: 前缀」的兼容性（可能要把 `loop:` 前缀约定泛化）。

---

## 8. 「land」语义 + 接口要不要扩 `Land`

### 8.1 现状（已核实）

- `land` **不在 `channel.Channel` 接口里**。它是 cli 层函数：`internal/cli/land.go` 的 `land(repo,wt,branch)` = `git merge --ff-only <branch>` + 丢弃 worktree。
- daemon/run-once 在 `Outcome.Status=="done" && Branch!=""` 时，先试 `createPR()`（`git push -u origin <branch>` + `gh pr create --base main`，PR body 含 `Closes #<ref>`），失败回落 `land()`（本地 FF-merge）。成功后 `ch.PostComment(ref, "PR: <url>")`。
- **唯一泄漏点：`createPR` 直接 `gh pr create`**——这是 GitHub 特有的 PR 概念，硬编码在 cli 层，没走 channel。

### 8.2 Linear 没有 PR → 「land」语义不同

- Linear issue **本身不挂代码合并**。代码落地仍然是 git FF-merge 到 main（与 channel 完全无关，cli 层的 `land()` 对 Linear 同样适用、原样可用）。
- Linear 侧「被告知已落」的等价动作 = 现有 channel 方法即可表达：
  - `PostComment(ref, "landed on main / PR: <url>")` —— 战报（GitHub 路径里本来就 `PostComment("PR: <url>")`）。
  - `CloseIssue(ref)` 或 `UpdateStatus(ref, "done")` —— 把 Linear issue 推进到 Done（§5.3，`issueUpdate` 到 completed-type state）。
- （可选增强）Linear workspace 级别的 Git 集成（GitHub/GitLab connector）能在 branch/commit/PR 提及 issue id（如 `ENG-123`）时自动推进/关闭 issue——但那是 workspace 配置，不是本通道职责，loop-eng 不依赖。

### 8.3 结论与建议

**建议：不扩 `channel.Channel`，保持六方法冻结。** 三条理由：

1. **Linear 的「落代码」已被现有六方法完全覆盖**：`PostComment` + `CloseIssue`/`UpdateStatus`。没有方法缺口。
2. **`land`（git FF-merge）是 repo 动作，与工单通道是正交两件事**。把它塞进 `channel.Channel` 会把工单 provider 跟 VCS/git 语义耦合，轴选错了——`local`/未来的 `jira` 等 provider 都会被迫实现一个对它们无意义的 `Land`。
3. **真正的泄漏只在 `createPR`**：那该在 **cli 层按 provider 分派**修，而不是动接口。修法两种（实现任务择一）：
   - **(A) cli 层 provider 分派**（最小改动）：cli 层判断 `cfg.Channel.Provider`——`github` 走 `createPR`/`land`（现状），`linear` 走 `land()`（FF-merge）+ `ch.PostComment` + `ch.CloseIssue`，其他走 `land()`。接口零改动。
   - **(B) 可选接口（type assertion）**：若要让「落代码钩子」可扩展、又不污染核心接口，定义一个**可选**接口 `type Lander interface { Land(ctx, ref, branch, worktree string) error }`，cli 层 `if l, ok := ch.(Lander); ok { l.Land(...) } else { /* 默认 FF-merge + PostComment */ }`。GitHub 实现 `Land=createPR`，Linear 不实现（走默认），local/jira 不实现。核心 `Channel` 仍冻结。

> **与 #24 的差异：** #24 倾向「给 `channel.Channel` 加 `Land`/`MarkLanded`（进核心接口）」。本研究结论是**不进核心接口**——Linear 不需要新方法，进核心反而逼所有 provider 实现无意义方法。若团队仍想要「按 provider 挂落代码钩子」，用方案 (B) 的**可选接口**满足，既「磨成熟抽象」又不破冻结。**此项最终取舍建议在实现任务开工前由人确认**（因为它改的是冻结接口契约，超出纯研究范畴）。

---

## 9. config 与认证草案（实现任务参考）

当前 `internal/config/config.go` 的 `Channel` 只有 `Provider / Repo / TaskLabel`（为 GitHub 设计）。Linear 需要：

```yaml
channel:
  provider: linear
  linear:
    team_id: "<team-uuid>"        # 或 team_key: "ENG"
    project_id: "<project-uuid>"  # 按 project 过滤（形态 A）；亦可 project_name
    # status_map 可选（见 §5.2；不填则按 state.type 隐式映射）
    # status_map: { running:"...", needs-review:"...", blocked:"...", done:"..." }
# API key 经环境变量，不进 config.yaml（#24 决定 A）：
#   LOOP_ENG_LINEAR_API_KEY=lin_api_...
```

- `config.Channel` 结构体需扩展（加 `Linear` 子结构或泛化字段）——**这是实现任务**，本草案只给形态。
- `config.validate()` 需加：`provider=="linear"` 时 `team_id`+`project_id` 必填（类比现有 github 校验）。
- `internal/cli/run_once.go` 的 `buildChannel` switch 需加 `case "linear": return channel.NewLinear(...)`。
- 认证：`channel.NewLinear` 从环境变量读 key（参考设计文档 `auth_token_env` 思路），存入结构体，每次 GraphQL 请求带 `Authorization` header。

---

## 10. 待人工核实项（实现前用真实 workspace + introspection 复核）

1. **`commentCreate.input.issueId` 是否接受 `ENG-123` 简写**——文档只明示 `issue`/`issueUpdate` 的 `id` 接受简写；`commentCreate.issueId` 未明示。建议实现里统一喂 UUID（identifier→UUID 解析），规避此不确定性。
2. **`project(id:)` 是否接受人类简写**——issue 明确接受，project 未明示；保险用 UUID。
3. **Issue / Project / Cycle / Workflow / Label 的完整字段表**——本文只核实了官方文档/示例里直接出现的字段；其余（如 `url`/`cycle`/`subIssues`/`dueDate`/Project 的 `targetDate`/`startDate` 等）来自 SDK/社区，**实现前跑 introspection 复核**：`{ __type(name:"Issue"){ fields{name type{name kind}} } }`。
4. **team → workflow → workflowStates 的遍历路径**——官方例直接用根查询 `workflowStates{ nodes{ id name } }`；若要限定到单 team/单 workflow，确认 `team(id:){ workflows{ states{ nodes{} } } }` 的嵌套形态。
5. **是否按 `state.type` 隐式映射 loop status**（§5.2）——实现任务的设计选择，需确认团队 workflow 里确实存在各 type 的 state（小团队可能缺 `canceled` 或自定义很简）。
6. **daemon reconcile 对 Linear `Labels=[state.name]` 的兼容性**（§7）——现有逻辑按 GitHub `loop:<status>` label 设计；Linear 把状态放 state，daemon 侧可能要适配（不属于通道接口，但影响 park/resume 正确性）。
7. **`land` 是否进接口**（§8.3）——本研究建议不进；最终取舍待人确认。

---

## 11. 参考来源

- Linear Developers – Getting started（endpoint / 认证 header / `issue(id:"BLA-123")` 简写 / `issueCreate` / `issueUpdate` / `workflowStates` / `includeArchived`）：https://linear.app/developers/graphql
- Linear Developers – Filtering（比较器全集 / `or` / `every` / 关系过滤 / `state:{type:{eq}}` / `project:{...}` / 相对时间 / `completedAt`）：https://linear.app/developers/filtering
- Linear API Schema @current（Apollo Studio，字段级权威，支持 introspection 核对）：https://studio.apollographql.com/public/Linear-API/schema/reference?variant=current
- `IssueUpdateInput`（含 `stateId`/`completedAt`/`canceledAt`，关闭语义）：https://studio.apollographql.com/public/Linear-API/variant/current/schema/reference/inputs/IssueUpdateInput
- `WorkflowStateType` 枚举（`backlog|unstarted|started|completed|canceled`）：https://linear.app/docs/configuring-workflows ；https://docs.rs/linear-api/latest/linear_api/enum.WorkflowStateType.html
- `commentCreate` / `Comment` 字段（`issueId`+`body`，`id`/`body`/`url`/`user`/`createdAt`/`updatedAt`）：Apollo Studio Mutation/Comment 类型 + 社区 recipes（linear-cli/graphql-recipes、Nango create-comment、Anthropic linear-api skill）
- 本仓代码：`internal/channel/channel.go`（接口）、`internal/channel/github.go`（参考实现）、`internal/cli/land.go`（land 现状）、`internal/config/config.go`（config 现状）、`internal/cli/run_once.go:buildChannel`（provider 接入点）

---

## 12. 实现契约（2026-07-18 实现任务落地，钉死 Go 调用面）

实现任务按本文档落地时采用的**具体 Go API**（写在这里是因为 tier-1 验收脚本由
plan 逐轮生成，需要一个权威的签名出处；改动实现前请先同步本节）：

```go
// internal/channel/linear.go
const DefaultLinearEndpoint = "https://api.linear.app/graphql"
const LinearAPIKeyEnv = "LOOP_ENG_LINEAR_API_KEY"

type Linear struct {
    // 导出字段（包外可读）与未导出字段（包内测试可改写，如指向 httptest server）
    // 成对存在，NewLinear 同步写入；运行时取值顺序：未导出 → 导出 → 默认/env。
    APIKey, Endpoint, ProjectID, TeamID string
    StatusMap  map[string]string
    HTTPClient *http.Client
    apiKey, endpoint, projectID, teamID string
    statusMap  map[string]string
    httpClient *http.Client
    // ... 内部缓存（identifier→UUID、workflowStates）
}

// 容忍式构造：含 "://" 的字符串 = endpoint，其余字符串按序 =
// apiKey / projectID / teamID，map = statusMap。两种形态都可用：
//   NewLinear(apiKey, projectID, teamID, statusMap)
//   NewLinear(apiKey, endpoint, projectID, teamID, statusMap)
func NewLinear(first string, rest ...any) *Linear

// 容忍式 helper：vars（map[string]any）与 out（指针）按类型分拣，顺序个数不敏感：
//   lc.gql(ctx, query) / lc.gql(ctx, query, vars) / lc.gql(ctx, query, vars, &out)
// GraphQL errors 数组（HTTP 200 也算）折成 Go error；网络错误/5xx 重试 3 次。
func (lc *Linear) gql(ctx context.Context, query string, rest ...any) error
```

- 认证：`Authorization: <API_KEY>` 原始 header（**无 Bearer**）；key 为空时回落环境
  变量 `LOOP_ENG_LINEAR_API_KEY`。
- `var _ Channel = (*Linear)(nil)` 编译期断言；六方法签名严格按冻结接口。
- config：`channel.linear.project`（必填）/ `channel.linear.team`（可选）/
  `channel.linear.status_map`（可选，loop status → WorkflowState **name**）；
  `buildChannel` 的 `case "linear"` 从 env 读 key，缺 key 直接报错。

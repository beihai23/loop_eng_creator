# loop-eng M2（真实工单通道 + 真实执行）实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 loop-eng 能从真实 GitHub Issue 捞任务、在隔离 worktree 里用 `claude --dangerously-skip-permissions` 真改代码、用 `go test`（tier1）硬验证、把战报写回 issue——即把 M1 的 Fake/本地桩换成真实通道+真实执行，并在 `beihai23/loop_eng_creator` 上自举（用它自己的 issue 喂后续 M3 任务）。

**Architecture:** 新增 `model.Executer` 接口（execute 专用，带 worktree 目录参数；`ClaudeClient.Exec` 设 `cmd.Dir=worktree` + 透传 `--dangerously-skip-permissions`），把 SubLoop 的 execute 从通用 `model.Client` 切到 `Executer`；SubLoop 的验证链改为**每轮按 worktree 重建**（`tiersFor(wt)`，tier1 `Deterministic.Dir=wt`），从而接上 tier1（`go test ./...`）——这是自举的硬闸门。新增 `channel.GitHub`（shell out 到已认证的 `gh` CLI，不引 SDK、不碰 token）。`run-once` 加 `--channel github`，从 issue 捞一个任务跑完整 loop。

**Tech Stack:** Go 1.22+、`spf13/cobra`、`modernc.org/sqlite`、`os/exec`（claude / gh）、标准 `testing`。

## Global Constraints（继承 M1，仍生效）

- **Go 1.22+**，模块路径 `loop-eng`；纯 Go、单静态二进制、`CGO_ENABLED=0`。
- **验证独立（spec §8.6）**：`verify` 包不导入 `loop`；verify 只读 `(diff, criteria)`。
- **状态只追加**；**三道预算刹车**存在、config 缺失即硬错误。
- **单路径 claude-p（裁决 I）**：无 anthropic SDK；四角色全 `claude -p`。
- **`model.Client.Call(ctx, prompt)` 签名冻结**——不得为传 worktree 而改它（故另起 `Executer`）。
- 每个任务结束 `CGO_ENABLED=0 go build ./... && go test ./...` 全绿后 commit；commit message 带 `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>` 尾注。
- **安全说明**：execute 路径启用 `--dangerously-skip-permissions`（claude 在 worktree 内自主改文件/跑命令不卡权限）。仅对 loop-eng 自己创建的隔离 worktree 生效；worktree 失败即丢弃分支（spec §8.9 回滚原语）。

## File Structure（M2 新增/改动）

```
loop-eng/
  internal/
    config/config.go         # 改：加 Channel 段；default config 更新
    model/model.go           # 改：加 Executer 接口
    model/claude.go          # 改：ClaudeClient.Exec（cmd.Dir + flags）
    model/fake.go            # 改：FakeClient.Exec（测试用，委托 Call）
    channel/channel.go       # 不变（接口已冻结）
    channel/github.go        # 新：GitHub 通道（gh CLI）
    channel/github_test.go   # 新：JSON/body 解析 fixture（不打真 GH）
    loop/subloop.go          # 改：Execute→Executer；真实 execute prompt；tiersFor(wt) 接 tier1
    loop/subloop_test.go     # 改：跟随 SubLoop 字段变更 + 加 tier1 用例
    cli/init.go              # 改：default config 加 channel + execute.cmd
    cli/run_once.go          # 改：--channel；buildChannel；真 ClaudeClient 带 flags
  .loop/config.yaml          # 自举配置（beihai23/loop_eng_creator）
```

**核心接口（M2 新增，后续不得改签名）：**

```go
// internal/model/model.go —— 追加（Client 不变）
type Executer interface {
    Exec(ctx context.Context, worktreeDir, prompt string) (output string, usage Usage, err error)
}

// internal/channel/github.go
type GitHub struct{ Repo, TaskLabel string }
func NewGitHub(repo, taskLabel string) *GitHub
// 实现 channel.Channel：ListNewTasks/ListReplies/PostComment/UpdateStatus
```

**SubLoop 字段变更（Task 3/4）：**
```go
type SubLoop struct {
    Repo    string
    Store   *state.Store
    Budget  *budget.Enforcer
    Execute model.Executer          // 原 model.Client → Executer
    Plan    skill.Skill[skill.PlanInput, skill.PlanOutput]
    VerifyDeterministic []verify.Deterministic // 原 Tiers []verify.Tier 拆开
    VerifyLLM           verify.LLM
    Tier3Human          bool
    Channel channel.Channel
}
```

---

## Task 1: config —— Channel 段 + execute flags

**Files:**
- Modify: `internal/config/config.go`（加 `Channel` 结构 + 校验）
- Modify: `internal/cli/init.go`（`defaultConfig` 加 channel + `execute.cmd`)
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `Config.Channel{Provider, Repo, TaskLabel string}`；`ModelRef.Cmd`（已存在）承载 execute flags。

- [ ] **Step 1: 写失败测试（追加到 config_test.go）**

```go
func TestLoadParsesChannel(t *testing.T) {
	p := writeFile(t, `
models:
  triage: { binary: claude }
budget:
  per_call_tokens: 20000
  per_task_tokens: 200000
  max_retries: 3
channel:
  provider: github
  repo: beihai23/loop_eng_creator
  task_label: "loop:task"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
	if cfg.Channel.Provider != "github" || cfg.Channel.Repo != "beihai23/loop_eng_creator" {
		t.Fatalf("channel not parsed: %+v", cfg.Channel)
	}
	if cfg.Channel.TaskLabel != "loop:task" {
		t.Fatalf("task_label not parsed: %q", cfg.Channel.TaskLabel)
	}
}
```

- [ ] **Step 2: 跑测试看它失败**

Run: `go test ./internal/config/ -run TestLoadParsesChannel -v`
Expected: FAIL（`cfg.Channel.Provider` 为空——结构未定义）

- [ ] **Step 3: 实现 Channel 段**

在 `config.go` 的 `Config` 结构加字段、加 `Channel` 类型；`validate()` 在 `provider=="github"` 时要求 `repo` 与 `task_label` 非空（provider 留空 = 本地，向后兼容 M1）。

```go
type Config struct {
	Models    Models    `yaml:"models"`
	Budget    Budget    `yaml:"budget"`
	Verify    Verify    `yaml:"verify"`
	Isolation Isolation `yaml:"isolation"`
	Skills    Skills    `yaml:"skills"`
	Channel   Channel   `yaml:"channel"`
}

type Channel struct {
	Provider   string `yaml:"provider"`    // "" | "local" | "github"
	Repo       string `yaml:"repo"`        // "owner/name"（github 必填）
	TaskLabel  string `yaml:"task_label"`  // issue 过滤标签（github 必填）
}
```

在 `validate()` 末尾追加：
```go
if c.Channel.Provider == "github" && (c.Channel.Repo == "" || c.Channel.TaskLabel == "") {
	return fmt.Errorf("channel: github provider 需 repo 与 task_label")
}
```

- [ ] **Step 4: 更新 `init.go` 的 `defaultConfig`**

把 `models.execute` 加上 `cmd: ["--dangerously-skip-permissions"]`，并加 `channel` 段（默认 local，自举时改 github）：

```yaml
models:
  triage:  { via: claude-p, binary: claude }
  plan:    { via: claude-p, binary: claude }
  execute: { via: claude-p, binary: claude, cmd: ["--dangerously-skip-permissions"] }
  verify:  { via: claude-p, binary: claude }
budget:
  per_call_tokens: 20000
  per_task_tokens: 200000
  max_retries: 3
verify:
  deterministic:
    - { label: go-test, cmd: ["go", "test", "./..."] }
  tier3_human: true
isolation: { worktree: true }
skills: { dir: .loop/skills }
channel: { provider: local }
```

> 注：`verify.deterministic` 默认给一条 `go test ./...`（M2 接入 tier1 用，见 Task 4）。

- [ ] **Step 5: 跑测试看它过 + 全量 gate**

Run: `go test ./internal/config/ -v` → PASS（三条）；`CGO_ENABLED=0 go build ./... && go test ./...` 全绿。
> 若 `init_test.go` 的 `TestInitScaffoldsLoopDir` 因 defaultConfig 变化而断言失败，按新 config 调整断言（不放宽语义）。

- [ ] **Step 6: Commit**

```bash
git add -A && git commit -m "feat(config): Channel 段 + execute --dangerously-skip-permissions 默认"
```

---

## Task 2: model.Executer 接口 + ClaudeClient.Exec + FakeClient.Exec

**Files:**
- Modify: `internal/model/model.go`（加 `Executer` 接口）
- Modify: `internal/model/claude.go`（加 `Exec` 方法）
- Modify: `internal/model/fake.go`（加 `Exec` 委托）
- Test: `internal/model/exec_test.go`

**Interfaces:**
- Produces: `model.Executer{ Exec(ctx, worktreeDir, prompt) (string, Usage, error) }`；`*ClaudeClient` 与 `*FakeClient` 均实现它。

- [ ] **Step 1: 写失败测试（验证 cmd.Dir 被设到 worktreeDir）**

用一个临时脚本当 "claude"：读空 stdin、打印 cwd。这样不依赖真 claude 也能验证 `Exec` 把进程跑在 `worktreeDir`。

```go
// internal/model/exec_test.go
package model

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// writeFakeBinary 写一个脚本：丢弃 stdin、打印当前工作目录。
func writeFakeBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	var path string
	var content string
	if runtime.GOOS == "windows" {
		path = filepath.Join(dir, "fake-claude.bat")
		content = "@echo off\r\nping -n 1 127.0.0.1 >nul\r\ncd\r\n"
	} else {
		path = filepath.Join(dir, "fake-claude")
		content = "#!/bin/sh\ncat >/dev/null\npwd\n"
	}
	if err := os.WriteFile(path, []byte(content), 0755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestClaudeClientExecRunsInWorktreeDir(t *testing.T) {
	bin := writeFakeBinary(t)
	wt := t.TempDir()
	c := NewClaudeClient(bin, []string{"--dangerously-skip-permissions"})
	out, _, err := c.Exec(context.Background(), wt, "do the task")
	if err != nil {
		t.Fatal(err)
	}
	// out 是脚本打印的 cwd，必须等于 wt（说明 cmd.Dir=wt 生效）
	abs, _ := filepath.Abs(wt)
	if !strings.Contains(out, abs) {
		t.Fatalf("Exec should run in worktree %s, got %q", abs, out)
	}
}
```

> 测试文件头需 `"strings"` import。

- [ ] **Step 2: 跑测试看它失败**

Run: `go test ./internal/model/ -run TestClaudeClientExecRunsInWorktreeDir -v`
Expected: FAIL（`c.Exec` 未定义）

- [ ] **Step 3: 加 Executer 接口（model.go 末尾追加）**

```go
// Executer is the worktree-aware execute gateway. Execute must run the agent
// INSIDE the isolated worktree (cmd.Dir = worktreeDir) so its edits land on the
// worktree, not the base repo. Client.Call carries no directory, hence a
// separate interface (Client.Call stays frozen).
type Executer interface {
	Exec(ctx context.Context, worktreeDir, prompt string) (output string, usage Usage, err error)
}
```

- [ ] **Step 4: ClaudeClient 实现 Exec（claude.go 追加）**

```go
// Exec runs `claude -p <Args>` with the process working directory set to
// worktreeDir and the prompt on stdin. Args (e.g. --dangerously-skip-permissions)
// come from config so execute runs autonomously without permission stalls.
func (c *ClaudeClient) Exec(ctx context.Context, worktreeDir, prompt string) (string, Usage, error) {
	args := append([]string{"-p"}, c.Args...)
	cmd := exec.CommandContext(ctx, c.Binary, args...)
	cmd.Dir = worktreeDir
	cmd.Stdin = strings.NewReader(prompt)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return "", Usage{}, fmt.Errorf("claude -p (exec @ %s): %w: %s", worktreeDir, err, errBuf.String())
	}
	return out.String(), Usage{TokensOut: out.Len()}, nil
}
```

- [ ] **Step 5: FakeClient 实现 Exec（fake.go 追加，委托 Call 忽略 dir）**

```go
// Exec delegates to Call, ignoring worktreeDir (tests don't execute real agents).
func (f *FakeClient) Exec(_ context.Context, _ string, prompt string) (string, Usage, error) {
	return f.Call(context.Background(), prompt)
}
```

- [ ] **Step 6: 跑测试看它过 + 全量 gate**

Run: `go test ./internal/model/ -v` → PASS；`CGO_ENABLED=0 go build ./... && go test ./...` 全绿。

- [ ] **Step 7: Commit**

```bash
git add -A && git commit -m "feat(model): Executer 接口 + ClaudeClient.Exec(worktree, --dangerously-skip-permissions)"
```

---

## Task 3: SubLoop execute → Executer + 真实 execute prompt

**Files:**
- Modify: `internal/loop/subloop.go`（`Execute` 字段类型改 `model.Executer`；execute 步骤改用 `Exec` + 真实 prompt）
- Test: `internal/loop/subloop_test.go`（跟随字段类型；构造器里 `Execute: fake` 仍可用，因 FakeClient 实现了 Executer）

**Interfaces:**
- Consumes: `model.Executer`（Task 2）。
- Produces: `SubLoop.Execute model.Executer`（后续 Task 14 run-once 适配）。

- [ ] **Step 1: 改 SubLoop.Execute 字段类型**

`subloop.go` 里：
```go
type SubLoop struct {
	Repo    string
	Store   *state.Store
	Budget  *budget.Enforcer
	Execute model.Executer // 原 model.Client
	Plan    skill.Skill[skill.PlanInput, skill.PlanOutput]
	Tiers   []verify.Tier // Task 4 会拆掉，本任务先保留
	Channel channel.Channel
}
```

- [ ] **Step 2: execute 步骤改用 Exec + 真实 prompt**

把 Run 里这段：
```go
		execOut, u2, err := sl.Execute.Call(ctx, "EXECUTE: "+task.Description+" @ "+wt)
```
改为构造真实 prompt 并调 `Exec`：
```go
		execPrompt := "EXECUTE: 你在一个 git worktree 里（当前工作目录即工作区）。\n" +
			"任务: " + task.Description + "\n" +
			"验收标准:\n" + criteriaBlock(task.AcceptanceCriteria) + "\n" +
			"在当前目录实现任务，确保 `go test ./...` 通过且满足全部验收标准。"
		execOut, u2, err := sl.Execute.Exec(ctx, wt, execPrompt)
```
新增辅助（同文件）：
```go
func criteriaBlock(c []string) string {
	if len(c) == 0 {
		return "(未提供)"
	}
	b := ""
	for _, line := range c {
		b += "- " + line + "\n"
	}
	return b
}
```
> prompt 以 `EXECUTE:` 开头，FakeClient 的前缀匹配仍生效（done/blocked 测试不变）。

- [ ] **Step 3: 跑现有 loop 测试（应仍绿，因 FakeClient.Exec 委托 Call）**

Run: `go test ./internal/loop/ -v`
Expected: PASS（TestSubLoopDoneOnFirstPass / TestSubLoopBlockedAfterRetries 不变）

- [ ] **Step 4: 全量 gate + Commit**

Run: `CGO_ENABLED=0 go build ./... && go test ./...` 全绿。
```bash
git add -A && git commit -m "feat(loop): execute 走 Executer + 真实 execute prompt(worktree)"
```

---

## Task 4: SubLoop tier1 接入 —— tiersFor(wt)（解决裁决 E）

**Files:**
- Modify: `internal/loop/subloop.go`（拆 `Tiers` → `VerifyDeterministic`/`VerifyLLM`/`Tier3Human` + `tiersFor(wt)`）
- Test: `internal/loop/subloop_test.go`（改构造 + 加 tier1 失败用例）

**Interfaces:**
- Consumes: `verify.Deterministic{Label, Cmd, Dir}`（M1 Task 9 已实现）。
- Produces: SubLoop 每轮按 worktree 重建 tier 链（tier1 在 worktree 里跑）。

- [ ] **Step 1: 写失败测试（tier1 失败 → 不 done）**

追加到 subloop_test.go：
```go
func TestSubLoopTier1FailBlocks(t *testing.T) {
	repo := initRepo(t)
	st, _ := state.Open(t.TempDir() + "/s.db")
	defer st.Close()
	fake := model.NewFake(map[string]string{
		"PLAN:":    mustJSON(skill.PlanOutput{}),
		"EXECUTE:": "ok",
		"VERIFY:":  mustJSON(skill.VerifyOutput{Passed: true}),
	})
	sl := &SubLoop{
		Repo: repo, Store: st, Budget: budget.New(100000, 1000000, 2),
		Execute: fake,
		Plan:    mkSkill[skill.PlanInput, skill.PlanOutput]("PLAN:", fake),
		VerifyDeterministic: []verify.Deterministic{{Label: "go-test", Cmd: []string{"false"}}}, // 永远失败
		VerifyLLM:           verify.LLM{Skill: mkSkill[skill.VerifyInput, skill.VerifyOutput]("VERIFY:", fake)},
		Tier3Human:          true,
		Channel:             channel.NewLocal(t.TempDir()),
	}
	out, _ := sl.Run(context.Background(), channel.Task{Ref: "3", Description: "d"})
	if out.Status != "blocked" {
		t.Fatalf("tier1 always-fail must block, got %s", out.Status)
	}
}
```

- [ ] **Step 2: 跑测试看它失败**

Run: `go test ./internal/loop/ -run TestSubLoopTier1FailBlocks -v`
Expected: FAIL（`VerifyDeterministic` 字段未定义 / `Tiers` 仍是旧字段）

- [ ] **Step 3: 重构 SubLoop 字段 + tiersFor**

把 `Tiers []verify.Tier` 替换为三字段 + 一个方法：
```go
type SubLoop struct {
	Repo    string
	Store   *state.Store
	Budget  *budget.Enforcer
	Execute model.Executer
	Plan    skill.Skill[skill.PlanInput, skill.PlanOutput]
	VerifyDeterministic []verify.Deterministic // tier1：Dir 每轮设为 wt
	VerifyLLM           verify.LLM             // tier2
	Tier3Human          bool                   // tier3 stub 开关
	Channel channel.Channel
}

// tiersFor 在每轮按 worktree 重建 tier 链：tier1（在 wt 里跑）→ tier2 → tier3。
// 这是裁决 E 的落地——execute 已 worktree 化（Task 2/3），故 tier1 的 Dir 可注入。
func (sl *SubLoop) tiersFor(wt string) []verify.Tier {
	var tiers []verify.Tier
	for _, d := range sl.VerifyDeterministic {
		d.Dir = wt
		tiers = append(tiers, d)
	}
	tiers = append(tiers, sl.VerifyLLM)
	if sl.Tier3Human {
		tiers = append(tiers, verify.HumanStub{})
	}
	return tiers
}
```

Run 里把 `verify.Chain(ctx, sl.Tiers, ...)` 改为 `verify.Chain(ctx, sl.tiersFor(wt), ...)`。

- [ ] **Step 4: 改既有测试的构造器**

把 `TestSubLoopDoneOnFirstPass` / `TestSubLoopBlockedAfterRetries` 里的：
```go
		Tiers: []verify.Tier{verify.LLM{Skill: mkSkill[...]("VERIFY:", fake)}, verify.HumanStub{}},
```
改为：
```go
		VerifyLLM:  verify.LLM{Skill: mkSkill[skill.VerifyInput, skill.VerifyOutput]("VERIFY:", fake)},
		Tier3Human: true,
```
（done 路径：tier1 空 + LLM pass + HumanStub → done；blocked 路径：LLM fail → 重试耗尽 → blocked。语义不变。）

- [ ] **Step 5: 跑测试看它过 + 全量 gate**

Run: `go test ./internal/loop/ -v`（三条全过：done / blocked / tier1-fail-blocks）；`CGO_ENABLED=0 go build ./... && go test ./...` 全绿。
> 若 `run_once.go` 的 `buildTiers` 仍返回 `[]verify.Tier` 赋给 `SubLoop.Tiers`——Task 6 会改 run-once 适配新字段；本任务先把 run-once 的 `Tiers: tiers` 行临时改成等价的 `VerifyLLM/Tier3Human`（见 Task 6 才正式接 tier1）。**为避免本任务破坏 run-once 编译**，同步把 `run_once.go` 里 `sl := &loop.SubLoop{...Tiers: tiers...}` 改为 `VerifyLLM: verify.LLM{Skill: verifySkill}, Tier3Human: true`（删掉 buildTiers 调用与函数），保持 run-once 行为（tier1 留到 Task 6 接）。

- [ ] **Step 6: Commit**

```bash
git add -A && git commit -m "feat(loop): tier1 接入——tiersFor(wt) 每轮按 worktree 重建验证链"
```

---

## Task 5: channel.GitHub（gh CLI 实现）

**Files:**
- Create: `internal/channel/github.go`、`internal/channel/github_test.go`

**Interfaces:**
- Produces: `channel.GitHub{Repo, TaskLabel}`，实现 `channel.Channel`（ListNewTasks/ListReplies/PostComment/UpdateStatus）。

**设计：** shell out 到已认证的 `gh` CLI（不引 go-github、不存 token，复用 beihai23 的 gh 登录）。issue 正文沿用 spec §9 格式（`## 任务\n<desc>\ntype: <t>\n## 验收标准\n- [ ] <c>`），复用 M1 的 `parseLocalTask` 逻辑解析 body。

- [ ] **Step 1: 写失败测试（JSON + body 解析，不打真 GH）**

```go
// internal/channel/github_test.go
package channel

import "testing"

func TestParseIssueJSONAndBody(t *testing.T) {
	raw := `[{"number":42,"title":"add X","body":"## 任务\nadd X\ntype: feature\n## 验收标准\n- [ ] it works"}]`
	tasks, err := parseIssuesJSON([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].Ref != "42" || tasks[0].TaskType != "feature" {
		t.Fatalf("parse wrong: %+v", tasks)
	}
	if len(tasks[0].AcceptanceCriteria) != 1 || tasks[0].AcceptanceCriteria[0] != "it works" {
		t.Fatalf("criteria wrong: %+v", tasks[0].AcceptanceCriteria)
	}
	if tasks[0].Description != "add X" {
		t.Fatalf("desc wrong: %q", tasks[0].Description)
	}
}
```

- [ ] **Step 2: 跑测试看它失败**

Run: `go test ./internal/channel/ -run TestParseIssueJSONAndBody -v`
Expected: FAIL（`parseIssuesJSON` 未定义）

- [ ] **Step 3: 实现 github.go**

```go
// internal/channel/github.go
package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
)

// GitHub is a channel.Channel backed by the authenticated `gh` CLI (no SDK,
// no stored token — reuses the operator's `gh auth`). Issues with TaskLabel
// are tasks; comments are battle reports; status moves via labels loop:<status>.
type GitHub struct {
	Repo       string // owner/name
	TaskLabel  string // e.g. "loop:task"
}

func NewGitHub(repo, taskLabel string) *GitHub {
	return &GitHub{Repo: repo, TaskLabel: taskLabel}
}

type ghIssue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
}

// parseIssuesJSON decodes `gh issue list --json` output into Tasks.
func parseIssuesJSON(raw []byte) ([]Task, error) {
	var issues []ghIssue
	if err := json.Unmarshal(raw, &issues); err != nil {
		return nil, fmt.Errorf("parse gh issues: %w", err)
	}
	var tasks []Task
	for _, is := range issues {
		t := parseLocalTask(is.Body) // 复用 M1 的 body 解析（## 任务/type:/- [ ]）
		t.Ref = strconv.Itoa(is.Number)
		if t.Description == "" {
			t.Description = is.Title // body 没解析出 desc 则回落 title
		}
		tasks = append(tasks, t)
	}
	return tasks, nil
}

func (g *GitHub) ListNewTasks(ctx context.Context) ([]Task, error) {
	out, err := g.gh(ctx, "issue", "list", "--repo", g.Repo,
		"--label", g.TaskLabel, "--state", "open",
		"--json", "number,title,body", "--limit", "50")
	if err != nil {
		return nil, err
	}
	return parseIssuesJSON(out)
}

func (g *GitHub) PostComment(ctx context.Context, ref, body string) error {
	_, err := g.gh(ctx, "issue", "comment", ref, "--repo", g.Repo, "--body", body)
	return err
}

func (g *GitHub) UpdateStatus(ctx context.Context, ref, status string) error {
	_, err := g.gh(ctx, "issue", "edit", ref, "--repo", g.Repo, "--add-label", "loop:"+status)
	return err
}

func (g *GitHub) ListReplies(ctx context.Context, refs []string) (map[string][]Reply, error) {
	// M2 单次 run-once 不需要回复（无 daemon/park）；返回空。M3 daemon 实现真回复拉取。
	return map[string][]Reply{}, nil
}

// gh runs a `gh` command and returns stdout. stderr is folded into the error.
func (g *GitHub) gh(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	var out, errBuf outBuf
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("gh %v: %w: %s", args, err, errBuf.String())
	}
	return out.Bytes(), nil
}

type outBuf struct{ b []byte }

func (b *outBuf) Write(p []byte) (int, error) { b.b = append(b.b, p...); return len(p), nil }
func (b *outBuf) Bytes() []byte                { return b.b }
func (b *outBuf) String() string               { return string(b.b) }
```

> `parseLocalTask` 已存在于 `local.go`（同包），直接复用。

- [ ] **Step 4: 跑测试看它过 + 全量 gate**

Run: `go test ./internal/channel/ -v`；`CGO_ENABLED=0 go build ./... && go test ./...` 全绿。

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat(channel): GitHub 通道（gh CLI，复用登录）"
```

---

## Task 6: run-once --channel github + tier1 接入 + buildChannel

**Files:**
- Modify: `internal/cli/run_once.go`（`--channel` flag、`buildChannel`、SubLoop 新字段、真 ClaudeClient 带 flags）
- Test: `internal/cli/run_once_test.go`

**Interfaces:**
- Consumes: `config.Channel`、`channel.GitHub`、`SubLoop` 新字段（Task 4）、`model.NewClaudeClient(binary, cmd)`。

- [ ] **Step 1: 写失败测试（注入 fake channel，--channel 不影响 run-once 主流程）**

```go
func TestRunOnceGitHubPathWiresChannel(t *testing.T) {
	// 这个测试验证 run-once 能按 cfg.Channel.Provider 选通道、且 SubLoop 字段装配正确。
	// 用 fake 模型 + local inbox 仍跑通 done（不真打 GH）。
	repo := t.TempDir()
	initGitRepo(t, repo)
	NewRootCmd().SetArgs([]string{"init", "--repo", repo}).Execute()
	// 把 config 的 channel.provider 改 local（默认就是 local），用 local inbox 验证装配
	inbox := filepath.Join(repo, "inbox")
	os.MkdirAll(inbox, 0755)
	os.WriteFile(filepath.Join(inbox, "1.md"), []byte("# 任务\ndo thing\ntype: bugfix\n## 验收标准\n- [ ] c"), 0644)
	cmd := NewRootCmd()
	cmd.SetArgs([]string{"run-once", "--repo", repo, "--channel", "local", "--models", "fake"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, "outbox", "1.md")); err != nil {
		t.Fatalf("no battle report: %v", err)
	}
}
```

- [ ] **Step 2: 跑测试看它失败**

Run: `go test ./internal/cli/ -run TestRunOnceGitHubPathWiresChannel -v`
Expected: FAIL（`--channel` flag 未定义）

- [ ] **Step 3: 改 run_once.go**

(a) 加 `--channel` flag：
```go
	var repo, inbox, models, channelFlag string
	...
	cmd.Flags().StringVar(&channelFlag, "channel", "", "local | github（空=用 cfg.Channel.Provider）")
```

(b) `buildModels` 真 ClaudeClient 带 execute flags（从 config 读 binary + cmd）：
```go
	m = model.NewClaudeClient(cfg.Models.Execute.Binary, cfg.Models.Execute.Cmd)
```
（fake 分支不变；`buildModels` 签名加 `cfg *config.Config`——原来是 `_ *config.Config`，现用它读 binary/cmd。）

(c) 加 `buildChannel`：
```go
func buildChannel(cfg *config.Config, repo string) (channel.Channel, error) {
	prov := cfg.Channel.Provider
	if prov == "" {
		prov = "local"
	}
	switch prov {
	case "local", "":
		return channel.NewLocal(repo), nil
	case "github":
		return channel.NewGitHub(cfg.Channel.Repo, cfg.Channel.TaskLabel), nil
	default:
		return nil, fmt.Errorf("unknown channel provider: %s", prov)
	}
}
```

(d) RunE 里：`channelFlag` 非空时覆盖 `cfg.Channel.Provider`；用 `buildChannel`；SubLoop 装配用新字段（Task 4 拆开的）：
```go
	if channelFlag != "" {
		cfg.Channel.Provider = channelFlag
	}
	ch, err := buildChannel(cfg, repo)
	if err != nil {
		return err
	}
	...
	// tier1 从 cfg.Verify.Deterministic 构造（裁决 E 落地）
	dets := make([]verify.Deterministic, 0, len(cfg.Verify.Deterministic))
	for _, d := range cfg.Verify.Deterministic {
		dets = append(dets, verify.Deterministic{Label: d.Label, Cmd: d.Cmd})
	}
	sl := &loop.SubLoop{
		Repo: repo, Store: st, Budget: bz,
		Execute: exec,
		Plan:    plan,
		VerifyDeterministic: dets,
		VerifyLLM:           verify.LLM{Skill: verifySkill},
		Tier3Human:          cfg.Verify.Tier3Human,
		Channel: ch,
	}
```

(e) 删掉旧的 `buildTiers` 函数与调用（已被 SubLoop 新字段 + tiersFor 取代）。

- [ ] **Step 4: 跑测试看它过 + 全量 gate**

Run: `go test ./internal/cli/ -v`；`CGO_ENABLED=0 go build ./... && go test ./...` 全绿。

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat(cli): run-once --channel github + tier1 从 config 接入"
```

---

## Task 7: 自举接种 —— 配置 beihai23/loop_eng_creator + 种子 issue + 实机冒烟

**Files:**
- Create: `.loop/config.yaml`（本仓库自举配置，**gitignore 已忽略 .loop/**，不提交）
- 操作：GitHub issue（用 `gh` 建种子 issue）

> 这是验收任务：证明 loop-eng 能读真 issue、在 worktree 真改代码、tier1（go test）验证、写回评论。

- [ ] **Step 1: 本仓库 init + 改 channel 为 github**

```bash
cd /Users/lance.wang/workspace/wzgown/loop_eng_creator
go build -o /tmp/loop-eng ./cmd/loop-eng
/tmp/loop-eng init --repo .
# 编辑 .loop/config.yaml：channel 改为
#   channel: { provider: github, repo: beihai23/loop_eng_creator, task_label: "loop:task" }
```

- [ ] **Step 2: 建一个最小种子 issue（手动可控、可判定）**

```bash
gh issue create --repo beihai23/loop_eng_creator --label "loop:task" --title "docs: 在 README 加一行 'hello from loop-eng'" --body '## 任务
在 README.md 末尾追加一行: `hello from loop-eng`
type: docs
## 验收标准
- [ ] README.md 末尾存在文本 hello from loop-eng'
```

- [ ] **Step 3: 实机跑 run-once（真 claude + 真 GH）**

```bash
/tmp/loop-eng run-once --repo . --channel github --models real
```
Expected: `outcome: done`；该 issue 出现一条 `DONE: …` 评论；worktree 里 README.md 被改、`go test ./...` 通过（tier1）。

- [ ] **Step 4: 人工核对 + 收尾**

- `gh issue view <N> --comments` 看到 `DONE:` 战报。
- 确认改动落在某个 `loop/<runID>` worktree（未污染主分支）。
- 若 claude 改错/没过 tier1 → 看 `replay` + issue 上的 `BLOCKED:` 战报，调 execute prompt 或验收标准后重试（这本身就是 loop 的反馈环）。

- [ ] **Step 5: Commit（若有对工具本身的修正，如 execute prompt 调优）**

```bash
git add -A && git commit -m "test(bootstrap): beihai23/loop_eng_creator 自举冒烟通过"
```

---

## Phase 2: 自举接种点（M3 backlog —— 由 loop-eng 自己从 issue 干）

完成 Phase 1（Task 1–7）后，loop-eng 能读 `beihai23/loop_eng_creator` 的 `loop:task` issue 并自主实现。把下列 M3 项写成**小而可测**的 issue（每个验收标准都映射到 `go test`，让 tier1 当硬闸门），逐个喂给 loop-eng：

1. **budget_ledger 接线** —— SubLoop/budget.Client 在 BeforeCall/AfterCall 后 `Store.AppendBudget(runID, scope, kind, amount, limit)`；加测试断言行写入。（关 M1 遗留债）
2. **常驻 daemon** —— `internal/daemon/engine.go`：tick 循环（poll_interval）、单活跃子循环、FIFO 队列、收割路由。`loop-eng daemon` 命令。加 daemon tick 单测（假 channel + 假 model 驱动多 tick，断言 FIFO/单活跃）。
3. **park/resume** —— tier3 `NeedsHuman` 时 park（释放活跃位）、daemon 轮询 `ListReplies` 恢复。加 park→resume 单测。
4. **异步 tier-3** —— `verify.Human`（非 stub）：发 review-request 评论、置 needs-review、park。替换 HumanStub。
5. **tier1 动态装配** —— 已在 Task 4/6 落地；M3 daemon 侧确认 config 驱动。
6. **githubChannel 补全** —— `ListReplies` 真实现（`gh issue view --comments --json`）、分页、rate-limit 退避、`UpdateStatus` 去 label。
7. **真模型 smoke 扩面** —— 用真 claude 跑一个 go 代码任务（非 docs），验证 tier1=go test 在 worktree 内真生效。

> 自举纪律：每条 issue 的验收标准必须可被 `go test ./...` 或确定性脚本判定（tier1），否则 tier2 LLM 验证太弱、loop 易空转。复杂项（daemon 并发）若 loop-eng 多次 blocked，由人拆细后再喂。

---

## M2 完成标准（Definition of Done）

- [ ] `CGO_ENABLED=0 go build ./...` 单二进制；`go test ./...` 全绿（含 tier1 接入、githubChannel 解析、run-once --channel）。
- [ ] `loop-eng run-once --channel github` 能从 `beihai23/loop_eng_creator` 的 `loop:task` issue 捞一个任务，在 worktree 真改代码，tier1（go test）验证，把战报写回 issue 评论。
- [ ] execute 路径带 `--dangerously-skip-permissions`、在 worktree 内执行（不污染主分支）。
- [ ] 自举种子 issue（docs 类）跑通 done。

**M2 不做（留给 M3 / 自举）：** daemon 常驻；park/resume；异步 tier-3；budget_ledger 接线；ListReplies 真实现；并发。

---

## 自检（Self-Review）

**1. Spec 覆盖：**
- 真工单通道（githubChannel，spec §8.11）→ Task 5 ✓（用 gh CLI 而非 go-github——见下「偏差」）
- issue 即任务、评论即战报（spec §8.1/§14）→ Task 5/7 ✓
- 真实 execute（claude -p 在 worktree，spec §8.10）→ Task 2/3 ✓（Executer + cmd.Dir）
- tier1 确定性脚本（spec §8.6）→ Task 4/6 ✓（裁决 E 落地）
- 通道可插拔（spec §6.2 接口不变）→ Task 5/6 ✓（GitHub 实现 frozen Channel 接口）
- `--dangerously-skip-permissions`（用户要求）→ Task 1/2 ✓（config.cmd → ClaudeClient.Args）

**2. 偏差说明（有意为之）：**
- **gh CLI 而非 go-github**：spec §8.11 写 go-github，M2 改用 `gh` CLI——复用操作者已有 gh 登录（不碰 token，契合裁决 I「不碰用户 key」）、零新依赖、契合工具 shell-out 哲学。代价：依赖宿主机装了 `gh`。可在 Phase 2 issue 里让 loop-eng 自己评估/补 go-github 备选实现。

**3. 占位符扫描：** 无 TBD/TODO；每步有完整代码或确切命令。Task 7 的 issue body 是可直接执行的种子任务。

**4. 类型一致性：** `Executer.Exec(ctx, worktreeDir, prompt)`（Task 2 定义，Task 3 消费）；`SubLoop.Execute model.Executer`（Task 3 改，Task 6 装配）；`SubLoop.VerifyDeterministic/VerifyLLM/Tier3Human + tiersFor(wt)`（Task 4 定义，Task 6 装配）；`channel.GitHub`（Task 5 定义，Task 6 经 buildChannel 装配）。签名跨任务一致。

**5. 自举可行性：** Phase 1 全部手搓到位后（Task 7 冒烟过），loop-eng 具备「读 issue → worktree 改码 → go test 验 → 写评论」闭环；Phase 2 的 M3 项可逐个做成 `loop:task` issue 喂给它自举。风险：tier2 LLM 验证偏弱——故强制每条 issue 验收标准映射 `go test`（tier1 硬闸门）。

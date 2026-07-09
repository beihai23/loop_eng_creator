// Package loop implements the per-task control loop (SubLoop): plan → execute
// (in a fresh worktree) → verify (Chain of tiers) → writeback (state trace +
// channel report), with bounded retries governed by the budget Enforcer.
package loop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"loop-eng/internal/budget"
	"loop-eng/internal/channel"
	"loop-eng/internal/isolation"
	"loop-eng/internal/model"
	"loop-eng/internal/skill"
	"loop-eng/internal/state"
	"loop-eng/internal/verify"
)

// Outcome is the terminal result of a SubLoop.Run. Status is one of
// done | blocked | needs-info | needs-review (M1 only produces done/blocked;
// needs-review is M3 machinery).
type Outcome struct {
	Status string // done | blocked | needs-info | needs-review
	Detail string
}

// SubLoop drives a single task through plan→execute→verify→writeback, retrying
// on verify failure up to Budget.MaxRetries. Each attempt executes in a fresh
// worktree; a non-passing attempt discards that worktree.
type SubLoop struct {
	Repo                string
	Store               *state.Store
	Budget              *budget.Enforcer
	Execute             model.Executer
	Plan                skill.Skill[skill.PlanInput, skill.PlanOutput]
	VerifyDeterministic []verify.Deterministic // tier1：Dir 每轮设为 wt
	VerifyLLM           verify.LLM             // tier2
	Tier3Human          bool                   // tier3 开关：true 时挂 tier-3（HumanTier，否则回落 HumanStub）
	HumanTier           verify.Tier            // M3 真 tier-3 人审 tier；nil 时回落 HumanStub（自动通过占位）
	Channel             channel.Channel
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
	// tier-3：M3 注入了真人审 tier（HumanTier）就用它；否则 Tier3Human 时挂 HumanStub
	// 自动通过占位。HumanStub 不再产 NeedsHuman，故 M1/M2 的 done/blocked 路径不受影响。
	switch {
	case sl.HumanTier != nil:
		tiers = append(tiers, sl.HumanTier)
	case sl.Tier3Human:
		tiers = append(tiers, verify.HumanStub{})
	}
	return tiers
}

// planExecEstimate is the conservative per-call token estimate SubLoop feeds
// the plan and execute BeforeCall pre-checks. AppendBudget logs the same value
// so the durable budget_ledger row records exactly the estimate the Enforcer
// checked (spec §8.8).
const planExecEstimate = 1000

// Run executes the plan→execute→verify→writeback loop for one task.
//
// Verify routing (spec §7.2c/§8.6/§10): NeedsHuman → needs-review (park; the
// M3 tier-3 signal, takes precedence over Passed), else Passed → done, else the
// failure becomes next round's feedback (retry within budget). HumanStub is now
// an auto-pass placeholder (NeedsHuman=false), so M1/M2 done/blocked outcomes
// are unchanged. Every terminal outcome writes a state transition + battle report.
func (sl *SubLoop) Run(ctx context.Context, task channel.Task) (Outcome, error) {
	taskID, err := sl.Store.InsertTask(state.TaskRow{
		IssueRef: task.Ref, Description: task.Description,
		TaskType: task.TaskType, Source: "run-once", Criteria: task.AcceptanceCriteria,
	})
	if err != nil {
		return Outcome{Status: "error"}, err
	}
	sl.Store.AppendTransition(taskID, "", "running", "dispatched")

	priorFailure := ""
	for attempt := 1; sl.Budget.ShouldRetry(attempt); attempt++ {
		// 预算刹车·重试：每轮入口记一行（spec §8.8）
		sl.Store.AppendBudget(taskID, "task", "retry", attempt, sl.Budget.MaxRetries)

		// ---- plan ----
		if err := sl.Budget.BeforeCall(planExecEstimate); err != nil {
			return sl.report(ctx, taskID, task, "blocked", "budget: "+err.Error()), nil
		}
		// 预算刹车·每调用 token：plan 模型调用前记一行（spec §8.8）
		sl.Store.AppendBudget(taskID, "call", "tokens", planExecEstimate, sl.Budget.PerCall)
		_, u, err := sl.Plan.Run(ctx, skill.PlanInput{
			Task: task.Description, AcceptanceCriteria: task.AcceptanceCriteria,
			BattleReport: priorFailure,
		})
		sl.Budget.AfterCall(u)
		sl.Store.AppendStep(state.StepRow{RunID: taskID, Seq: attempt*10 + 1, Role: "plan", Status: statusOf(err), Error: errStr(err)})
		if err != nil {
			if errors.Is(err, model.ErrClaudeFatal) {
				return sl.report(ctx, taskID, task, "blocked", "fatal model error: "+err.Error()), nil
			}
			priorFailure = "plan error: " + err.Error()
			continue
		}

		// ---- execute (in a fresh worktree) ----
		wt, err := isolation.Create(sl.Repo, taskID+"-r"+fmt.Sprint(attempt))
		if err != nil {
			return Outcome{Status: "error", Detail: err.Error()}, err
		}
		if err := sl.Budget.BeforeCall(planExecEstimate); err != nil {
			isolation.Discard(sl.Repo, wt)
			return sl.report(ctx, taskID, task, "blocked", "budget: "+err.Error()), nil
		}
		// 预算刹车·每调用 token：execute 模型调用前记一行（spec §8.8）
		sl.Store.AppendBudget(taskID, "call", "tokens", planExecEstimate, sl.Budget.PerCall)
		execPrompt := "EXECUTE: 你在一个 git worktree 里（当前工作目录即工作区）。\n" +
			"任务: " + task.Description + "\n" +
			"验收标准:\n" + criteriaBlock(task.AcceptanceCriteria) + "\n" +
			"上下文：若任务/issue 引用了设计文档或 spec，开工前先读相关章节；也可浏览仓库的 README/docs 了解项目约定与冻结接口，再动手。\n" +
			"在当前目录实现任务，确保满足全部验收标准（若项目有测试，确保测试通过）。\n" +
			"注意：不要执行 git add / git commit —— 只修改或创建文件；loop-eng 会自动捕获你的改动生成 diff。"
		execOut, u2, err := sl.Execute.Exec(ctx, wt, execPrompt)
		sl.Budget.AfterCall(u2)
		_ = execOut
		if err != nil {
			isolation.Discard(sl.Repo, wt)
			if errors.Is(err, model.ErrClaudeFatal) {
				return sl.report(ctx, taskID, task, "blocked", "fatal model error: "+err.Error()), nil
			}
			priorFailure = "execute error: " + err.Error()
			sl.Store.AppendStep(state.StepRow{RunID: taskID, Seq: attempt*10 + 2, Role: "execute", Status: "fail", Error: err.Error()})
			continue
		}
		diff := worktreeDiff(sl.Repo, wt)
		sl.Store.AppendStep(state.StepRow{RunID: taskID, Seq: attempt*10 + 2, Role: "execute", Status: "ok", OutputJSON: diff})

		// ---- verify (Chain of tiers; independent judgment) ----
		res, err := verify.Chain(ctx, sl.tiersFor(wt), diff, task.AcceptanceCriteria, priorFailure)
		// NeedsHuman（tier-3 人审信号）记录进 verify trace；下面在 Passed 之前优先裁决。
		sl.Store.AppendStep(state.StepRow{
			RunID: taskID, Seq: attempt*10 + 3, Role: "verify",
			Status:     statusOf2(res.Passed),
			OutputJSON: fmt.Sprintf("passed=%v needs_human=%v detail=%s", res.Passed, res.NeedsHuman, res.Detail),
		})
		if err != nil {
			// verify 基础设施错误：致命（auth）→ 立刻中断；可重试 flake → 当作可重试失败。
			isolation.Discard(sl.Repo, wt)
			if errors.Is(err, model.ErrClaudeFatal) {
				return sl.report(ctx, taskID, task, "blocked", "fatal model error: "+err.Error()), nil
			}
			priorFailure = "verify error: " + err.Error()
			continue
		}
		// tier-3 人审 → needs-review：park、释放活跃位，worktree 保留待 daemon 恢复（spec §7.2c/§8.6/§10）。
		// 在 Passed 之前裁决——NeedsHuman 优先于 done/反馈。注意：worktree 不丢弃（park 保留）。
		if res.NeedsHuman {
			return sl.report(ctx, taskID, task, "needs-review", res.Detail), nil
		}
		if res.Passed {
			return sl.report(ctx, taskID, task, "done", res.Detail), nil
		}
		// 不过 → 反馈，下一轮重试
		priorFailure = res.Detail
		isolation.Discard(sl.Repo, wt)
	}
	return sl.report(ctx, taskID, task, "blocked", "retries exhausted: "+priorFailure), nil
}

// criteriaBlock formats acceptance criteria into a bullet list string.
// Returns "(未提供)" when the list is empty.
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

// report writes the terminal state transition + channel battle report for a
// terminal outcome (done or blocked) and returns the corresponding Outcome.
// Per spec §7.2d / §14, writeback happens on EVERY terminal outcome — not just
// blocked. Writeback has three legs: state transition, channel comment, and a
// channel status mark (Local: status/<ref>; GitHub: loop:<status> label).
//
// Writeback errors (state DB write, channel comment post, status mark) are
// surfaced rather than swallowed: each is logged to os.Stderr AND folded into
// the returned Detail so callers/tests can see partial writeback. The Outcome
// status itself is unchanged — a writeback failure does not flip a done to a
// blocked.
func (sl *SubLoop) report(ctx context.Context, taskID string, task channel.Task, status, detail string) Outcome {
	if err := sl.Store.AppendTransition(taskID, "running", status, detail); err != nil {
		fmt.Fprintf(os.Stderr, "writeback error: AppendTransition failed: %v\n", err)
		detail += " [writeback partial: transition: " + err.Error() + "]"
	}
	if err := sl.Channel.PostComment(ctx, task.Ref, strings.ToUpper(status)+": "+detail); err != nil {
		fmt.Fprintf(os.Stderr, "writeback error: PostComment failed: %v\n", err)
		detail += " [writeback partial: comment: " + err.Error() + "]"
	}
	// Mark the ticket's status: Local writes status/<ref>; GitHub adds a
	// loop:<status> label. Same error contract as PostComment — surface to
	// stderr + fold into Detail, but never flip the outcome status.
	if err := sl.Channel.UpdateStatus(ctx, task.Ref, status); err != nil {
		fmt.Fprintf(os.Stderr, "writeback error: UpdateStatus failed: %v\n", err)
		detail += " [writeback partial: status: " + err.Error() + "]"
	}
	return Outcome{Status: status, Detail: detail}
}

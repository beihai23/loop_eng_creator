// Package loop implements the per-task control loop (SubLoop): plan → execute
// (in a fresh worktree) → verify (Chain of tiers) → writeback (state trace +
// channel report), with bounded retries governed by the budget Enforcer.
package loop

import (
	"context"
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
	Repo    string
	Store   *state.Store
	Budget  *budget.Enforcer
	Execute model.Client
	Plan    skill.Skill[skill.PlanInput, skill.PlanOutput]
	Tiers   []verify.Tier // 完整链：tier1[], tier2, tier3
	Channel channel.Channel
}

// Run executes the plan→execute→verify→writeback loop for one task.
//
// M1 success = done: a passed verify.Chain → done, regardless of NeedsHuman
// (tier3 HumanStub returns NeedsHuman=true; parking is M3). Every terminal
// outcome writes a state transition and a channel battle report.
func (sl *SubLoop) Run(ctx context.Context, task channel.Task) (Outcome, error) {
	taskID, err := sl.Store.InsertTask(state.TaskRow{
		IssueRef: task.Ref, Description: task.Description,
		TaskType: task.TaskType, Source: "local", Criteria: task.AcceptanceCriteria,
	})
	if err != nil {
		return Outcome{Status: "error"}, err
	}
	sl.Store.AppendTransition(taskID, "", "running", "dispatched")

	priorFailure := ""
	for attempt := 1; sl.Budget.ShouldRetry(attempt); attempt++ {
		// ---- plan ----
		if err := sl.Budget.BeforeCall(1000); err != nil {
			return sl.report(ctx, taskID, task, "blocked", "budget: "+err.Error()), nil
		}
		_, u, err := sl.Plan.Run(ctx, skill.PlanInput{
			Task: task.Description, AcceptanceCriteria: task.AcceptanceCriteria,
			BattleReport: priorFailure,
		})
		sl.Budget.AfterCall(u)
		sl.Store.AppendStep(state.StepRow{RunID: taskID, Seq: attempt*10 + 1, Role: "plan", Status: statusOf(err), Error: errStr(err)})
		if err != nil {
			priorFailure = "plan error: " + err.Error()
			continue
		}

		// ---- execute (in a fresh worktree) ----
		wt, err := isolation.Create(sl.Repo, taskID+"-r"+fmt.Sprint(attempt))
		if err != nil {
			return Outcome{Status: "error", Detail: err.Error()}, err
		}
		if err := sl.Budget.BeforeCall(1000); err != nil {
			isolation.Discard(sl.Repo, wt)
			return sl.report(ctx, taskID, task, "blocked", "budget: "+err.Error()), nil
		}
		execOut, u2, err := sl.Execute.Call(ctx, "EXECUTE: "+task.Description+" @ "+wt)
		sl.Budget.AfterCall(u2)
		_ = execOut
		if err != nil {
			isolation.Discard(sl.Repo, wt)
			priorFailure = "execute error: " + err.Error()
			sl.Store.AppendStep(state.StepRow{RunID: taskID, Seq: attempt*10 + 2, Role: "execute", Status: "fail", Error: err.Error()})
			continue
		}
		diff := worktreeDiff(sl.Repo, wt)
		sl.Store.AppendStep(state.StepRow{RunID: taskID, Seq: attempt*10 + 2, Role: "execute", Status: "ok", OutputJSON: diff})

		// ---- verify (Chain of tiers; independent judgment) ----
		res, err := verify.Chain(ctx, sl.Tiers, diff, task.AcceptanceCriteria, priorFailure)
		// M3: NeedsHuman → park via daemon. In M1 NeedsHuman must NOT change the
		// outcome; record it into the verify trace for forward-reference only.
		sl.Store.AppendStep(state.StepRow{
			RunID: taskID, Seq: attempt*10 + 3, Role: "verify",
			Status:     statusOf2(res.Passed),
			OutputJSON: fmt.Sprintf("passed=%v needs_human=%v detail=%s", res.Passed, res.NeedsHuman, res.Detail),
		})
		if err != nil {
			isolation.Discard(sl.Repo, wt)
			return Outcome{Status: "error", Detail: err.Error()}, err
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

// report writes the terminal state transition + channel battle report for a
// terminal outcome (done or blocked) and returns the corresponding Outcome.
// Per spec §7.2d / §14, writeback happens on EVERY terminal outcome — not just
// blocked.
//
// Writeback errors (state DB write, channel comment post) are surfaced rather
// than swallowed: each is logged to os.Stderr AND folded into the returned
// Detail so callers/tests can see partial writeback. The Outcome status itself
// is unchanged — a writeback failure does not flip a done to a blocked.
func (sl *SubLoop) report(ctx context.Context, taskID string, task channel.Task, status, detail string) Outcome {
	if err := sl.Store.AppendTransition(taskID, "running", status, detail); err != nil {
		fmt.Fprintf(os.Stderr, "writeback error: AppendTransition failed: %v\n", err)
		detail += " [writeback partial: transition: " + err.Error() + "]"
	}
	if err := sl.Channel.PostComment(ctx, task.Ref, strings.ToUpper(status)+": "+detail); err != nil {
		fmt.Fprintf(os.Stderr, "writeback error: PostComment failed: %v\n", err)
		detail += " [writeback partial: comment: " + err.Error() + "]"
	}
	return Outcome{Status: status, Detail: detail}
}

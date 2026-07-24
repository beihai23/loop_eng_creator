// Package loop implements the per-task control loop (SubLoop): plan → execute
// (in a fresh worktree) → verify (Chain of tiers) → writeback (state trace +
// channel report), with bounded retries governed by the budget Enforcer.
package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

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
// needs-review is M3 machinery). On done, Worktree+Branch identify where the
// committed work lives so the caller can land it on main (the execute model is
// forbidden from committing, so SubLoop captures the diff on the worktree's
// branch and hands the branch off).
type Outcome struct {
	Status   string // done | blocked | needs-info | needs-review
	Detail   string
	Worktree string // done only: absolute path to the worktree holding the committed work
	Branch   string // done only: branch holding that commit; caller FF-merges to main then cleans up
}

// SubLoop drives a single task through plan→execute→verify→writeback, retrying
// on verify failure up to Budget.MaxRetries. Each attempt executes in a fresh
// worktree; a non-passing attempt discards that worktree.
type SubLoop struct {
	Repo              string
	Store             *state.Store
	Budget            *budget.Enforcer
	Execute           model.Executer
	Plan              skill.Skill[skill.PlanInput, skill.PlanOutput]
	VerifyLLM         verify.LLM  // tier2
	Tier3Human        bool        // tier3 开关：true 时挂 tier-3（HumanTier，否则回落 HumanStub）
	HumanTier         verify.Tier // M3 真 tier-3 人审 tier；nil 时回落 HumanStub（自动通过占位）
	Channel           channel.Channel
	PreinsertedTaskID string // daemon path: if set, skip InsertTask (task already ingested by daemon tick)

	// PlanModelRef / ExecuteModelRef / VerifyModelRef carry each phase's provider
	// label ("who ran this step") into steps.model_ref — the per-step audit column
	// that always existed but was never populated. Optional (zero value "" =
	// legacy empty model_ref); set by run-once/daemon from the effective config
	// (incl. task-level agent override) so dashboard/replay show the agent per
	// step. subloop_test.go does not reference these — zero value = old behavior.
	PlanModelRef    string
	ExecuteModelRef string
	VerifyModelRef  string

	// Log is the observability sink for phase start/done, retry, and budget
	// events. When nil, defaults to os.Stderr with a "[subloop]" prefix. Tests
	// inject a logger backed by a bytes.Buffer to assert on log output without
	// scraping stderr.
	Log *log.Logger

	// RetryBackoff is the backoff per attempt before re-running after a verify
	// failure. Zero (the default) means no sleep — useful in tests. In
	// production a small backoff (e.g. 1s × attempt) spaces retries so the
	// daemon's self-healing logs have observable gaps.
	RetryBackoff time.Duration
}

// logf writes a formatted line to the SubLoop log (or stderr if Log is nil).
func (sl *SubLoop) logf(format string, args ...interface{}) {
	if sl.Log != nil {
		sl.Log.Printf(format, args...)
	} else {
		fmt.Fprintf(os.Stderr, format+"\n", args...)
	}
}

// shortID returns a truncated task ID for log lines (first 12 chars).
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// tiersFor 在每轮按 worktree + plan 产出重建 tier 链：tier1（plan 产出的验收脚本，
// 在 wt 里跑）→ tier2 → tier3。
//
// tier-1 完全来自 plan（planOut.VerifyScript），无任何静态/兜底列表：
//   - plan 产出且 Valid（非空、有运行命令）→ 挂 tier-1（Dir=wt，脚本 body 先落盘）。
//   - plan 未产出（VerifyScript=nil）→ tier-1 缺席，链直接落 tier-2。
//   - plan 产出了但非法（缺运行命令等）→ Run 已记一行，这里同样跳过，落 tier-2。
func (sl *SubLoop) tiersFor(wt string, planOut skill.PlanOutput) []verify.Tier {
	var tiers []verify.Tier
	if s := planOut.VerifyScript; s.Valid() {
		tiers = append(tiers, verify.Deterministic{
			Label:      labelOf(s),
			Cmd:        s.Run,
			Dir:        wt,
			ScriptFile: s.File,
			ScriptBody: s.Body,
		})
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

// labelOf picks the tier-1 observability tag from a plan verify script, falling
// back to the run command (or "tier-1") when the planner left Label blank.
func labelOf(s *skill.PlanVerifyScript) string {
	if s.Label != "" {
		return s.Label
	}
	if len(s.Run) > 0 {
		return strings.Join(s.Run, " ")
	}
	return "tier-1"
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
func (sl *SubLoop) Run(ctx context.Context, task channel.Task) (out Outcome, err error) {
	var taskID string
	if sl.PreinsertedTaskID != "" {
		taskID = sl.PreinsertedTaskID // daemon path: task already ingested by tick
	} else {
		var err error
		taskID, err = sl.Store.InsertTask(state.TaskRow{
			IssueRef: task.Ref, Description: task.Description,
			TaskType: task.TaskType, Source: "run-once", Criteria: task.AcceptanceCriteria,
			CreatedAt: task.CreatedAt,
		})
		if err != nil {
			return Outcome{Status: "error"}, err
		}
	}
	sl.Store.AppendTransition(taskID, "", "running", "dispatched")

	// →running 即在 channel 上标出「正在处理」（loop:running）。SubLoop.Run 是所有
	// 执行路径（daemon 派发 + cli run-once）的漏斗，在这里打标覆盖 run-once——它
	// 绕过 daemon，daemon 派发点的打标不会触发。daemon 路径下这会与派发点的打标
	// 重复，但幂等（标签互斥后只是 no-op 的 remove+add）。失败只记日志、不翻转
	// 任务结局（与 report 的写回容错一致）。
	if sl.Channel != nil {
		if err := sl.Channel.UpdateStatus(ctx, task.Ref, "running"); err != nil {
			sl.logf("[subloop] %s running mark failed: %v", shortID(taskID), err)
		}
	}

	// 开一行 run（spec §4.1）：每次 Run 用 StartRun 拿真 runID 透传给 steps/budget。
	// 修 resume 后 replay 交错 bug——修前 run_id 都是 taskID，同任务两次 run 的 step seq
	// 撞车后 Replay 串在一起。defer 引用命名返回 out：每条终态路径都自动 EndRun，
	// out.Status 即结局（done/blocked/needs-review/error），无需改现有 return。
	runID, rerr := sl.Store.StartRun(taskID)
	if rerr != nil {
		_ = sl.Store.ClearInFlight()
		return Outcome{Status: "error"}, rerr
	}
	defer func() { _ = sl.Store.EndRun(runID, out.Status) }()

	// Mark the active slot in the cross-process in_flight table.
	_ = sl.Store.SetInFlight(taskID, "starting")
	sid := shortID(taskID)

	// 每次运行（不论触发原因：首次 / reopen / resume / 重排队）都收集 issue 的全部
	// 评论作为「战报」上下文喂给 plan —— 修「reopen 写的反馈 plan 看不到」的 bug。
	issueContext := sl.collectIssueComments(ctx, task.Ref)
	priorFailure := ""
	if fb, err := sl.Store.PopResumeFeedback(taskID); err == nil && fb != "" {
		priorFailure = fb
	}
	// lastCompileError 携带「上一轮 verify 驳回若是编译/构建类错误」的原始 detail，作为
	// 结构化的一手信号喂给下一轮 execute prompt 的独立显眼段（#46）：编译错误原本要绕
	// verify detail → issue 评论 → collectIssueComments → 战报散文 才到 execute，信号被
	// 稀释到 execute 连续多轮不修。空串表示上一轮无编译错误（首次或语义驳回）。
	lastCompileError := ""
	for attempt := 1; sl.Budget.ShouldRetry(attempt); attempt++ {
		// 协作式 cancel：phase 边界自查（spec §4.5/§7）。命中则提前以 cancelled 收尾。
		if ok, _ := sl.Store.CancelRequested(taskID); ok {
			sl.logf("[subloop] %s cancelled by TUI at phase boundary", sid)
			_ = sl.Store.ClearInFlight()
			return sl.report(ctx, taskID, task, "cancelled", "cancelled by TUI"), nil
		}
		// 预算刹车·重试：每轮入口记一行（spec §8.8）
		sl.Store.AppendBudget(runID, "task", "retry", attempt, sl.Budget.MaxRetries)

		// ---- plan ----
		sl.logf("[subloop] %s phase=plan start", sid)
		_ = sl.Store.SetInFlight(taskID, "plan")
		if err := sl.Budget.BeforeCall(planExecEstimate); err != nil {
			_ = sl.Store.ClearInFlight()
			return sl.report(ctx, taskID, task, "blocked", "budget: "+err.Error()), nil
		}
		// 预算刹车·每调用 token：plan 模型调用前记一行（spec §8.8）
		sl.Store.AppendBudget(runID, "call", "tokens", planExecEstimate, sl.Budget.PerCall)
		planIn := skill.PlanInput{
			Task: task.Description, AcceptanceCriteria: task.AcceptanceCriteria,
			BattleReport: joinNonEmpty(issueContext, priorFailure),
			Body:         task.Body, // 全文保留：issue 原文（背景/约束）也喂给 plan
			// 重试诊断（attempt≥2 + 当轮 priorFailure 非空时注入）：引用当轮驳回原文，
			// 要求 plan 诊断 loop 数据流的结构性不可满足、行使 revised_criteria 把证据要求
			// 翻译成 tier-1 可机械判定的退出码/编译期判据。attempt=1 或无 priorFailure 时为空串，
			// 不干扰首次规划。承载在独立字段（不进 BattleReport）：这是「如何规划」的元指令，
			// 与「发生了什么」的战报分离——经 plan embed 的 {{.RetryDiagnosis}} 条件块渲染进
			// planPrompt，落进 plan step 的 input_json（attempt≥2 的 seq≥20 行）可审计。
			RetryDiagnosis: retryDiagnosisFor(attempt, priorFailure),
		}
		// 把喂给 plan 的原始提示词落进 step trace（input_json）——dashboard 详情页
		// 的「初始提示词」读它。RenderPrompt 与 Plan.Run 内部渲染同一模板+输入，
		// 文本一致；渲染失败（模板错）时 Plan.Run 同样会报 render 错，这里留空即可。
		planPrompt, _ := skill.RenderPrompt(sl.Plan.PromptTmpl, planIn)
		planOut, u, err := sl.Plan.Run(ctx, planIn)
		sl.Budget.AfterCall(u)
		// 空 plan 防护（plan-execute-contract-drift）：plan 调用成功但产出空计划
		// （Plan nil 或 len 0，即 `{"plan":null}` / `{"plan":[]}`）= 模型摆烂，视为
		// 可重试失败——不进 execute（否则 execute 只能靠战报上下文瞎续，浪费整轮
		// plan→execute→verify）。与 plan error 路径同构：记 plan step status=fail、设
		// priorFailure、continue 重试，MaxRetries 耗尽 → blocked（detail 含「plan 产出空计划」）。
		emptyPlan := err == nil && len(planOut.Plan) == 0
		// plan 产出也落 trace（output_json）：含 plan 步骤、verify_script 及
		// 修订后的验收标准——修订权的审计轨迹（dashboard 详情页/人审可查）。
		// 空计划也落 output_json（plan 到底返回了啥可审计），只把 step 的 status/error 标 fail。
		var planOutJSON string
		if err == nil {
			if jb, mErr := json.Marshal(planOut); mErr == nil {
				planOutJSON = string(jb)
			}
		}
		// step 的 status/error：调用级 err 优先；调用成功但空计划记 fail + "empty plan"。
		planStatus, planStepErr := statusOf(err), errStr(err)
		if emptyPlan {
			planStatus, planStepErr = "fail", "empty plan"
		}
		sl.Store.AppendStep(state.StepRow{RunID: runID, Seq: attempt*10 + 1, Role: "plan", Status: planStatus, ModelRef: sl.PlanModelRef, InputJSON: planPrompt, OutputJSON: planOutJSON, Error: planStepErr})
		if err != nil {
			sl.logf("[subloop] %s phase=plan fail: %v", sid, err)
			if errors.Is(err, model.ErrClaudeFatal) {
				_ = sl.Store.ClearInFlight()
				return sl.report(ctx, taskID, task, "blocked", "fatal model error: "+err.Error()), nil
			}
			priorFailure = "plan error: " + err.Error()
			sl.logRetry(sid, attempt, priorFailure)
			continue
		}
		if emptyPlan {
			// priorFailure 用中文锚点「plan 产出空计划」：MaxRetries 耗尽时 blocked
			// detail = "retries exhausted: " + priorFailure，故含此字样；下一轮 plan 的
			// 重试诊断也会逐字引用它，提示「上一轮你给了空计划」。
			sl.logf("[subloop] %s phase=plan fail: plan 产出空计划 (empty plan)", sid)
			priorFailure = "plan 产出空计划"
			sl.logRetry(sid, attempt, priorFailure)
			continue
		}
		sl.logf("[subloop] %s phase=plan done", sid)
		// plan 产出验收脚本但非法（缺运行命令等）→ tier-1 缺席，落 tier-2。记一行可观测。
		// plan 未产出是正常分支（判定不可脚本化 → tier-2），不算异常，不打 warning。
		if vs := planOut.VerifyScript; vs != nil && !vs.Valid() {
			sl.logf("[subloop] %s plan verify_script invalid (run command missing?) — skip tier-1, fall to tier-2", sid)
		}

		// 有效验收标准：plan 行使评审权（RevisedCriteria 非 nil）时以 plan 承诺的
		// 版本为准——execute 按它实现、verify 按它判；未修订（nil）沿用 issue 原版。
		// effTask 仅替换标准（verifyFailComment 的签名被测试钉死，从调用点喂修订版）。
		effTask := task
		criteriaRevised := planOut.RevisedCriteria != nil
		if criteriaRevised {
			effTask.AcceptanceCriteria = *planOut.RevisedCriteria
			sl.logf("[subloop] %s plan revised acceptance criteria: %d → %d 条 (%s)",
				sid, len(task.AcceptanceCriteria), len(*planOut.RevisedCriteria), truncateStr(planOut.CriteriaNotes, 80))
		}

		// 协作式 cancel：phase 边界自查（spec §4.5/§7）。命中则提前以 cancelled 收尾。
		if ok, _ := sl.Store.CancelRequested(taskID); ok {
			sl.logf("[subloop] %s cancelled by TUI at phase boundary", sid)
			_ = sl.Store.ClearInFlight()
			return sl.report(ctx, taskID, task, "cancelled", "cancelled by TUI"), nil
		}

		// ---- execute (in a fresh worktree) ----
		sl.logf("[subloop] %s phase=execute start", sid)
		_ = sl.Store.SetInFlight(taskID, "execute")
		wt, err := isolation.Create(sl.Repo, taskID+"-r"+fmt.Sprint(attempt))
		if err != nil {
			_ = sl.Store.ClearInFlight()
			return Outcome{Status: "error", Detail: err.Error()}, err
		}
		if err := sl.Budget.BeforeCall(planExecEstimate); err != nil {
			isolation.Discard(sl.Repo, wt)
			_ = sl.Store.ClearInFlight()
			return sl.report(ctx, taskID, task, "blocked", "budget: "+err.Error()), nil
		}
		// 预算刹车·每调用 token：execute 模型调用前记一行（spec §8.8）
		sl.Store.AppendBudget(runID, "call", "tokens", planExecEstimate, sl.Budget.PerCall)
		execPrompt := "EXECUTE: 你在一个 git worktree 里（当前工作目录即工作区）。\n" +
			"任务: " + task.Description + "\n" +
			"验收标准:\n" + criteriaBlock(effTask.AcceptanceCriteria) + "\n"
		// plan 修订过标准时把修订理由也告诉 execute——它实现的是 plan 承诺的合同，
		// 知道「为什么改」才能不在被删除/改写的条款上浪费力气或自作主张补回。
		if criteriaRevised && planOut.CriteriaNotes != "" {
			execPrompt += "（以上验收标准经 plan 评审修订：" + planOut.CriteriaNotes + "）\n"
		}
		// 编译错误特化（#46）：上一轮 verify 驳回若是编译/构建类错误，作为独立且显眼的段
		// 直达 execute prompt——不埋进下面的战报散文（issue 评论）。这是确定性、可机械判定
		// 的杠杆：字符串特征命中 + prompt 拼装，直接命中 #46「execute 连续多轮不修编译错误」
		// 的失败模式。非编译错误（如 tier-2 语义驳回）compileErrorSection 返回空串，不触发。
		if sec := compileErrorSection(lastCompileError); sec != "" {
			execPrompt += sec
		}
		// 全文保留：issue 原文（背景/约束/上下文）也喂给 execute——「任务」行只是
		// 首行蒸馏，实现细节往往藏在正文其余段落里。
		if task.Body != "" {
			execPrompt += "Issue 全文（背景/约束，实现时以全文为准）:\n" + task.Body + "\n"
		}
		// 战报/反馈也喂给 execute（不只是 plan）：否则 execute 只对照验收标准、看不见
		// issue 里的反馈，对「已实现但需按反馈精修」的任务会反复产出空 diff（#20 即此）。
		if issueContext != "" {
			execPrompt += "战报/反馈（issue 评论，含历轮驳回与人审意见）——务必据此修正代码，" +
				"不要只对照验收标准就说「已完成」:\n" + issueContext + "\n"
		}
		execPrompt += "上下文：若任务/issue 引用了设计文档或 spec，开工前先读相关章节；也可浏览仓库的 README/docs 了解项目约定与冻结接口，再动手。\n" +
			"在当前目录实现任务，确保满足全部验收标准（若项目有测试，确保测试通过）。\n" +
			"注意：不要执行 git add / git commit —— 只修改或创建文件；loop-eng 会自动捕获你的改动生成 diff。"
		execOut, u2, err := sl.Execute.Exec(ctx, wt, execPrompt)
		sl.Budget.AfterCall(u2)
		if err != nil {
			isolation.Discard(sl.Repo, wt)
			sl.logf("[subloop] %s phase=execute fail: %v", sid, err)
			if errors.Is(err, model.ErrClaudeFatal) {
				_ = sl.Store.ClearInFlight()
				return sl.report(ctx, taskID, task, "blocked", "fatal model error: "+err.Error()), nil
			}
			priorFailure = "execute error: " + err.Error()
			sl.Store.AppendStep(state.StepRow{RunID: runID, Seq: attempt*10 + 2, Role: "execute", Status: "fail", ModelRef: sl.ExecuteModelRef, InputJSON: execPrompt, Error: err.Error()})
			sl.logRetry(sid, attempt, priorFailure)
			continue
		}
		diff := worktreeDiff(sl.Repo, wt)
		// 捕获 execute 的完整 I/O 进 trace：之前 execOut 被 `_ = execOut` 丢弃，
		// 导致「空 diff」时无从诊断 claude 到底返回了啥、为什么没改文件。
		// InputJSON=execute prompt；OutputJSON={out: 模型输出, diff: 捕获的改动}。
		rec, _ := json.Marshal(struct {
			Out  string `json:"out"`
			Diff string `json:"diff"`
		}{Out: execOut, Diff: diff})
		sl.Store.AppendStep(state.StepRow{RunID: runID, Seq: attempt*10 + 2, Role: "execute", Status: "ok", ModelRef: sl.ExecuteModelRef, InputJSON: execPrompt, OutputJSON: string(rec)})
		sl.logf("[subloop] %s phase=execute done", sid)

		// 协作式 cancel：phase 边界自查（spec §4.5/§7）。命中则提前以 cancelled 收尾。
		if ok, _ := sl.Store.CancelRequested(taskID); ok {
			sl.logf("[subloop] %s cancelled by TUI at phase boundary", sid)
			_ = sl.Store.ClearInFlight()
			return sl.report(ctx, taskID, task, "cancelled", "cancelled by TUI"), nil
		}

		// ---- verify (Chain of tiers; independent judgment) ----
		sl.logf("[subloop] %s phase=verify start", sid)
		_ = sl.Store.SetInFlight(taskID, "verify")
		res, err := verify.Chain(ctx, sl.tiersFor(wt, planOut), diff, effTask.AcceptanceCriteria, priorFailure)
		// NeedsHuman（tier-3 人审信号）记录进 verify trace；下面在 Passed 之前优先裁决。
		// verifyTrace 写成结构化 JSON：驳回时 Detail 由 verify.detailFor 兜底永不空，
		// 且 failing_criteria 随行落库——修 #10 黑箱（旧 trace 只剩空的 detail=）。
		vt, _ := json.Marshal(verifyTrace{
			Passed:          res.Passed,
			NeedsHuman:      res.NeedsHuman,
			Detail:          res.Detail,
			FailingCriteria: res.FailingCriteria,
		})
		sl.Store.AppendStep(state.StepRow{
			RunID: runID, Seq: attempt*10 + 3, Role: "verify",
			Status:     statusOf2(res.Passed),
			ModelRef:   sl.VerifyModelRef,
			OutputJSON: string(vt),
		})
		// 逐 tier 落盘 verifications（spec §4.6）：best-effort，trace 不 gate loop。
		// runID 来自 Task 2 的 StartRun 透传；短路时只落实际跑过的 tier。
		for _, to := range res.Tiers {
			_ = sl.Store.AppendVerification(runID, to.Tier, to.Passed, to.Detail)
		}
		if err != nil {
			// verify 基础设施错误：致命（auth）→ 立刻中断；可重试 flake → 当作可重试失败。
			isolation.Discard(sl.Repo, wt)
			sl.logf("[subloop] %s phase=verify error: %v", sid, err)
			if errors.Is(err, model.ErrClaudeFatal) {
				_ = sl.Store.ClearInFlight()
				return sl.report(ctx, taskID, task, "blocked", "fatal model error: "+err.Error()), nil
			}
			priorFailure = "verify error: " + err.Error()
			sl.logRetry(sid, attempt, priorFailure)
			continue
		}
		// tier-3 人审 → needs-review：park、释放活跃位，worktree 保留待 daemon 恢复（spec §7.2c/§8.6/§10）。
		// 在 Passed 之前裁决——NeedsHuman 优先于 done/反馈。注意：worktree 不丢弃（park 保留）。
		if res.NeedsHuman {
			sl.logf("[subloop] %s phase=verify done needs-human", sid)
			_ = sl.Store.ClearInFlight()
			return sl.report(ctx, taskID, task, "needs-review", res.Detail), nil
		}
		if res.Passed {
			sl.logf("[subloop] %s phase=verify done passed", sid)
			// Capture the execute output on the worktree's branch, then hand the
			// branch to the caller for landing. The execute prompt forbids the
			// model from committing, so loop-eng commits the work itself; the
			// caller (daemon/run-once) FF-merges this branch to main + cleans up.
			// Closes the done-worktree-never-landed gap (lost #12/#14): a Passed
			// verify used to leave the worktree uncommitted → diff vanished.
			branch := branchName(taskID, attempt)
			if cerr := commitWorktree(wt, branch, landCommitMessage(task, taskID)); cerr != nil {
				_ = sl.Store.ClearInFlight()
				// commit failed — verify still passed, so this stays done; but landing
				// is now manual. Leave the worktree (uncommitted changes survive on
				// disk) and surface in Detail so the operator can salvage by hand.
				return sl.report(ctx, taskID, task, "done", res.Detail+
					" [land: worktree commit failed: "+cerr.Error()+"; worktree preserved at "+wt+"]"), nil
			}
			_ = sl.Store.ClearInFlight()
			// plan 修订过验收标准时，在 done 战报里留人可见的审计线索（tier-3 人审
			// 与 issue 读者能看到「按修订版判过」及理由）。
			doneDetail := res.Detail
			if criteriaRevised {
				doneDetail += "\n（验收标准经 plan 评审修订：" + planOut.CriteriaNotes + "）"
			}
			out := sl.report(ctx, taskID, task, "done", doneDetail)
			out.Worktree = wt
			out.Branch = branch
			return out, nil
		}
		// 不过 → 反馈，下一轮重试
		sl.logf("[subloop] %s phase=verify done rejected: %s", sid, res.Detail)
		// verify 不过必给「失败理由 + 改进建议」，落进 issue 评论：给人看（驳回不再黑箱）
		// + 作下一轮持久反馈（collectIssueComments 下一轮读回喂 plan/execute）。best-effort——
		// 发评论失败只记日志、不 gate loop（与 report 的 PostComment 容错一致）。
		if cErr := sl.Channel.PostComment(ctx, task.Ref, verifyFailComment(effTask, attempt, res)); cErr != nil {
			sl.logf("[subloop] %s verify-fail comment post failed: %v", sid, cErr)
		}
		// 编译错误信号更新（#46）：本轮驳回是编译/构建类 → 把原始 detail 存为下一轮 execute
		// 的结构化一手信号；非编译错误（如 tier-2 语义驳回）→ 清空，避免上一轮已修好的编译
		// 错误作为陈旧信号残留、误导下一轮 execute。lastCompileError 永远反映「最近一轮驳回
		// 是否为编译错误」。
		if looksLikeCompileError(res.Detail) {
			lastCompileError = res.Detail
		} else {
			lastCompileError = ""
		}
		priorFailure = res.Detail
		isolation.Discard(sl.Repo, wt)
		sl.logRetry(sid, attempt, priorFailure)
	}
	_ = sl.Store.ClearInFlight()
	return sl.report(ctx, taskID, task, "blocked", "retries exhausted: "+priorFailure), nil
}

// logRetry logs a retry event and sleeps the backoff. The backoff is
// attempt × RetryBackoff (zero RetryBackoff = no sleep, test-friendly).
func (sl *SubLoop) logRetry(sid string, attempt int, reason string) {
	backoff := time.Duration(attempt) * sl.RetryBackoff
	sl.logf("[subloop] %s retry attempt=%d reason=%q backoff=%s",
		sid, attempt, truncateStr(reason, 80), backoff)
	if backoff > 0 {
		time.Sleep(backoff)
	}
}

// truncateStr returns s cut to at most n runes with "…" appended.
func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
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

// verifyTrace is the structured record SubLoop writes to the verify step's
// OutputJSON (state.steps.output_json). Structured JSON — not a free-form
// "k=v" line — so the trace is greppable and the failing_criteria list survives
// intact. This closes the #10 observability hole: a bare "detail=" was empty
// whenever the LLM left its reason blank, making rejections a black box. Detail
// is now never empty on rejection (verify.detailFor), and failing_criteria is
// carried alongside so every rejection is explainable and feeds the next retry.
type verifyTrace struct {
	Passed          bool     `json:"passed"`
	NeedsHuman      bool     `json:"needs_human"`
	Detail          string   `json:"detail"`
	FailingCriteria []string `json:"failing_criteria,omitempty"`
}

// collectIssueComments 拉取 issue/ticket 的全部评论（人审反馈 + 历轮战报）作为 plan
// 的上下文。每次 Run 都调——不论触发原因（修「reopen 写的反馈 plan 看不到」）：
// reopen 走 reconcile 只翻状态、不读评论，导致 plan 拿不到人在 issue 里写的反馈。
// since 为零值表示「全部评论」。读失败非致命（plan 只是少了上下文，不致崩）。
func (sl *SubLoop) collectIssueComments(ctx context.Context, ref string) string {
	if sl.Channel == nil {
		return ""
	}
	replies, err := sl.Channel.ListReplies(ctx, []string{ref}, time.Time{})
	if err != nil {
		sl.logf("[subloop] collect comments: ListReplies(%s) failed: %v", ref, err)
		return ""
	}
	parts := make([]string, 0, len(replies[ref]))
	for _, r := range replies[ref] {
		if s := strings.TrimSpace(r.Body); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "\n---\n")
}

// joinNonEmpty 用双换行拼接非空片段（issue 战报 + 当轮失败/反馈），喂给 plan 的
// BattleReport。空片段跳过，避免前导空行。
func joinNonEmpty(parts ...string) string {
	var out []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "\n\n")
}

// retryDiagnosisFor builds the retry-diagnosis meta-instruction SubLoop injects
// into PlanInput on retries. It is the automation of the human-written diagnosis
// that #47's second run proved is the actual trigger for plan exercising
// revised_criteria: a vague "you may revise criteria" nudge produced 0 revisions
// across 3 rounds, while a concrete unsatisfiability diagnosis + revision
// direction produced a (higher-quality) revision that passed first try.
//
// Gating (deliberate, never relax): injected ONLY when attempt ≥ 2 AND this
// round's priorFailure is non-empty. attempt=1 has nothing failed yet to
// diagnose, so the first plan is left undisturbed. attempt ≥ 2 with an empty
// priorFailure is a defensive clause — a retry only fires after a failure, so
// priorFailure is in practice always non-empty by attempt ≥ 2.
//
// The instruction is a "how to plan" meta-directive, carried in its own
// PlanInput.RetryDiagnosis field — NOT folded into BattleReport. BattleReport is
// "what happened" (history/context); this is "how to plan" (meta). Mixing them
// would make it hard for plan to tell a past battle log apart from a directive.
//
// Returned text is rendered verbatim by the plan embed's {{.RetryDiagnosis}}
// conditional block, so it lands in the plan step's input_json (steps table,
// seq ≥ 20 for attempt ≥ 2) — auditable in the dashboard detail / Replay. By
// design the dashboard's "初始提示词" panel reads the first round (attempt=1,
// seq LIMIT 1), where this is empty, so the diagnosis is not shown there; it is
// auditable in attempt ≥ 2 plan steps + Replay.
func retryDiagnosisFor(attempt int, priorFailure string) string {
	if attempt < 2 || strings.TrimSpace(priorFailure) == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("## 重试诊断（attempt=" + fmt.Sprint(attempt) + " 的元指令：如何规划，不是战报）\n\n")
	b.WriteString("本轮 priorFailure 原文（逐字引用，请据此诊断）：\n")
	b.WriteString("<<<\n" + priorFailure + "\n>>>\n\n")
	b.WriteString("诊断要求：判断上述驳回理由是否属于 loop 数据流的**结构性不可满足**——即证据要求本身" +
		"在 loop 的数据流里拿不到，与实现质量无关。典型形态：\n")
	b.WriteString("- 要求 diff/battle-report 附某条命令的输出，但 execute 在 worktree 里跑，其 stdout 不进战报" +
		"（战报只有 diff + verify 判定），这条标准对 loop 永远不可满足。\n")
	b.WriteString("- 要求 diff 出现某文件，而调用面已兼容该场景、合法地无需改动（空 diff 即正确）。\n")
	b.WriteString("- 要求出现某运行时产物，而该产物只在 execute 沙箱内短暂存在、不落进可验收的 diff。\n\n")
	b.WriteString("若判定属于结构性不可满足：行使 **revised_criteria**，把那条「拿不到的证据要求」翻译成" +
		"tier-1 可机械判定的退出码/编译期判据（例如把「报告附 build 输出」改写为「退出码 0 = 编译通过」、" +
		"或用编译期钉子 `var _ T = expr` 锁定签名），使验收能真正在 worktree 里判定通过与否。\n")
	b.WriteString("若不属于结构性不可满足（驳回指向真实未完成的实现）→ **不要**修订标准，按 priorFailure 修正实现。\n")
	return b.String()
}

// landCommitMessage builds the commit subject for a done task's auto-land: the
// issue ref + the first (truncated) line of the description. The execute model
// writes no commit message (it's forbidden from committing), so loop-eng
// synthesizes one that identifies the task in `git log`.
func landCommitMessage(task channel.Task, taskID string) string {
	_ = taskID
	subject := task.Description
	if i := strings.IndexByte(subject, '\n'); i >= 0 {
		subject = subject[:i]
	}
	if len(subject) > 72 {
		subject = subject[:72]
	}
	return "loop-eng auto-land #" + task.Ref + ": " + subject
}

// verifyFailComment 构造「verify 驳回」的 issue 评论正文：失败理由 + 改进建议。
//
// verify 不过（且非 needs-human）时落进 issue 评论——双重作用：
//   - 给人看：驳回不再是黑箱，每轮失败原因 + 该怎么改都可见（issue 侧的可观测性）。
//   - 作下一轮的持久反馈：collectIssueComments 下一轮（及 reopen/resume 后）读回，
//     喂给 plan/execute。in-memory 的 priorFailure 只活在一次 Run 内；落成评论后，
//     即便跨进程重启、跨 reopen，反馈也不丢。
//
// 纯函数（不碰 channel），便于直接单测正文；调用方负责 PostComment + best-effort 容错。
// 签名固定为 (channel.Task, int, verify.VerifyResult)：task 给验收标准（推导建议），
// attempt 标第几轮，res 给失败理由 + 未满足标准。测试必须与此签名对齐。
func verifyFailComment(task channel.Task, attempt int, res verify.VerifyResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## VERIFY-FAIL（第 %d 轮 verify 驳回）\n\n", attempt)

	b.WriteString("### 失败理由\n")
	if reason := strings.TrimSpace(res.Detail); reason != "" {
		b.WriteString(reason)
	} else {
		b.WriteString("（verify 驳回但未给出理由——请逐条复核验收标准）")
	}
	b.WriteString("\n\n")

	b.WriteString("### 改进建议\n")
	for _, s := range improvementSuggestions(task, res) {
		b.WriteString("- " + s + "\n")
	}
	return b.String()
}

// improvementSuggestions 由 verify 结果推导下一轮的可执行修正方向：
//   - 有 FailingCriteria → 每条未满足标准转成「针对性修正」（最准）。
//   - 否则回退到任务的验收标准，逐条提示复核（tier-1 这类不给 FailingCriteria 的驳回）。
//   - 两者皆空 → 兜底一条通用建议（引用失败理由，绝不交空的建议区）。
func improvementSuggestions(task channel.Task, res verify.VerifyResult) []string {
	var fc []string
	for _, c := range res.FailingCriteria {
		if s := strings.TrimSpace(c); s != "" {
			fc = append(fc, s)
		}
	}
	if len(fc) > 0 {
		out := make([]string, 0, len(fc))
		for _, c := range fc {
			out = append(out, "针对未满足标准修正："+c)
		}
		return out
	}
	var out []string
	for _, c := range task.AcceptanceCriteria {
		if s := strings.TrimSpace(c); s != "" {
			out = append(out, "复核验收标准是否满足："+s)
		}
	}
	if len(out) > 0 {
		return out
	}
	return []string{"对照上面的失败理由，逐条重做未满足的验收标准后再提交"}
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
	} else if err := sl.Store.SetLastCommentAt(taskID, time.Now()); err != nil {
		fmt.Fprintf(os.Stderr, "writeback error: SetLastCommentAt failed: %v\n", err)
		detail += " [writeback partial: last_comment_at: " + err.Error() + "]"
	}
	// Mark the ticket's status: Local writes status/<ref>; GitHub adds a
	// loop:<status> label. Same error contract as PostComment — surface to
	// stderr + fold into Detail, but never flip the outcome status.
	if err := sl.Channel.UpdateStatus(ctx, task.Ref, status); err != nil {
		fmt.Fprintf(os.Stderr, "writeback error: UpdateStatus failed: %v\n", err)
		detail += " [writeback partial: status: " + err.Error() + "]"
	}
	if status == "done" {
		if err := sl.Channel.CloseIssue(ctx, task.Ref); err != nil {
			fmt.Fprintf(os.Stderr, "writeback error: CloseIssue failed: %v\n", err)
			detail += " [writeback partial: close: " + err.Error() + "]"
		}
	}
	return Outcome{Status: status, Detail: detail}
}

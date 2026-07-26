// Package daemon implements the resident M3 control loop (Engine): a slow
// ticker that polls the ticket channel, ingests newly-arrived tasks into the
// durable FIFO, and drives a single active SubLoop to completion. The daemon
// never blocks on a human (spec principle 7): tier-3 human review parks the
// task, freeing the active slot, and is resumed on a later tick.
package daemon

import (
	"context"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"strings"
	"sync"
	"time"

	"loop-eng/internal/channel"
	"loop-eng/internal/skill"
	"loop-eng/internal/state"
)

// RunTaskFunc runs one task to completion and returns its terminal status
// (e.g. "done", "blocked") plus the outcome Detail (the failure reason on a
// block — used by the engine to classify transient-infra blocks for cooldown).
// It is injected so the daemon stays decoupled from the concrete SubLoop wiring.
// The daemon owns the task lifecycle bookkeeping (status transitions, spec §8.3
// step 3/4); RunTask just does the work and reports the outcome — it must NOT
// mutate task_status itself, so the daemon is the single writer of the lifecycle.
type RunTaskFunc func(ctx context.Context, task state.TaskRow) (status, detail string, err error)

// TriageFunc runs the triage gate on one task about to be dispatched and
// returns the triage verdict. Injected like RunTaskFunc so the Engine stays
// decoupled from model wiring (cli/daemon.go wires the triage skill). nil on
// the Engine means "no gate" (legacy behavior, used by tests).
type TriageFunc func(ctx context.Context, task state.TaskRow) (skill.TriageOutput, error)

// Engine is the resident M3 daemon. It polls Channel every Interval, ingesting
// new tasks into Store (the durable FIFO) deduped by issue_ref, resuming parked
// tasks whose human has replied, then dispatching the head of the FIFO by
// running it through RunTask. The active slot is implicit (single-active by
// construction — one synchronous RunTask per tick, spec §12) and the parked set
// is the durable query Store.ParkedTasks() (status=needs-review), so the Engine
// holds no task-lifecycle state of its own — it is all rebuildable from disk
// (principle 4).
type Engine struct {
	Channel  channel.Channel
	Store    *state.Store
	Interval time.Duration
	Cooldown time.Duration
	RunTask  RunTaskFunc

	// Triage is the dispatch gate: when non-nil, every FIFO head is triaged
	// before it occupies the active slot. !Startable → parked as needs-info
	// (missing info posted to the issue; a human reply re-queues it and the
	// next dispatch re-triages); NeedsHumanDecision or !LoopDoable → parked as
	// needs-human-decision; otherwise dispatched normally. A triage error does
	// NOT gate: it is logged and the task dispatches anyway — a broken triager
	// must not stall the whole FIFO.
	Triage TriageFunc

	// Log is the observability sink for daemon tick events (ingest, dispatch,
	// park, resume). When nil, defaults to os.Stderr with a "[daemon]" prefix.
	// Tests inject a logger backed by bytes.Buffer to assert on output.
	Log *log.Logger

	// IngestMin/IngestMax bound the background ingest loop's per-iteration
	// jittered sleep. When IngestMax > 0, Run starts a goroutine that keeps
	// ingesting newly-filed issues into the durable FIFO *independently* of the
	// synchronous main tick — so an issue filed while the single active slot is
	// busy running a task for minutes still lands in state.db and shows up on the
	// dashboard (the single-active sync-tick blind spot). When IngestMax <= 0
	// (the zero value), no background goroutine runs and the main tick alone
	// ingests (legacy behavior, used by tests that drive tick() directly).
	IngestMin time.Duration
	IngestMax time.Duration

	// GC is an optional per-tick hook for worktree scene garbage collection
	// (loop.GCWorktrees, wired by the CLI layer which owns the repo path). Nil =
	// no GC (tests). Errors are logged and never stall the tick — GC losing a
	// cycle just means scene caches live a little longer.
	GC func(ctx context.Context) error

	coolUntil time.Time

	// ingestMu serializes ingest() calls (the main tick's step 1 and the
	// background loop) so the read-IssueRefs-then-InsertTask dedup is atomic
	// w.r.t. itself — without it two concurrent ingests could both miss a ref
	// and double-insert it (issue_ref has no UNIQUE index).
	ingestMu sync.Mutex

	// wg tracks the background ingest goroutine so Run can wait for it to exit
	// cleanly on ctx cancellation before returning.
	wg sync.WaitGroup
}

// logf writes a formatted line to the daemon log (or stderr if Log is nil).
func (e *Engine) logf(format string, args ...interface{}) {
	if e.Log != nil {
		e.Log.Printf(format, args...)
	} else {
		fmt.Fprintf(os.Stderr, format+"\n", args...)
	}
}

// Run is the resident entry point. It ticks once immediately (a fresh daemon
// should not wait a full Interval before its first poll) and then every
// Interval until ctx is cancelled, returning ctx.Err(). A single tick's error
// is logged to stderr but does NOT halt the loop — a transient channel failure
// skips this tick and retries next, losing no persisted state (spec §11).
//
// Because a tick's dispatch step blocks synchronously inside RunTask (single
// active sub-loop, spec §12), the loop does NOT advance while a task is
// running — so a freshly-filed issue would go unseen for the task's whole run.
// When IngestMax > 0, Run therefore starts a background ingest goroutine that
// keeps pulling new issues into the FIFO on a jittered 3–10s cadence,
// independent of the blocked main tick (spec principle: the daemon never
// blocks on a human; this extends "never blocks" to ingestion during a run).
func (e *Engine) Run(ctx context.Context) error {
	if n, err := e.Store.RequeueOrphanedRunning(); err != nil {
		e.logf("[daemon] orphan recovery failed: %v", err)
	} else if n > 0 {
		e.logf("[daemon] recovered %d orphaned running task(s) → new", n)
	}
	if e.IngestMax > 0 {
		e.wg.Add(1)
		go e.ingestLoop(ctx)
	}
	if err := e.tick(ctx); err != nil {
		e.logf("daemon tick error: %v", err)
	}
	t := time.NewTicker(e.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			e.wg.Wait() // let the background ingest goroutine exit cleanly
			return ctx.Err()
		case <-t.C:
			if err := e.tick(ctx); err != nil {
				e.logf("daemon tick error: %v", err)
			}
		}
	}
}

// tick is one daemon pass (spec §7.1). Steps:
//  1. Ingest new tasks from the channel (dedup against persisted tasks).
//  2. Reconcile: read channel-side state for terminal tasks (done/blocked) and
//     detect human-driven reversals — done issue reopened → re-queue as new;
//     blocked issue had its label removed → re-queue as new.
//  3. Poll signals: check needs-review + blocked tasks for new human replies
//     since the daemon's last comment; re-queue any that got a reply.
//  4. Dispatch the FIFO head (single-active synchronous, spec §12).
func (e *Engine) tick(ctx context.Context) error {
	// ---- step 1: ingest new tasks (non-fatal: a flaky channel must not block
	// dispatch of already-ingested ready tasks) ----
	if err := e.ingest(ctx); err != nil {
		e.logf("[daemon] ingest error: %v", err)
	}

	// ---- step 2: reconcile terminal tasks against channel-side state ----
	if err := e.reconcile(ctx); err != nil {
		e.logf("[daemon] reconcile error: %v", err)
	}

	// ---- step 3: poll new human replies on parked + blocked tasks ----
	if err := e.pollSignals(ctx); err != nil {
		e.logf("[daemon] poll signals error: %v", err)
	}

	// ---- step 3.5: drain TUI commands (spec §4.2/§7) ----
	if err := e.drainCommands(ctx); err != nil {
		e.logf("[daemon] drain commands error: %v", err)
	}

	// ---- step 3.6: worktree scene GC（best-effort：失败只记日志，下个 tick 再来）----
	if e.GC != nil {
		if err := e.GC(ctx); err != nil {
			e.logf("[daemon] gc error: %v", err)
		}
	}

	// ---- step 4: dispatch (spec §7.1 step 3) ----
	if e.RunTask == nil {
		return nil
	}
	if now := time.Now(); now.Before(e.coolUntil) {
		e.logf("[daemon] tick dispatch: skip (cooldown until %s)", e.coolUntil.Format(time.TimeOnly))
		return nil
	}
	ready, ok, err := e.Store.NextReadyTask()
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	// ---- triage 门（just-in-time：队首才分诊，看到的是 spec re-ingest 热更新后
	// 的最新正文）——不占活跃位；挂起的任务等人回复后由 pollSignals 唤醒、下次
	// 派发重新分诊。triage 出错不 gate，记日志照跑。
	if e.Triage != nil {
		gate, terr := e.Triage(ctx, ready)
		switch {
		case terr != nil:
			e.logf("[daemon] triage error for task %s (%s): %v — dispatching anyway",
				shortTaskID(ready.ID), ready.IssueRef, terr)
		case !gate.Startable:
			return e.parkByTriage(ctx, ready, "needs-info",
				"triage: 缺信息——"+strings.Join(gate.MissingInfo, "；"),
				needsInfoComment(gate))
		case gate.NeedsHumanDecision || !gate.LoopDoable:
			return e.parkByTriage(ctx, ready, "needs-human-decision",
				"triage: 需人工裁决——"+gate.Reason,
				"NEEDS-HUMAN-DECISION: 分诊判断此任务需要人来拍板，暂不由 loop 自动执行。\n\n原因: "+gate.Reason+
					"\n\n请在本 issue 回复你的决定；daemon 会拾起回复并重新分诊/派发。")
		default:
			e.logf("[daemon] triage: task %s (%s) startable (difficulty=%s)",
				shortTaskID(ready.ID), ready.IssueRef, gate.Difficulty)
		}
	}
	// new→running 是单实例正确性的命门：用 ClaimTask 的原子条件更新（WHERE
	// status='new'）取代读出 NextReadyTask → AppendTransition(running) 的
	// read-modify-write 窗口。两个 engine 实例各 tick 一次同一 task 时，只有先到的一
	// 方 RowsAffected()==1 真正领到 running、进 RunTask；另一方 !claimed 直接 return，
	// 绝不 UpdateStatus / RunTask（不会产生第二个 worktree / 双份 token）。兜底纵深：
	// 实例锁（daemon.AcquireLock）是前门，这里是后门——锁被手删/绕过时仍只有一方领到。
	claimed, err := e.Store.ClaimTask(ready.ID)
	if err != nil {
		return err
	}
	if !claimed {
		e.logf("[daemon] tick dispatch: task %s (%s) already claimed by another instance — skip",
			shortTaskID(ready.ID), ready.IssueRef)
		return nil
	}
	e.logf("[daemon] tick dispatch: task %s (%s) → running", shortTaskID(ready.ID), ready.IssueRef)
	// 派发即在 channel 上标出「正在处理」（loop:running）。打标点选 daemon 派发处
	// 而非只放 SubLoop.Run 入口：daemon 是任务生命周期的唯一写者（RunTask 契约不碰
	// 状态），→running 的 transition 就发生在这行——且 RunTask 是注入的（测试 stub、
	// 未来的替代 runner 可能根本不进 SubLoop），派发点打标覆盖所有 runner。SubLoop.Run
	// 入口也会打一次（覆盖绕过 daemon 的 run-once 路径），重复打标幂等（标签互斥后
	// 第二次只是 no-op 的 remove+add）。打标失败只记日志、不翻转派发结果（与 report /
	// parkByTriage 的写回容错一致）。
	if err := e.Channel.UpdateStatus(ctx, ready.IssueRef, "running"); err != nil {
		e.logf("[daemon] dispatch running mark failed for %s: %v", ready.IssueRef, err)
	}
	status, detail, err := e.RunTask(ctx, ready)
	if err != nil {
		e.logf("dispatch: task %s (%s) failed: %v", ready.ID, ready.IssueRef, err)
		status = "error"
	}
	if status == "blocked" && isTransientInfra(detail) {
		e.coolUntil = time.Now().Add(e.cooldown())
		e.logf("[daemon] tick park: task %s transient-infra block → cooldown until %s, re-queue",
			shortTaskID(ready.ID), e.coolUntil.Format(time.TimeOnly))
		return e.Store.AppendTransition(ready.ID, "running", "new", "transient infra; re-queued")
	}
	if status == "needs-review" {
		e.logf("[daemon] tick park: task %s parked on tier-3 human review", shortTaskID(ready.ID))
	} else {
		e.logf("[daemon] tick done: task %s → %s", shortTaskID(ready.ID), status)
	}
	return e.Store.AppendTransition(ready.ID, "running", status, "ran")
}

// ingest pulls new tasks from the channel into the durable FIFO, deduped by
// issue_ref against already-persisted tasks (spec §7.1 step 1). It is called
// both from the synchronous tick (step 1) and from the background ingestLoop
// goroutine, so it serializes on ingestMu: without the lock, two concurrent
// ingests could both read IssueRefs before either inserts, and each would
// double-insert the same ref (issue_ref has no UNIQUE index).
//
// New refs are INSERTed at status="new" and never occupy the active slot, flip
// in_flight, or mutate a running task — single-active dispatch (NextReadyTask +
// one synchronous RunTask per tick) is unaffected by concurrent ingestion.
//
// Known refs are NOT simply skipped: issue bodies get edited after ingest, and
// ListNewTasks already returns full bodies every poll, so each polled task is
// diffed against the stored spec snapshot (description + criteria) and
// re-ingested via UpdateTaskSpec only when it actually changed — zero extra
// channel reads, zero no-op writes. Runs already dispatched keep their
// snapshot (channel.Task was built at dispatch); the refreshed spec takes
// effect on the next dispatch/resume.
func (e *Engine) ingest(ctx context.Context) error {
	e.ingestMu.Lock()
	defer e.ingestMu.Unlock()

	tasks, err := e.Channel.ListNewTasks(ctx)
	if err != nil {
		return err
	}
	specs, err := e.Store.TaskSpecsByRef()
	if err != nil {
		return err
	}
	var ingested int
	for _, t := range tasks {
		if known, ok := specs[t.Ref]; ok {
			// 已知 ref：正文被编辑过才回写（比对解析后的 desc+criteria+全文 body——
			// 与落库内容同构，未变时绝不产生 no-op UPDATE）。
			if known.Description != t.Description || !equalStrings(known.Criteria, t.AcceptanceCriteria) || known.Body != t.Body || known.Title != t.Title {
				if err := e.Store.UpdateTaskSpec(known.ID, t.Description, t.AcceptanceCriteria, t.Body, t.Title); err != nil {
					return err
				}
				e.logf("[daemon] ingest: task %s spec updated (issue body edited)", t.Ref)
			}
			continue
		}
		row := state.TaskRow{
			IssueRef:    t.Ref,
			Title:       t.Title, // issue 标题：PR 标题的源头（区别于 Description=正文首行）
			Description: t.Description,
			TaskType:    t.TaskType,
			Source:      "daemon",
			Criteria:    t.AcceptanceCriteria,
			Body:        t.Body,      // 全文：摄入即落库，不靠下轮 compare-and-update 回填
			CreatedAt:   t.CreatedAt, // #33/#36: 存 issue 提交时间 → NextReadyTask 按 created_at FIFO（不是入库时间）
			Agent:       t.Agent,     // 任务级 agent override（issue frontmatter `agent: codex`）
		}
		if _, err := e.Store.InsertTask(row); err != nil {
			return err
		}
		specs[t.Ref] = row // 同批次内重复 ref 不再重复 insert
		ingested++
	}
	if ingested > 0 {
		e.logf("[daemon] ingest: %d new task(s)", ingested)
	}
	return nil
}

// equalStrings 比较两个 string 切片是否逐项相等（顺序敏感）——验收标准列表的
// diff 语义：顺序/内容任一不同都视为「正文已编辑」。
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ingestLoop is the background ingest goroutine started by Run when IngestMax >
// 0. It keeps pulling new issues into the FIFO on a jittered cadence so an
// issue filed while the single active slot is busy running a long task still
// lands in state.db (and on the dashboard) — the synchronous tick is blocked
// inside RunTask and would otherwise not poll again until that task finishes.
//
// Each iteration sleeps a random delay in [IngestMin, IngestMax) (3–10s by
// default). A ListNewTasks / Store failure is logged and retried on the next
// jitter; it never propagates to or halts the main tick (spec §11 resilience).
// The loop exits when ctx is cancelled; Run's wg.Wait() awaits it on shutdown.
func (e *Engine) ingestLoop(ctx context.Context) {
	defer e.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(e.nextIngestDelay()):
		}
		if err := e.ingest(ctx); err != nil {
			e.logf("[daemon] background ingest error: %v", err)
		}
	}
}

// nextIngestDelay returns the next randomized ingest interval in [IngestMin,
// IngestMax). Randomizing the cadence (jitter) spreads channel polls off a
// fixed beat — avoiding synchronized bursts against a rate-limited upstream and
// making the loop's timing unobservable.
func (e *Engine) nextIngestDelay() time.Duration {
	min, max := e.IngestMin, e.IngestMax
	if min < 0 {
		min = 0
	}
	if max <= min {
		return min
	}
	return min + time.Duration(rand.Int64N(int64(max-min)))
}

// shortTaskID returns a truncated task ID for log lines (first 12 chars).
func shortTaskID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// cooldown returns the configured transient-infra cooldown, defaulting to 5
// minutes (spec §10/§11 resilience: wait out upstream congestion rather than
// burn the FIFO one blocked task per tick).
func (e *Engine) cooldown() time.Duration {
	if e.Cooldown > 0 {
		return e.Cooldown
	}
	return 5 * time.Minute
}

// isTransientInfra reports whether a blocked task's detail smells like transient
// upstream infrastructure failure (model-gateway congestion / rate limiting) — as
// opposed to a real verify rejection. Such blocks trip the daemon cooldown so the
// whole queue isn't burned one task per tick. Substring match (case-insensitive
// for ASCII) on the signals seen in the wild: GLM 529 "该模型当前访问量过大",
// generic 503 / rate-limit / overloaded. "context canceled" is NOT transient infra
// (that's a shutdown/timeout, not congestion).
func isTransientInfra(detail string) bool {
	d := strings.ToLower(detail)
	for _, sig := range []string{
		"529", "该模型当前", "访问量过大", "rate limit", "rate-limit",
		"overloaded", "too many requests", "service unavailable", "503",
	} {
		if strings.Contains(d, sig) {
			return true
		}
	}
	return false
}

// truncate returns s cut to at most n bytes with a "..." marker, for log lines.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// reconcile reads the channel-side state of every terminal task (done /
// blocked) and detects human-driven reversals:
//   - done + channel shows issue OPEN (reopened) → done → new
//   - blocked + channel shows no "loop:blocked" label → blocked → new
//
// This is the bidirectional sync: when a human acts on the channel (reopen,
// remove label), the daemon notices on the next tick and re-queues the task
// without a restart or DB poke.
func (e *Engine) reconcile(ctx context.Context) error {
	terms, err := e.Store.TerminalTasks()
	if err != nil {
		return err
	}
	if len(terms) == 0 {
		return nil
	}
	refs := make([]string, len(terms))
	byRef := make(map[string]state.TaskRow, len(terms))
	for i, t := range terms {
		refs[i] = t.IssueRef
		byRef[t.IssueRef] = t
	}
	states, err := e.Channel.GetTaskStates(ctx, refs)
	if err != nil {
		return err
	}
	for ref, s := range states {
		task, ok := byRef[ref]
		if !ok {
			continue
		}
		cur, _ := e.statusOf(task.ID)
		switch cur {
		case "done":
			if s.IsOpen {
				if task.LandBranch != "" {
					// 非空 land_branch = done 任务故意留 issue OPEN 等合并（PR 已建 /
					// LAND PARTIAL / land 失败 / commit 失败哨兵）。不重排队——等 PR 合并。
					// channel 实现 MergeChecker（GitHub）且该分支 PR 已 merged → 关 issue
					//（自迁移记 reason，task_status 仍 done）；未 merged / 查询出错 / channel
					// 无合并检测能力（Local/Linear）→ 不动，等下一 tick 再查。
					if mc, ok := e.Channel.(channel.MergeChecker); ok {
						merged, merr := mc.IsPRMerged(ctx, task.LandBranch)
						if merr != nil {
							e.logf("[daemon] reconcile: done task %s (#%s) IsPRMerged(%s) error: %v (leave open, retry next tick)",
								task.ID, ref, task.LandBranch, merr)
						}
						if merr == nil && merged {
							e.logf("[daemon] reconcile: done task %s (#%s) PR %s merged → close issue",
								task.ID, ref, task.LandBranch)
							if err := e.Channel.CloseIssue(ctx, ref); err != nil {
								e.logf("[daemon] reconcile: CloseIssue #%s failed: %v", ref, err)
							}
							if err := e.Store.AppendTransition(task.ID, "done", "done",
								"reconcile: PR merged → issue closed"); err != nil {
								return err
							}
						}
					}
				} else {
					// land_branch 空 = 旧 reopen 语义（本地直落已关 issue、被人 reopen）→ 重排队（回归不变）。
					e.logf("[daemon] reconcile: done task %s (#%s) reopened on channel → re-queue", task.ID, ref)
					if err := e.Store.AppendTransition(task.ID, "done", "new", "reconcile: channel reopened"); err != nil {
						return err
					}
				}
			}
		case "blocked":
			// 状态标签名从 channel 取（自定义 label_prefix 时不是 loop:blocked）；
			// 无标签语义的 channel（Local）回落默认族名。
			blockLabel := "loop:blocked"
			if sl, ok := e.Channel.(channel.StatusLabeler); ok {
				blockLabel = sl.StatusLabel("blocked")
			}
			hasBlockLabel := false
			for _, l := range s.Labels {
				if l == blockLabel {
					hasBlockLabel = true
					break
				}
			}
			if !hasBlockLabel {
				if s.IsOpen {
					e.logf("[daemon] reconcile: blocked task %s (#%s) label removed on channel → re-queue", task.ID, ref)
					if err := e.Store.AppendTransition(task.ID, "blocked", "new", "reconcile: channel label removed"); err != nil {
						return err
					}
				} else {
					e.logf("[daemon] reconcile: blocked task %s (#%s) manually closed on channel → resolved", task.ID, ref)
					if err := e.Store.AppendTransition(task.ID, "blocked", "done", "reconcile: channel closed"); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func (e *Engine) statusOf(taskID string) (string, bool) {
	rows, err := e.Store.ListStatuses()
	if err != nil {
		return "", false
	}
	for _, r := range rows {
		if r.ID == taskID {
			return r.Status, true
		}
	}
	return "", false
}

// parkByTriage 把 triage 拦下的任务挂起到 needs-info / needs-human-decision：
// 状态迁移 + issue 评论（缺什么/要人拍什么板，人可见——人的回复会被 pollSignals
// 拾起重新排队，下次派发重新分诊）。评论/打标失败不翻转挂起结果（记日志，
// 与 SubLoop.report 的写回容错一致）。
func (e *Engine) parkByTriage(ctx context.Context, task state.TaskRow, status, reason, comment string) error {
	if err := e.Store.AppendTransition(task.ID, "new", status, reason); err != nil {
		return err
	}
	e.logf("[daemon] triage park: task %s (%s) → %s (%s)",
		shortTaskID(task.ID), task.IssueRef, status, truncRunes(reason, 80))
	if err := e.Channel.PostComment(ctx, task.IssueRef, comment); err != nil {
		e.logf("[daemon] triage park comment failed for %s: %v", task.IssueRef, err)
	} else {
		_ = e.Store.SetLastCommentAt(task.ID, time.Now())
	}
	if err := e.Channel.UpdateStatus(ctx, task.IssueRef, status); err != nil {
		e.logf("[daemon] triage park status mark failed for %s: %v", task.IssueRef, err)
	}
	return nil
}

// needsInfoComment 构造「缺信息」的 issue 评论正文：逐条列出缺什么（人要照着
// 补的清单）+ 分诊理由 + 回复指引。
func needsInfoComment(g skill.TriageOutput) string {
	var b strings.Builder
	b.WriteString("NEEDS-INFO: 分诊判断任务信息不足，暂不开工。\n\n缺少的信息:\n")
	for _, mi := range g.MissingInfo {
		b.WriteString("- " + mi + "\n")
	}
	if strings.TrimSpace(g.Reason) != "" {
		b.WriteString("\n分诊理由: " + g.Reason + "\n")
	}
	b.WriteString("\n请在本 issue 补充；daemon 会拾起回复并重新分诊/派发。")
	return b.String()
}

// truncRunes 截断到 n 个字符（超出加 …）——日志行用。
func truncRunes(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n]) + "…"
}

// pollSignals checks every parked (needs-review / needs-info /
// needs-human-decision) and blocked task for new human replies since the
// daemon's last comment. A task that has been replied to is re-queued (→ new)
// with the human feedback recorded in the transition reason. The daemon never
// blocks here: ListReplies is a non-blocking poll, and a transient channel
// error is logged but does not halt the tick (spec §10/§11).
func (e *Engine) pollSignals(ctx context.Context) error {
	if err := e.pollTaskReplies(ctx, e.Store.ParkedTasks, "needs-review"); err != nil {
		e.logf("poll signals (needs-review): %v", err)
	}
	if err := e.pollTaskReplies(ctx, e.Store.BlockedTasks, "blocked"); err != nil {
		e.logf("poll signals (blocked): %v", err)
	}
	if err := e.pollTaskReplies(ctx, e.Store.NeedsInfoTasks, "needs-info"); err != nil {
		e.logf("poll signals (needs-info): %v", err)
	}
	if err := e.pollTaskReplies(ctx, e.Store.NeedsHumanDecisionTasks, "needs-human-decision"); err != nil {
		e.logf("poll signals (needs-human-decision): %v", err)
	}
	return nil
}

// pollTaskReplies polls one class of parked task (needs-review / blocked /
// needs-info / needs-human-decision) for new human replies and re-queues any
// that have been answered. Previously this issued one ListReplies PER task
// (each a serial network round-trip with its own retry/backoff), so N parked
// tasks blocked the whole daemon tick — a network blip could stall dispatch
// for minutes. Now it makes ONE batched ListReplies for the whole class
// (since=zero: the channel returns every comment, capped/concurrent inside),
// then re-filters each task against its OWN last_comment_at client-side. A
// batch error is logged but does not halt the tick: partial results are still
// processed (a ref that failed simply has no replies and is skipped, exactly
// as the old per-task error path did — it no longer also blocks every other
// ref). A task is re-queued iff it has ≥1 reply newer than its last_comment_at.
func (e *Engine) pollTaskReplies(
	ctx context.Context,
	fetch func() ([]state.TaskRow, error),
	currentStatus string,
) error {
	tasks, err := fetch()
	if err != nil {
		return err
	}
	if len(tasks) == 0 {
		return nil
	}
	refs := make([]string, len(tasks))
	sinces := make([]time.Time, len(tasks))
	for i, t := range tasks {
		refs[i] = t.IssueRef
		sinces[i], _ = e.Store.LastCommentAt(t.ID)
	}
	// One batched fetch for the whole class. since=zero → channel returns all
	// comments (carrying CreatedAt); repliesSince re-applies each task's own
	// last_comment_at below. An error is log-only: the partial map is still
	// consumed so a failing ref doesn't block the rest of the class.
	replies, lerr := e.Channel.ListReplies(ctx, refs, time.Time{})
	if lerr != nil {
		e.logf("%s: ListReplies %d refs failed: %v (processing partial)", currentStatus, len(refs), lerr)
	}
	var resumed int
	for i, t := range tasks {
		rs := repliesSince(replies[t.IssueRef], sinces[i])
		if len(rs) == 0 {
			continue
		}
		e.logf("[daemon] tick resume: task %s (%s=%s) has new human reply → re-queue",
			shortTaskID(t.ID), currentStatus, t.IssueRef)
		reason := "resumed: " + joinReplies(rs)
		if err := e.Store.AppendTransition(t.ID, currentStatus, "new", reason); err != nil {
			return err
		}
		if err := e.Store.SetResumeFeedback(t.ID, joinReplies(rs)); err != nil {
			e.logf("[daemon] task %s SetResumeFeedback failed: %v", t.ID, err)
		}
		resumed++
	}
	if resumed > 0 {
		e.logf("[daemon] tick resume: %d task(s) re-queued", resumed)
	}
	return nil
}

// repliesSince filters replies to those created strictly after since. It is the
// client-side half of the batched poll: ListReplies fetched every comment for a
// whole class with since=zero, and this re-applies one task's own
// last_comment_at. A reply whose CreatedAt is empty or unparseable is KEPT
// (safe side: surface a possibly-stale reply and re-queue rather than silently
// drop a real human response whose timestamp the channel couldn't report).
// When since is zero, every reply passes.
func repliesSince(rs []channel.Reply, since time.Time) []channel.Reply {
	if since.IsZero() {
		return rs
	}
	out := make([]channel.Reply, 0, len(rs))
	for _, r := range rs {
		if r.CreatedAt == "" {
			out = append(out, r) // channel reported no time → keep (safe side)
			continue
		}
		t, err := time.Parse(time.RFC3339, r.CreatedAt)
		if err != nil {
			out = append(out, r) // unparseable → keep (safe side)
			continue
		}
		if t.After(since) {
			out = append(out, r)
		}
	}
	return out
}

// joinReplies flattens one task's human replies into a single feedback string
// for the durable resume record. A human may post several replies; they are
// joined so the resumed task sees the whole thread as next round's feedback.
func joinReplies(rs []channel.Reply) string {
	parts := make([]string, len(rs))
	for i, r := range rs {
		parts[i] = r.Body
	}
	return strings.Join(parts, " | ")
}

// drainCommands applies every pending TUI command (resume/cancel) and marks it
// applied (spec §4.2/§7)。幂等：目标已终态（done/cancelled）或 running（由 SubLoop
// 自查处理）的命令只回写 applied_at，不产生 transition。单条命令出错则中断本轮
// drain，下 tick 从尚未 applied 的行重试。
func (e *Engine) drainCommands(ctx context.Context) error {
	cmds, err := e.Store.PendingCommands()
	if err != nil {
		return err
	}
	for _, c := range cmds {
		if err := e.applyCommand(ctx, c); err != nil {
			return err
		}
		if err := e.Store.MarkCommandApplied(c.ID); err != nil {
			return err
		}
	}
	if len(cmds) > 0 {
		e.logf("[daemon] tick commands: drained %d", len(cmds))
	}
	return nil
}

// applyCommand 把单条 TUI 命令翻译成 transition（spec §7）：
//   - resume（needs-review/blocked/needs-info/needs-human-decision/cancelled）→ X→new 并把 payload 落盘为 resume 反馈。
//   - cancel（new/needs-info/needs-review/blocked）→ X→cancelled。
//   - running/done/error → 不动作（running 由 SubLoop 自查；done/error 已终态）。
func (e *Engine) applyCommand(ctx context.Context, c state.CommandRow) error {
	cur, _ := e.statusOf(c.TaskID)
	switch c.Verb {
	case "resume":
		if cur == "needs-review" || cur == "blocked" || cur == "needs-info" || cur == "needs-human-decision" || cur == "cancelled" {
			if err := e.Store.SetResumeFeedback(c.TaskID, c.Payload); err != nil {
				return err
			}
			return e.Store.AppendTransition(c.TaskID, cur, "new", "tui resume: "+c.Payload)
		}
	case "cancel":
		switch cur {
		case "new", "needs-info", "needs-human-decision", "needs-review", "blocked":
			return e.Store.AppendTransition(c.TaskID, cur, "cancelled", "cancelled by TUI")
		}
		// running → SubLoop 自查处理；done/cancelled → 已终态
	}
	return nil
}

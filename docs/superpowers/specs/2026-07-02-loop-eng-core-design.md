# loop-eng Core (Sub-project 1) — Design Spec

- **Date:** 2026-07-02
- **Status:** Draft, awaiting user review
- **Scope:** Sub-project 1 of the `loop-eng` v1 roadmap (the core + verification foundation)
- **Build language:** Go

This spec designs **only sub-project 1**. Sub-projects 2–4 are listed for context and get their own specs later.

---

## 1. Context & Goal

`loop-eng` is a personal loop-engineering tool that implements the feedback-control-loop methodology from the two source articles: a control loop whose **controller is an LLM**, with the three adaptations that forces — **independent verification, accurate current-state representation, and state persistence to disk**.

`loop-eng` is a **resident CLI + runtime** (the "B" form): the tool owns the loop orchestration code; the user owns the volatile parts (skills, verification scripts, config). The user installs the tool once, configures it per project, and runs it against development tasks.

**v1 includes all of:** core loop, full 3-tier verification, observability data layer, a TUI dashboard, GitHub Issue integration, and a resident daemon. v1 is built as four sequenced sub-projects; this spec covers the first.

**Sub-project 1 goal:** a local, on-demand loop that can take a single task, triage it, run a sub-loop (plan → execute → verify → write-back) with full 3-tier verification, enforce budget, isolate execution in a worktree, persist a replay-able trace, and route the outcome (done / blocked / needs-review / needs-info). This is the foundation everything else stands on; verification is its keystone.

---

## 2. v1 Roadmap (context)

| # | Sub-project | This spec? |
|---|---|---|
| 1 | Core loop + full 3-tier verification + replay-able state + budget + worktree isolation + observability **data layer** | ✅ |
| 2 | Dashboard TUI (reads #1's SQLite) | later |
| 3 | GitHub Issue integration (task source + battle reports; replaces local reports) | later |
| 4 | Daemon (resident polling over #3's Issue source) | later |

Build order rationale: verification is the life-or-death line, so it must land solid before the dashboard or daemon add value on top of it. Observability's **data layer** belongs to #1 (every transition is a row from day one); its **presentation layer** (TUI) is #2.

---

## 3. Non-Negotiable Principles (from the articles)

These are hard rules. Any design choice that violates one is wrong.

1. **Context is cache, not source of truth.** Anything that must survive a round is persisted to SQLite/disk before the round ends. Nothing cross-round lives only in LLM context. *"LLM's memory only counts what's on disk. Context counts as zero."*
2. **Verification is independent of execution.** Independence comes from **not sharing context/information**, not merely from using a different model. Verify reads only the persisted diff + acceptance criteria, never the execution reasoning.
3. **Self-report is a signal, not a verdict.** Execute saying "I'm done, criteria met" only triggers verification. The verdict is always set by an independent verification layer.
4. **Feed accurate current state every round.** The controller sees only the text we feed it. The sub-loop re-reads persisted state at the start of each round; it never trusts a stale narrative.
5. **Three budget brakes from day one** — per-call tokens, per-task tokens, max retries. A loop without these is a money fire.
6. **Skills are non-deterministic code.** Stateless, fixed responsibility, fixed I/O contract. Versioned, with regression sets.
7. **Main loop vs sub-loop (cascade control).** Main loop manages global state and dispatch; sub-loop is created for one task, runs, returns its result, and is destroyed. Sub-loop results are *ingested* by the main loop.

---

## 4. Scope

**In scope (sub-project 1):**
- CLI: `init`, `run`, `status`, `replay`, `skill` (edit/test), `config`
- Main loop: triage → hard-gate → dispatch sub-loop → route outcome
- Sub-loop orchestration: plan → execute → verify → write-back, with retry
- Full 3-tier verification chain (deterministic → LLM fresh-context → human)
- 4 skills: `triage`, `plan`, `verify`, `help` (execute uses `claude` directly, no skill)
- Two model-integration paths: direct API (triage, plan) and `claude -p` (execute, verify-tier2)
- Replay-able SQLite trace (observability data layer)
- Budget enforcement (3 brakes) + append-only ledger
- Worktree isolation for execution
- Skill regression test harness
- Resumability after crash

**Out of scope (later sub-projects / post-v1):**
- Dashboard TUI (#2), GitHub Issue integration (#3), daemon/polling (#4)
- Web dashboard (post-v1), multi-loop coordination (post-v1)
- Multi-repo worktree fan-out, skill auto-versioning via git (future)

---

## 5. Ownership Line (the "B" contract)

| Tool owns (stable core; only changes on upgrade) | User owns (volatile; the user tunes these) |
|---|---|
| Main-loop dispatch, sub-loop skeleton, state read/write, budget enforcement, verification chaining, model clients | Skill prompt files (`.loop/skills/*.md`), verification scripts (declared in config), config params, acceptance criteria |

Built-in skill defaults ship inside the binary via `go:embed`; any file in `.loop/skills/` overrides the built-in of the same name. So "tuning loop engineering as code" happens entirely on the user's side of the line and survives tool upgrades.

---

## 6. Architecture

### 6.1 Component map

```
cmd/loop-eng/main.go            entry point
internal/
  cli/        cobra commands: root, init, run, status, replay, skill, config
  config/     load + validate .loop/config.yaml
  loop/
    mainloop.go    triage → gate → dispatch → route  (one task)
    subloop.go     plan → execute → verify → writeback (one task, with retry)
  skill/      skill registry, template rendering, I/O (de)serialization, regression
  model/      two clients: apiClient (anthropic SDK) + claudeClient (os/exec `claude -p`)
  verify/     chain.go, deterministic.go, llm.go, human.go
  state/      SQLite store (modernc.org/sqlite), schema, replay reader
  budget/     per-call / per-task / retry enforcement + ledger
  isolation/  worktree create / commit / discard
  embed/skills/*.md   built-in skill defaults (go:embed)
```

Each package has one clear purpose, communicates through typed interfaces, and is unit-testable in isolation.

### 6.2 Key interfaces (responsibility boundaries)

- `model.Client` — `Call(ctx, prompt, schema) (output, usage, err)`. Two implementations (`apiClient`, `claudeClient`); the loop only depends on the interface, so tests inject a stub.
- `skill.Skill` — `{Name, Version, Render(input) (prompt string), Parse(output) (typed, err)}`. Stateless.
- `verify.Tier` — `Check(ctx, diff, criteria, priorFailure) (result{Passed, Detail, FailingCriteria}, err)`. Three implementations.
- `state.Store` — append-only writer + replay reader. No in-place mutation of trace rows.
- `budget.Enforcer` — `BeforeCall(usageEstimate) error`, `AfterCall(usage)`, `ShouldRetry(attempt) bool`.

---

## 7. Data Flow — one task's journey

```
loop-eng run "<task>"  (or --task-file task.md)
  │
  ▼
[MAIN LOOP]  (lives only for this task)
  1. load task (description + acceptance_criteria + type)
  2. TRIAGE  ── triage skill (small model, direct API)
  3. HARD-GATE ── config rules (mechanical, e.g. deploy-type requires human-go)
  4. if not startable   → needs-info  (write missing_info)  → END
     if gate blocks     → needs-human-decision             → END
  5. dispatch SUB-LOOP with a fresh budget
  │
  ▼
[SUB-LOOP]  (plan → execute → verify → writeback, retry within budget)
  round n:
    a. PLAN     ── plan skill (direct API): reads persisted state + task + criteria → execution plan (no code)
    b. EXECUTE  ── `claude -p` in a worktree: plan + task + criteria + repo path → diff + self-report(signal)
    c. VERIFY   ── 3-tier chain, in order:
                    tier 1  deterministic scripts (config)        ─ fail → feedback
                    tier 2  `claude -p` FRESH: diff + criteria only ─ fail → feedback
                    tier 3  human review (needs-review)            ─ reject → feedback
                  all pass → writeback
    d. WRITEBACK ── persist state (SQLite trace) + battle report (.md) + ingest result into main loop
    on verify fail (tier1/2) or human reject:
       feed failure back as input to next round's PLAN; retry (≤ max_retries)
    on 3 consecutive fails OR budget exhausted:
       → blocked → help skill → help_request written → END
  │
  ▼
[ROUTE]  done | blocked | needs-review | needs-info | error
```

Every numbered step and every state transition writes a row to SQLite **before** proceeding, so the run is resumable and replay-able from any point.

---

## 8. Component Details

### 8.1 CLI surface (v1)

| Command | Purpose |
|---|---|
| `loop-eng init` | Scaffold `.loop/` in cwd: write `config.yaml`, copy default skills, create empty `state.db`, set up worktree base dir. The "setup" step. |
| `loop-eng run "<task>" \| --task-file <path>` | Run the full loop on one task. `--resume <task-id>` resumes from last checkpoint. |
| `loop-eng status [<task-id>]` | Print task/run state from SQLite. |
| `loop-eng replay <run-id>` | Replay a past run step-by-step from the trace. |
| `loop-eng skill edit <name>` | Open the project's override of a skill (creating from the built-in default if absent). |
| `loop-eng skill test [<name>]` | Run skill regression fixtures. |
| `loop-eng config (get\|set\|edit)` | View/edit config. |

`loop-eng dashboard` is reserved (sub-project 2).

### 8.2 Config (`.loop/config.yaml`)

```yaml
models:
  triage: { provider: anthropic, name: claude-haiku-4-5 }   # small/cheap/fast
  plan:   { provider: anthropic, name: claude-sonnet-5 }
  execute: { via: claude-p, binary: claude }
  verify:  { via: claude-p, binary: claude }                 # fresh session per call
budget:
  per_call_tokens: 20000
  per_task_tokens: 200000
  max_retries: 3
verify:
  deterministic:
    - { label: tests, cmd: ["pytest", "-q"] }
    - { label: types, cmd: ["mypy", "src/"] }
    - { label: lint, cmd: ["ruff", "check", "."] }
  tier3_human: true
isolation:
  worktree: true
gate:
  rules:
    - { when: "task_type == deploy", requires: human_go }
skills:
  dir: .loop/skills     # overrides built-in defaults
```

Config is validated on load; missing/invalid budget values are a hard error (no silent defaults for the brakes).

### 8.3 Main loop

1. Load task.
2. **Triage** via `triage` skill (small model, direct API). Output is a typed judgment (see 8.5).
3. **Hard-gate**: apply config `gate.rules` mechanically. Rules do *not* understand — they enforce (e.g. deploy-type tasks require a human-go flag). Model understanding happened in triage; rules do the hard gating.
4. Route: `needs-info` (startable=false), `needs-human-decision` (gate blocks), or dispatch sub-loop.
5. On sub-loop return, record outcome + ingest result.

### 8.4 Sub-loop orchestration

The plan→execute→verify→writeback skeleton, run per task with bounded retries. State is re-read from the persisted store at the start of each round (principle 4). The sub-loop is destroyed after the task; only its persisted result remains (principle 7).

### 8.5 Skills (user-owned, override `go:embed` defaults)

Stateless, fixed I/O contract, versioned. JSON in/out.

- **triage** — in: `{task_description, acceptance_criteria, task_type}` → out: `{startable, missing_info[], loop_doable, suggested_type, difficulty, needs_human_decision, reason}`
- **plan** — in: `{task, acceptance_criteria, battle_report(prior rounds), repo_state_summary}` → out: `{plan:[{step, files, expected}], risks[]}` (no code)
- **verify** (tier-2) — in: `{diff, acceptance_criteria, prior_failure_signal?}` → out: `{passed, reason, failing_criteria[]}` (fresh context; execution reasoning is **never** in the input)
- **help** — in: `{task, blocked_state, attempts_summary, last_error}` → out: `{help_request:{stuck_at, tried[], need_from_human}}`

**Execute is not a skill** — it invokes `claude -p` directly (needs tool use / file editing).

Skill I/O is strongly typed Go structs; `encoding/json` round-trips through `skill.Skill.Parse`. Strong typing *is* the enforcement of "fixed I/O contract."

### 8.6 Verification — the keystone (full 3-tier, no phasing)

Architectural enforcement of independence (principle 2): the `verify` package is a **separate component** from execution and shares **no** in-memory context with it. It reads only `(diff, acceptance_criteria)` from the persisted store.

- **Tier 1 — deterministic scripts.** Run each configured `verify.deterministic` command against the worktree; parse exit code + output. Most independent (no LLM). Default first filter. A script that errors (not just fails) is treated as fail-with-detail, never as pass.
- **Tier 2 — LLM fresh context.** `claude -p` in a brand-new session, given **only** diff + acceptance criteria (+ the prior round's *verification* failure detail if retrying — never execute/plan output). Never the execution conversation, never plan/execute output. For semantic criteria scripts can't check.
- **Tier 3 — human.** Task → `needs-review`; print diff + report; interactive prompt (accept / reject-with-feedback). Non-interactive runs exit `needs-review` (resumable). For business correctness, aesthetics, external-dependency correctness.

**Order:** tier 1 → 2 → 3. Fail at tier 1 short-circuits (don't spend tier 2/3). Fail at any tier → failure becomes feedback to the next round's plan; retry within budget. **Self-report from execute only triggers the chain; it is never a verdict** (principle 3, enforced structurally: execute's output is not an input to any tier's pass decision).

### 8.7 State / persistence (observability data layer)

SQLite via `modernc.org/sqlite` (pure Go → static binary). Schema is **append-only and designed for replay**, not just current state.

Tables (sketch):

- `tasks(id, description, acceptance_criteria_json, task_type, source, created_at, updated_at)`
- `runs(id, task_id, started_at, ended_at, outcome, total_tokens, retry_count)`
- `steps(id, run_id, seq, role[triage|plan|execute|verify], skill, model_ref, input_hash, input_json, output_json, tokens_in, tokens_out, status[ok|fail|blocked], error, at)`
- `verifications(id, step_id, tier[1|2|3], passed, detail, at)`
- `transitions(id, task_id, from_status, to_status, reason, at)` — one row per state change
- `budget_ledger(id, run_id, scope[call|task], kind[tokens|retry], amount, limit, at)`
- `reports(id, task_id, round, path, at)` — pointer to the battle-report `.md`

Indexes on `(run_id, seq)`, `(task_id, at)`. Trace rows (`steps`, `transitions`, `verifications`, `budget_ledger`) are **append-only**; replay = `SELECT … WHERE run_id=? ORDER BY seq`. Battle reports are human-readable markdown at `.loop/reports/<task-id>-r<round>.md`, one per round — the text the next round's controller reads (the "battle report" / 失忆将军's morning briefing).

### 8.8 Budget

Three brakes enforced before every model call and every retry (principle 5): per-call token cap, per-task token cap, max retries (default 3). Every check appends to `budget_ledger`. Hitting any brake → abort → `blocked` → `help` skill → write help_request → end. The limits are configurable; the *existence* of all three is not.

### 8.9 Worktree isolation

Each sub-loop executes in a fresh git worktree at `.loop/worktrees/<run-id>/`. On verify-pass, the worktree diff is the artifact (later: proposed as a PR). On fail/abort, the worktree branch is discarded. This is the rollback primitive (article 2): "跑飞了丢这个分支."

### 8.10 Model integration (two paths)

- **Direct API** (`apiClient`, anthropic Go SDK): `triage`, `plan`. Cheap, fast, structured-JSON out. Triage uses a small model. Keeps the heavy `claude` spin-up off the lightweight judgments.
- **`claude -p`** (`claudeClient`, `os/exec`): `execute`, `verify`-tier2. Execute needs tool use / file editing; verify-tier2 needs a fresh Claude Code session per the articles. The `claudeClient` always starts a **new** session (no shared conversation) — this is part of how verification independence is enforced.

The loop depends only on `model.Client`; the two implementations are swappable and stubbable.

---

## 9. Task & Acceptance-Criteria Format

A task (CLI string or `--task-file`, markdown/yaml) has:

```yaml
description: "Fix the login button 500 error"
task_type: bugfix
acceptance_criteria:
  - "clicking login with valid creds returns 200"      # → maps to a test (tier 1)
  - "no new errors in the error log"                   # → tier 1 (log check) or tier 2
  - "the fix matches the product intent (no UX change)"# → tier 3 (human)
```

Triage judges whether criteria are sufficient and which are scriptable. Verify tier-1 maps scriptable criteria to configured scripts; tiers 2/3 handle the rest. Without checkable acceptance criteria, triage returns `startable=false` → `needs-info`.

---

## 10. Error Handling & Resumability

- **Triage `startable=false`** → transition `needs-info`, write `missing_info` to report, end.
- **Gate block** → transition `needs-human-decision`, write reason, end.
- **Verify fail (tier 1/2)** or **human reject** → failure becomes feedback to next round's plan; retry within budget.
- **3 consecutive fails or budget exhausted** → transition `blocked`; `help` skill generates `help_request`; write to report; end.
- **Tool crash** → SQLite holds the last committed transition (every transition is a checkpoint written before proceeding). `loop-eng run --resume <task-id>` reconstructs run state from the latest `transition` + `run` row and continues.
- **Deterministic script errors** (vs fails) → treated as fail-with-detail, never silent pass.

---

## 11. Testing Strategy

- **Unit** (deterministic, no network): budget enforcement, gate rules, verify-chain ordering & short-circuit, skill I/O (de)serialization, config validation, state append + replay read, worktree create/discard.
- **Skill regression** (principle 6): each skill ships a fixture set (input → expected output *shape*). `loop-eng skill test` runs them. Two modes: live-model (local) and recorded-fixture (CI, deterministic). Skills are versioned; a regression failure blocks a skill change.
- **Integration**: inject a stub `model.Client` (canned plan/verify outputs) + a fake `claude` (fixed diff) → drive a full run deterministically and assert state transitions.
- **E2E acceptance** (the v1 "minimum runnable loop" target): a hello-world task — *"create file greet.txt containing 'hello'"* with criterion *"file greet.txt exists with content 'hello'"* (tier-1 check script). Exercises triage → plan → execute → verify → writeback → done end-to-end.

---

## 12. Key Decisions (with rationale)

- **Go over Python** — single static binary (deploy), goroutine-native daemon (later), strong-typed skill I/O contracts. The usual "Python LLM-ecosystem" objection dissolves: this tool makes thin API calls and shells out to `claude`; it doesn't need langchain.
- **`modernc.org/sqlite` (pure Go)** over CGO driver — keeps the binary fully static and cross-compilable.
- **Two model paths** (API for triage/plan, `claude -p` for execute/verify) — article-faithful; keeps lightweight judgments cheap and gives execute/verify the tool-use/fresh-session they need.
- **Verification independence enforced structurally** (separate package, no shared context, fresh sessions) — the life line is an architectural property, not a convention.
- **Skills as user-owned markdown overriding `go:embed` defaults** — the B ownership line; tuning survives upgrades.
- **Append-only replay-able trace** — debugging by replay, not by reproduction (article 2).
- **Self-report is signal not verdict** — hard rule, enforced by execute's output never feeding any tier's pass decision.

---

## 13. Open Questions (to resolve before/early in implementation)

1. **Skill prompt authoring for v1 defaults** — the four built-in skills need first-draft prompts. Decide: write them as part of sub-project 1, or ship minimal stubs and tune via the seed-task calibration (article 2's cold-start)?
2. **Tier-3 interaction model default** — interactive prompt vs. always non-interactive-exit-resumable. (Default proposed: interactive when on a TTY, else resumable exit.)
3. **`claude -p` invocation specifics** — exact flags / input format for execute and for fresh verify sessions. Confirm against the installed Claude Code version at implementation time.
4. **Seed-task set for cold-start calibration** — article 2 mandates real tasks to tune triage thresholds / acceptance-criteria patterns. Decide the seed set for sub-project 1's calibration.

---

## 14. Glossary (article terms → components)

| Article term | In this design |
|---|---|
| 反馈控制环 (feedback control loop) | the whole `loop-eng run` flow |
| 控制器 = LLM | model clients (triage/plan via API, execute/verify via `claude -p`) |
| 设定值 (setpoint) / 期望 | acceptance criteria |
| 观测值 / 现状 (current state) | persisted SQLite state re-read each round |
| 失忆将军 (amnesiac general) | stateless skills + context-as-cache principle |
| 战报 (battle report) | `.loop/reports/*.md` + `steps`/`transitions` rows |
| 主循环 / 子循环 (cascade) | `mainloop.go` / `subloop.go` |
| 验证独立 (verification independence) | `internal/verify`, fresh sessions, no shared context |
| 落盘 (persist to disk) | SQLite append-only trace |
| 预算刹车 (budget brakes) | `internal/budget` (3 brakes) |
| skill（非确定性代码） | `internal/skill` + regression harness |

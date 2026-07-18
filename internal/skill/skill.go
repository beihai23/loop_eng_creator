// Package skill defines the generic, stateless Skill[I,O] abstraction used
// across loop-eng. Each skill fixes an I/O contract via strong Go types
// (spec §8.5 / 原则 6): Run renders a text/template prompt from the typed
// input, calls model.Client, and ParseJSON decodes the typed output. Skills
// carry no state themselves; versioning lives in Registry metadata for the
// CLI skill edit/test commands.
package skill

import (
	"context"
	"fmt"
	"strings"

	"loop-eng/internal/model"
)

type Skill[I any, O any] struct {
	Name, Version, PromptTmpl string
	ParseJSON                 func([]byte) (O, error)
	Model                     model.Client
}

func (s Skill[I, O]) Run(ctx context.Context, input I) (O, model.Usage, error) {
	var zero O
	prompt, err := render(s.PromptTmpl, input)
	if err != nil {
		return zero, model.Usage{}, fmt.Errorf("render skill %s: %w", s.Name, err)
	}
	out, usage, err := s.Model.Call(ctx, prompt)
	if err != nil {
		return zero, usage, err
	}
	parsed, err := s.ParseJSON([]byte(extractJSON(out)))
	if err != nil {
		return zero, usage, fmt.Errorf("parse skill %s output: %w (raw=%q)", s.Name, err, out)
	}
	return parsed, usage, nil
}

// ---- I/O 类型（对应 spec §8.5）----

type TriageInput struct {
	TaskDescription    string
	AcceptanceCriteria []string
	TaskType           string
	// Body is the full raw issue text (背景/约束/上下文)。TaskDescription 只是
	// 正文首行的蒸馏——判断「缺不缺信息」恰恰要看全文，不能只凭首行。
	Body string
}
type TriageOutput struct {
	Startable          bool     `json:"startable"`
	MissingInfo        []string `json:"missing_info"`
	LoopDoable         bool     `json:"loop_doable"`
	SuggestedType      string   `json:"suggested_type"`
	Difficulty         string   `json:"difficulty"`
	NeedsHumanDecision bool     `json:"needs_human_decision"`
	Reason             string   `json:"reason"`
}

type PlanInput struct {
	Task               string
	AcceptanceCriteria []string
	BattleReport       string
	RepoStateSummary   string
	// Body is the full raw issue text (背景/约束/上下文)。Description 只是正文
	// 首行的蒸馏；plan 评审验收标准、规划实现时应能看到全文。
	Body string
	// RetryDiagnosis is a "how to plan" meta-instruction SubLoop injects on retries
	// (attempt ≥ 2 with a non-empty priorFailure): it quotes this round's
	// priorFailure and asks plan to diagnose whether the verify rejection is a
	// structural unsatisfiability of the loop's data flow (e.g. demanding command
	// output in a diff/battle-report that execute's worktree stdout never enters),
	// and if so to exercise revised_criteria by translating the evidence
	// requirement into a tier-1 mechanically-checkable exit-code/compile-time
	// predicate. Empty on attempt=1 / no prior failure — first-time planning is
	// left undisturbed. This is meta (how to plan), deliberately kept OUT of
	// BattleReport (which is "what happened" history/context); rendering it through
	// its own {{.RetryDiagnosis}} block keeps the two separable for plan.
	RetryDiagnosis string
}
type PlanStep struct {
	Step     string   `json:"step"`
	Files    []string `json:"files"`
	Expected string   `json:"expected"`
}
type PlanOutput struct {
	Plan  []PlanStep `json:"plan"`
	Risks []string   `json:"risks"`
	// VerifyScript is the tier-1 acceptance script the planner emits when it
	// judges the task scriptable. It REPLACES the old static
	// config.verify.deterministic list — tier-1 is now per-task, produced by
	// plan. nil (or invalid) ⇒ tier-1 absent ⇒ the verify chain falls straight
	// to tier-2 (LLM). No static/fallback list anywhere.
	VerifyScript *PlanVerifyScript `json:"verify_script,omitempty"`
	// RevisedCriteria is the acceptance-criteria contract the planner commits
	// to after critically reviewing the issue's raw criteria (clarify / rewrite
	// / drop with justification). nil ⇒ 未修订 ⇒ downstream falls back to the
	// issue's raw criteria; non-nil (even empty) ⇒ plan 修订版，execute 按它
	// 实现、verify 按它判。防放水约束写在 plan.md：修订必须忠于任务意图，
	// 删除必须给理由（CriteriaNotes），由 tier-3 人审兜底。
	RevisedCriteria *[]string `json:"revised_criteria,omitempty"`
	// CriteriaNotes is the planner's justification for the revision (why each
	// criterion was clarified/rewritten/dropped) — the audit trail surfaced in
	// the run trace and battle reports. Empty when nothing was revised.
	CriteriaNotes string `json:"criteria_notes,omitempty"`
}

// PlanVerifyScript is the plan-produced tier-1 acceptance script: a runnable
// command (Run) plus, optionally, a multi-line script body (Body) the tier
// writes into the worktree before running. The tech stack is chosen by the
// planner — Go/Node/Python/Rust/... all flow through the same {run, body, file}
// shape. There is no static config list behind this; every tier-1 script comes
// from a PlanOutput.
//
// Fields:
//   - Run:  the run command (REQUIRED), executed in the worktree root; exit 0
//     = pass. e.g. ["go","test","./..."], ["sh","-c","CGO_ENABLED=0 go build ./..."],
//     ["npm","test"], ["sh","verify.sh"].
//   - Body: optional script source. When non-empty the tier writes it to
//     <worktree>/<File> first, then runs Run — for multi-line scripts.
//   - File: worktree-relative path Body is written to. REQUIRED when Body is
//     non-empty (nowhere to land otherwise).
//   - Label: optional one-line observability tag (lands in the verify trace).
type PlanVerifyScript struct {
	Label string   `json:"label,omitempty"`
	File  string   `json:"file,omitempty"`
	Body  string   `json:"body,omitempty"`
	Run   []string `json:"run"`
}

// Valid reports whether a plan-produced verify script is runnable — the basic
// plan-output validation (non-empty + has a run command). A nil receiver ("plan
// did not produce one") is NOT invalid, it is absent: callers treat absent as
// "skip tier-1, fall to tier-2". Invalid means "plan produced a script that
// cannot run" (missing Run, or a Body with no File to land in) — callers also
// drop it to tier-2 and log the drop.
func (s *PlanVerifyScript) Valid() bool {
	if s == nil {
		return false
	}
	if len(s.Run) == 0 { // 必须有运行命令
		return false
	}
	if strings.TrimSpace(s.Body) != "" && s.File == "" { // 有 body 必须有落盘文件
		return false
	}
	return true
}

type VerifyInput struct {
	Diff               string
	AcceptanceCriteria []string
	PriorFailureSignal string
}
type VerifyOutput struct {
	Passed          bool     `json:"passed"`
	Reason          string   `json:"reason"`
	FailingCriteria []string `json:"failing_criteria"`
}

type HelpInput struct {
	Task            string
	BlockedState    string
	AttemptsSummary string
	LastError       string
}
type HelpOutput struct {
	HelpRequest struct {
		StuckAt       string   `json:"stuck_at"`
		Tried         []string `json:"tried"`
		NeedFromHuman string   `json:"need_from_human"`
	} `json:"help_request"`
}

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
	parsed, err := s.ParseJSON([]byte(out))
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
}
type PlanStep struct {
	Step     string   `json:"step"`
	Files    []string `json:"files"`
	Expected string   `json:"expected"`
}
type PlanOutput struct {
	Plan  []PlanStep `json:"plan"`
	Risks []string   `json:"risks"`
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

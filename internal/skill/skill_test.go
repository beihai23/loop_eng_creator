package skill

import (
	"context"
	"encoding/json"
	"testing"

	"loop-eng/internal/model"
)

func TestSkillRunParsesOutput(t *testing.T) {
	fake := model.NewFake(map[string]string{
		"TRIAGE:": `{"startable":true,"loop_doable":true,"suggested_type":"bugfix","difficulty":"low","reason":"ok"}`,
	})
	s := Skill[TriageInput, TriageOutput]{
		Name: "triage", Version: "1", PromptTmpl: "TRIAGE: {{.TaskDescription}}",
		ParseJSON: func(b []byte) (TriageOutput, error) {
			var o TriageOutput
			return o, json.Unmarshal(b, &o)
		},
		Model: fake,
	}
	out, _, err := s.Run(context.Background(), TriageInput{TaskDescription: "fix login"})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Startable || out.SuggestedType != "bugfix" {
		t.Fatalf("parsed wrong: %+v", out)
	}
}

func TestSkillRunParseError(t *testing.T) {
	fake := model.NewFake(map[string]string{"X:": "not-json"})
	s := Skill[struct{}, struct{}]{
		Name: "x", PromptTmpl: "X:",
		ParseJSON: func(b []byte) (struct{}, error) {
			return struct{}{}, json.Unmarshal(b, &struct{}{}) // 故意失败
		},
		Model: fake,
	}
	if _, _, err := s.Run(context.Background(), struct{}{}); err == nil {
		t.Fatal("want parse error")
	}
}

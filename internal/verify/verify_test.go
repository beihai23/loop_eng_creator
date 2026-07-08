package verify

import (
	"context"
	"testing"
)

func TestDeterministicPass(t *testing.T) {
	d := Deterministic{Label: "true", Cmd: []string{"true"}}
	r, err := d.Check(context.Background(), "", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if !r.Passed {
		t.Fatal("true should pass")
	}
}

func TestDeterministicFail(t *testing.T) {
	d := Deterministic{Label: "false", Cmd: []string{"false"}}
	r, _ := d.Check(context.Background(), "", nil, "")
	if r.Passed {
		t.Fatal("false should fail")
	}
	if r.Detail == "" {
		t.Fatal("want detail on fail")
	}
}

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

func TestChainShortCircuitsOnTier1Fail(t *testing.T) {
	fail := Deterministic{Label: "tests", Cmd: []string{"false"}}
	called := false
	t2 := tierSpy{called: &called}
	res, _ := Chain(context.Background(), []Tier{fail, t2}, "", nil, "")
	if res.Passed {
		t.Fatal("should fail")
	}
	if called {
		t.Fatal("tier2 must not run when tier1 fails")
	}
}

func TestChainPassesWhenAllPass(t *testing.T) {
	ok := Deterministic{Label: "tests", Cmd: []string{"true"}}
	human := HumanStub{}
	res, _ := Chain(context.Background(), []Tier{ok, human}, "", nil, "")
	if !res.Passed {
		t.Fatal("ok+tier3-stub should pass (stub doesn't block in M1 chain)")
	}
}

type tierSpy struct{ called *bool }

func (s tierSpy) Check(context.Context, string, []string, string) (VerifyResult, error) {
	*s.called = true
	return VerifyResult{Passed: true}, nil
}

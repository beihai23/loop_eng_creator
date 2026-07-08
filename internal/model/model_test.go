package model

import (
	"context"
	"testing"
)

func TestFakeClientByPrefix(t *testing.T) {
	f := NewFake(map[string]string{
		"triage:": `{"startable":true,"loop_doable":true}`,
		"plan:":   `{"plan":[],"risks":[]}`,
	})
	out, usage, err := f.Call(context.Background(), "triage: 修复登录")
	if err != nil {
		t.Fatal(err)
	}
	if out != `{"startable":true,"loop_doable":true}` {
		t.Fatalf("got %q", out)
	}
	if usage.TokensIn == 0 {
		t.Fatal("usage should be nonzero")
	}
}

func TestFakeClientUnknownPrefixErrors(t *testing.T) {
	f := NewFake(map[string]string{"x:": "y"})
	if _, _, err := f.Call(context.Background(), "unknown"); err == nil {
		t.Fatal("want error for unknown prefix")
	}
}

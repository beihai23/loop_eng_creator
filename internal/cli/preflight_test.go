// internal/cli/preflight_test.go
package cli

import (
	"context"
	"strings"
	"testing"

	"loop-eng/internal/channel"
)

// fakePreflightChannel embeds *channel.Local (satisfies channel.Channel via
// promotion) and adds a Preflight method so it's a channel.Preflighter — lets
// runPreflight be exercised without a live GitHub/Linear backend.
type fakePreflightChannel struct {
	*channel.Local
	issues []channel.PreflightIssue
	err    error
}

func (f *fakePreflightChannel) Preflight(_ context.Context) ([]channel.PreflightIssue, error) {
	return f.issues, f.err
}

// TestRunPreflightSkipsNonPreflighter: a channel without prerequisites (Local)
// never implements Preflighter → runPreflight is a no-op that returns nil.
func TestRunPreflightSkipsNonPreflighter(t *testing.T) {
	if err := runPreflight(context.Background(), channel.NewLocal(t.TempDir())); err != nil {
		t.Fatalf("Local has no preflight; want nil, got %v", err)
	}
}

// TestRunPreflightReady: Preflighter returning no issues → nil (daemon proceeds).
func TestRunPreflightReady(t *testing.T) {
	ch := &fakePreflightChannel{Local: channel.NewLocal(t.TempDir())}
	if err := runPreflight(context.Background(), ch); err != nil {
		t.Fatalf("ready channel must not block startup, got %v", err)
	}
}

// TestRunPreflightIssuesRefuseStart: the #65 gate. Missing prerequisites must
// (a) return a non-nil error so cobra exits non-zero / daemon refuses to start,
// and (b) carry a checklist naming each issue's Code + Message verbatim so the
// operator sees exactly what to fix.
func TestRunPreflightIssuesRefuseStart(t *testing.T) {
	ch := &fakePreflightChannel{
		Local: channel.NewLocal(t.TempDir()),
		issues: []channel.PreflightIssue{
			{Code: "missing-label", Target: "loop:running", Message: "缺 loop:running"},
			{Code: "missing-project", Target: "ghost", Message: "project 不存在"},
		},
	}
	err := runPreflight(context.Background(), ch)
	if err == nil {
		t.Fatal("missing prerequisites must refuse startup (non-nil error), got nil")
	}
	msg := err.Error()
	for _, want := range []string{"missing-label", "缺 loop:running", "missing-project", "project 不存在", "2 项"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("checklist error missing %q:\n%s", want, msg)
		}
	}
}

// TestRunPreflightHardErrorWrapped: a Preflight that can't even run (infra
// failure) surfaces as a wrapped error, distinct from a config-gap checklist.
func TestRunPreflightHardErrorWrapped(t *testing.T) {
	ch := &fakePreflightChannel{
		Local: channel.NewLocal(t.TempDir()),
		err:   context.DeadlineExceeded,
	}
	err := runPreflight(context.Background(), ch)
	if err == nil || !strings.Contains(err.Error(), "preflight:") {
		t.Fatalf("hard error must be wrapped with `preflight:`, got %v", err)
	}
}

// TestFormatPreflightIssuesRendersChecklist: every issue becomes one indented
// bullet carrying its Code and Message — the format daemon and doctor share.
func TestFormatPreflightIssuesRendersChecklist(t *testing.T) {
	out := formatPreflightIssues([]channel.PreflightIssue{
		{Code: "auth", Message: "key bad"},
		{Code: "missing-label", Message: "no loop:running"},
	})
	for _, want := range []string{"[auth] key bad", "[missing-label] no loop:running"} {
		if !strings.Contains(out, want) {
			t.Fatalf("checklist missing %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "\n") != 2 {
		t.Fatalf("want one bullet per issue (2 lines), got:\n%s", out)
	}
}

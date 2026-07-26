// internal/cli/preflight_test.go
package cli

import (
	"context"
	"strings"
	"testing"

	"loop-eng/internal/channel"
	"loop-eng/internal/config"
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

// TestProviderPreflight: doctor's provider gate surfaces a role whose binary is
// missing (here: execute → codex with a nonexistent binary path), while the
// claude-backed roles pass. Each issue is role-prefixed and tagged [provider] so
// the operator can see exactly which provider/binary to install.
func TestProviderPreflight(t *testing.T) {
	cfg := &config.Config{Models: config.Models{
		Triage:  config.ModelRef{Provider: "", Binary: "claude"},
		Plan:    config.ModelRef{Provider: "", Binary: "claude"},
		Execute: config.ModelRef{Provider: "codex", Binary: "/no/such/codex-bin-zzz"},
		Verify:  config.ModelRef{Provider: "", Binary: "claude"},
	}}
	issues := providerPreflight(cfg)
	if len(issues) == 0 {
		t.Fatal("providerPreflight must flag the codex role with a missing binary")
	}
	var found bool
	for _, msg := range issues {
		if strings.Contains(msg, "execute") && strings.Contains(msg, "codex") {
			found = true
		}
	}
	if !found {
		t.Fatalf("providerPreflight must report models.execute (codex) missing; got %v", issues)
	}
}

// TestProviderPreflightUnknownProvider: an unregistered provider on a role is
// reported (NewAgent error), not silently dropped — doctor must tell the
// operator the provider name is wrong.
func TestProviderPreflightUnknownProvider(t *testing.T) {
	cfg := &config.Config{Models: config.Models{
		Plan: config.ModelRef{Provider: "no-such-provider-zzz", Binary: "no-such-provider-zzz"},
	}}
	issues := providerPreflight(cfg)
	// The unset roles default to claude; when claude is not on PATH (e.g. the CI
	// runner) they also emit "binary not found" issues. Search the whole list for
	// the plan/unknown-provider issue instead of indexing [0], so the test does
	// not depend on claude being installed — mirrors TestProviderPreflight's loop.
	var found bool
	for _, msg := range issues {
		if strings.Contains(msg, "plan") && strings.Contains(msg, "no-such-provider-zzz") {
			found = true
		}
	}
	if !found {
		t.Fatalf("providerPreflight must report models.plan (no-such-provider-zzz) as unknown; got %v", issues)
	}
}

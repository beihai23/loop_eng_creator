package cli

import (
	"strings"
	"testing"
)

// TestTier1PRTitle pins the title computation handed to createPR: issue Title
// wins (DoD1); empty Title falls back to the distilled body first line
// (DoD2 regression — identical to the pre-fix Description path); both empty
// falls back to the synthetic ref string.
func TestTier1PRTitle(t *testing.T) {
	cases := []struct{ title, desc, ref, want string }{
		{"Fix login bug", "- bullet from # 目标 section", "74", "Fix login bug"},
		{"", "fix login", "74", "fix login"},
		{"", "", "74", "loop-eng task #74"},
	}
	for _, c := range cases {
		got := prTitleFor(c.title, c.desc, c.ref)
		if got != c.want {
			t.Fatalf("prTitleFor(%q,%q,%q)=%q want %q", c.title, c.desc, c.ref, got, c.want)
		}
	}
}

// TestTier1PRBody pins that the PR body carries the issue title above the
// Closes line, and that an empty title does not prepend a blank line.
func TestTier1PRBody(t *testing.T) {
	b := prBodyFor("Fix login bug", "74")
	if !strings.Contains(b, "Fix login bug") || !strings.Contains(b, "Closes #74") {
		t.Fatalf("body should carry title + Closes: %q", b)
	}
	b2 := prBodyFor("", "74")
	if !strings.Contains(b2, "Closes #74") {
		t.Fatalf("empty-title body must still Closes: %q", b2)
	}
	if strings.HasPrefix(b2, "\n") {
		t.Fatalf("empty-title body must not start with a blank line: %q", b2)
	}
}

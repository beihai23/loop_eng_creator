package channel

import (
	"context"
	"strconv"
	"sync"
	"testing"
)

// TestRefCapRotatesNoStarvation pins the rotation fix: when refs exceed
// maxRefsPerTick, successive calls cover DIFFERENT windows so the overflow is
// eventually polled — NOT starved. A fixed refs[:cap] truncation cut the same
// tail every tick (the refs come from an ordered query), so those refs were
// never polled. With N=cap+5, two ticks together must poll every ref.
func TestRefCapRotatesNoStarvation(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]bool{}
	g := &GitHub{Repo: "owner/repo", TaskLabel: "loop:task", ghFunc: func(_ context.Context, args ...string) ([]byte, error) {
		// gh args for GetTaskStates: ["issue","view",<ref>,"--repo",...]
		if len(args) > 2 {
			mu.Lock()
			seen[args[2]] = true
			mu.Unlock()
		}
		return []byte(`{"state":"OPEN","labels":[]}`), nil
	}}
	n := maxRefsPerTick + 5
	refs := make([]string, n)
	for i := range refs {
		refs[i] = strconv.Itoa(i + 1)
	}
	// tick 1: first window (cap refs).
	if _, err := g.GetTaskStates(context.Background(), refs); err != nil {
		t.Fatal(err)
	}
	// tick 2: rotated window — the +5 overflow must now be covered.
	if _, err := g.GetTaskStates(context.Background(), refs); err != nil {
		t.Fatal(err)
	}
	// Across the two ticks, EVERY ref must have been polled at least once.
	for _, r := range refs {
		if !seen[r] {
			t.Fatalf("ref %s never polled across 2 ticks — rotation starved the overflow (cap=%d, n=%d)", r, maxRefsPerTick, n)
		}
	}
}

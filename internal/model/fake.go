package model

import (
	"context"
	"errors"
	"strings"
)

// FakeClient is a test stub that selects a canned response by prompt prefix.
// It is safe for concurrent use only when no goroutine mutates byPrefix after
// construction; tests build the map once via NewFake and then only read.
type FakeClient struct {
	byPrefix map[string]string
	calls    int
}

// NewFake returns a FakeClient whose Call returns the value mapped to the
// first matching prefix of the prompt. Unknown prompts yield an error.
func NewFake(byPrefix map[string]string) *FakeClient {
	return &FakeClient{byPrefix: byPrefix}
}

// Call implements Client. The first prefix (by Go map iteration order) that
// matches the prompt wins; usage is derived from prompt/output lengths so
// tests can assert non-zero accounting without a real model.
func (f *FakeClient) Call(_ context.Context, prompt string) (string, Usage, error) {
	f.calls++
	for prefix, out := range f.byPrefix {
		if strings.HasPrefix(prompt, prefix) {
			return out, Usage{TokensIn: len(prompt), TokensOut: len(out)}, nil
		}
	}
	return "", Usage{}, errors.New("fake: no matching prefix for prompt")
}

// Exec delegates to Call, ignoring worktreeDir (tests don't execute real agents).
func (f *FakeClient) Exec(_ context.Context, _ string, prompt string) (string, Usage, error) {
	return f.Call(context.Background(), prompt)
}

package model

import (
	"context"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// APIClient talks to the Anthropic Messages API via the official SDK.
// It is intended for the triage and plan stages, where a named model
// (ModelRef) is selected by the caller. The Key field is retained for
// introspection; it is also forwarded to the SDK at construction so the
// client is usable without relying on ambient ANTHROPIC_API_KEY.
type APIClient struct {
	Name  string // model identifier (e.g. "claude-sonnet-5")
	Key   string // ANTHROPIC_API_KEY
	inner *anthropic.Client
}

// NewAPIClient builds an APIClient bound to the given model name and key.
// The key is passed through option.WithAPIKey; an empty key falls back to
// the SDK's environment default (ANTHROPIC_API_KEY).
func NewAPIClient(name, key string) *APIClient {
	opts := []option.RequestOption{}
	if key != "" {
		opts = append(opts, option.WithAPIKey(key))
	}
	c := anthropic.NewClient(opts...)
	return &APIClient{Name: name, Key: key, inner: &c}
}

// Call implements Client by issuing a single non-streaming Messages request.
// Text content blocks are concatenated into the output; non-text blocks
// (thinking, tool_use, etc.) are skipped. MaxTokens is set to 4096, which is
// sufficient for triage/plan JSON and keeps cost predictable.
func (a *APIClient) Call(ctx context.Context, prompt string) (string, Usage, error) {
	resp, err := a.inner.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(a.Name),
		MaxTokens: 4096,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
		},
	})
	if err != nil {
		return "", Usage{}, err
	}
	out := ""
	for _, b := range resp.Content {
		if b.Type == "text" {
			out += b.Text
		}
	}
	return out, Usage{
		TokensIn:  int(resp.Usage.InputTokens),
		TokensOut: int(resp.Usage.OutputTokens),
	}, nil
}

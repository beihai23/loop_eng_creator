package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// APIClient is a lightweight direct-LLM client: ONE HTTP POST to an
// Anthropic-compatible /v1/messages endpoint per Call (no SDK, net/http only).
//
// Why it exists (决策 J, supersedes the all-claude-p 裁决 I for plan/verify):
// ClaudeClient shells out to the `claude` CLI, which wraps every call in a full
// agentic Claude Code session (extended thinking + harness + plugins/hooks).
// For plan/verify — skills that only need a single text response — that
// overhead ballooned a ~17s / 406-token request into a 2-minute, high-traffic
// one that tripped GLM 529 "该模型访问量过大". A direct /v1/messages call stays
// at ~17s / 406 tokens. It reuses the operator's existing credentials from the
// environment (ANTHROPIC_BASE_URL + ANTHROPIC_AUTH_TOKEN) — no new key, no SDK.
//
// execute keeps ClaudeClient: it genuinely needs the agentic harness (to edit
// files, run go test). Routing only the single-response skills through the API
// cuts per-attempt traffic from 3 heavy claude -p calls to 1.
type APIClient struct {
	BaseURL string // env ANTHROPIC_BASE_URL (e.g. https://open.bigmodel.cn/api/anthropic)
	APIKey  string // env ANTHROPIC_AUTH_TOKEN
	Model   string // from config (e.g. glm-5.2)
	client  *http.Client
}

// NewAPIClient builds an APIClient for modelName, reading endpoint + key from
// the environment (the same env the `claude` CLI uses, so no extra config).
func NewAPIClient(modelName string) *APIClient {
	return &APIClient{
		BaseURL: envDefault("ANTHROPIC_BASE_URL", "https://api.anthropic.com"),
		APIKey:  os.Getenv("ANTHROPIC_AUTH_TOKEN"),
		Model:   modelName,
		client:  &http.Client{Timeout: 120 * time.Second},
	}
}

func envDefault(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// Call implements Client: one POST {BaseURL}/v1/messages → text + usage.
func (a *APIClient) Call(ctx context.Context, prompt string) (string, Usage, error) {
	if a.APIKey == "" {
		return "", Usage{}, fmt.Errorf("APIClient: ANTHROPIC_AUTH_TOKEN env not set")
	}
	body, _ := json.Marshal(map[string]any{
		"model":      a.Model,
		"max_tokens": 4096,
		"messages":   []map[string]string{{"role": "user", "content": prompt}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.BaseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", Usage{}, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("x-api-key", a.APIKey)
	req.Header.Set("authorization", "Bearer "+a.APIKey)
	resp, err := a.client.Do(req)
	if err != nil {
		return "", Usage{}, fmt.Errorf("APIClient: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", Usage{}, fmt.Errorf("APIClient http %d: %s", resp.StatusCode, snip(string(respBody)))
	}
	var msg struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &msg); err != nil {
		return "", Usage{}, fmt.Errorf("APIClient decode: %w", err)
	}
	out := ""
	for _, b := range msg.Content {
		if b.Type == "text" {
			out += b.Text
		}
	}
	return out, Usage{TokensIn: msg.Usage.InputTokens, TokensOut: msg.Usage.OutputTokens}, nil
}

func snip(s string) string {
	if len(s) > 300 {
		return s[:300]
	}
	return s
}

var _ Client = (*APIClient)(nil)

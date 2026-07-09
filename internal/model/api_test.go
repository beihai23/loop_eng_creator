package model

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAPIClientParsesResponse: a mocked /v1/messages endpoint — APIClient must
// POST to /v1/messages, send x-api-key, and parse content[].text + usage.
func TestAPIClientParsesResponse(t *testing.T) {
	var gotPath, gotKey, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-api-key")
		gotAuth = r.Header.Get("authorization")
		fmt.Fprint(w, `{"content":[{"type":"text","text":"{\"plan\":[]}"}],"usage":{"input_tokens":10,"output_tokens":5}}`)
	}))
	defer srv.Close()
	c := &APIClient{BaseURL: srv.URL, APIKey: "test-key", Model: "m", client: srv.Client()}
	out, u, err := c.Call(context.Background(), "plan me")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/messages" {
		t.Fatalf("expected POST /v1/messages, got %s", gotPath)
	}
	if gotKey != "test-key" || gotAuth != "Bearer test-key" {
		t.Fatalf("auth headers wrong: x-api-key=%q authorization=%q", gotKey, gotAuth)
	}
	if out != "{\"plan\":[]}" {
		t.Fatalf("output %q", out)
	}
	if u.TokensIn != 10 || u.TokensOut != 5 {
		t.Fatalf("usage %+v", u)
	}
}

// TestAPIClientErrorOnHTTP400: a non-2xx response surfaces as an error
// carrying the status (so callers see the real failure, not a silent empty).
func TestAPIClientErrorOnHTTP400(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"type":"bad_model","message":"model not found"}}`)
	}))
	defer srv.Close()
	c := &APIClient{BaseURL: srv.URL, APIKey: "k", Model: "m", client: srv.Client()}
	_, _, err := c.Call(context.Background(), "x")
	if err == nil {
		t.Fatal("want error on HTTP 400")
	}
}

// TestAPIClientMissingKey: no ANTHROPIC_AUTH_TOKEN → clear error before any HTTP.
func TestAPIClientMissingKey(t *testing.T) {
	c := &APIClient{BaseURL: "http://example.invalid", APIKey: "", Model: "m", client: http.DefaultClient}
	if _, _, err := c.Call(context.Background(), "x"); err == nil {
		t.Fatal("want error when APIKey empty")
	}
}

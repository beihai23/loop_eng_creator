package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"loop-eng/internal/state"
	"loop-eng/internal/web"
)

// TestWebCmdFlagsAndRegistration checks the cobra wiring: default flag values
// and that `web` is registered under the root command.
func TestWebCmdFlagsAndRegistration(t *testing.T) {
	cmd := NewWebCmd()
	if addr, err := cmd.Flags().GetString("addr"); err != nil || addr != "127.0.0.1:7474" {
		t.Fatalf("default --addr=%q err=%v, want 127.0.0.1:7474 (loopback)", addr, err)
	}
	if repo, err := cmd.Flags().GetString("repo"); err != nil || repo != "." {
		t.Fatalf("default --repo=%q err=%v, want '.'", repo, err)
	}
	// `web` must appear among the root subcommands.
	for _, c := range NewRootCmd().Commands() {
		if c.Name() == "web" {
			return
		}
	}
	t.Fatal("root command missing 'web' subcommand")
}

// TestWebServerIntegration spins a real HTTP server (httptest, not a raw
// listener) over a temp repo's state DB and asserts the two tier-1 endpoints
// the acceptance criteria pin: GET / (200 text/html, body names loop-eng) and
// GET /api/overview (200 application/json).
func TestWebServerIntegration(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	if err := os.MkdirAll(filepath.Join(repo, ".loop"), 0755); err != nil {
		t.Fatal(err)
	}
	// defaultConfig (package var, init.go) — same scaffold a real `loop-eng init`
	// writes; mustLoad validates its providers.
	if err := os.WriteFile(filepath.Join(repo, ".loop", "config.yaml"), []byte(defaultConfig), 0644); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(filepath.Join(repo, ".loop", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := mustLoad(repo)
	srv := httptest.NewServer(web.New(st, cfg).Handler())
	t.Cleanup(srv.Close)

	// GET / → 200 + text/html + body contains 'loop-eng'.
	res, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET / status=%d want 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("GET / content-type=%q want text/html…", ct)
	}
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), "loop-eng") {
		t.Fatalf("GET / body missing 'loop-eng': %s", body)
	}
	// the overview container is present (id contains 'overview').
	if !strings.Contains(string(body), "overview") {
		t.Fatalf("GET / body missing overview container: %s", body)
	}

	// GET /api/overview → 200 + application/json.
	res2, err := http.Get(srv.URL + "/api/overview")
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/overview status=%d want 200", res2.StatusCode)
	}
	if ct := res2.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("GET /api/overview content-type=%q want application/json", ct)
	}
}

// TestWebCmdHelpExitsZero pins the `loop-eng web --help` contract: Execute
// returns nil (→ exit 0) and the help text mentions 'web'.
func TestWebCmdHelpExitsZero(t *testing.T) {
	cmd := NewRootCmd()
	cmd.SetArgs([]string{"web", "--help"})
	buf := &strings.Builder{}
	// cobra's child command resolves Out/Err by walking to the root's writer,
	// so setting them here is enough to capture the web subcommand's help.
	cmd.SetOut(buf)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("web --help returned err=%v want nil (exit 0)", err)
	}
	if !strings.Contains(buf.String(), "web") {
		t.Fatalf("web --help output missing 'web': %q", buf.String())
	}
}

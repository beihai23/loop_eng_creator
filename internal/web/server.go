// Package web serves a read-only HTTP dashboard over a loop-eng state DB.
//
// It is the web counterpart of the TUI `loop-eng dashboard`: a vanilla-JS SPA
// embedded via go:embed (no npm, no build step, no external CDN), JSON read
// handlers backed entirely by state.Store read methods, and a single control
// endpoint (POST /api/tasks/{id}/command) that writes resume/cancel rows the
// daemon's drainCommands step picks up — exactly the same control channel the
// TUI uses. Zero third-party deps: net/http + embed only.
package web

import (
	"embed"
	"io/fs"
	"net/http"

	"loop-eng/internal/config"
	"loop-eng/internal/state"
)

// staticFiles holds the embedded frontend (index.html / app.js / style.css) so
// the loop-eng binary is fully self-contained — same //go:embed + embed.FS
// pattern as internal/cli/init.go's embedded skills.
//
//go:embed static/*
var staticFiles embed.FS

// Server is the loop-eng web dashboard. It holds a read handle to the durable
// state DB and, optionally, the repo config used to derive the verify-tier
// labels shown on the detail view. cfg may be nil — handlers tolerate it
// (tier-2/3 labels simply fall back to defaults). The only write path is the
// command endpoint (InsertCommand); the daemon remains the single writer that
// moves task state. The DB opens with journal_mode=WAL + busy_timeout (see
// state.Open), so this long-lived reader never blocks the daemon writer.
type Server struct {
	st  *state.Store
	cfg *config.Config
	mux *http.ServeMux
}

// New wires a Server to st (read source) and cfg (optional, for verify-tier
// labels). It registers all routes on an internal ServeMux; Handler() returns
// that mux so callers — the `web` cobra command (real listener) or tests
// (httptest.NewServer) — can mount it without binding a port.
//
// Routes use the Go 1.22+ enhanced ServeMux ("METHOD /path/{id}"):
//
//	GET  /                         -> index.html
//	GET  /static/*                 -> embedded assets (app.js, style.css)
//	GET  /api/overview             -> {tasks, counts, running}
//	GET  /api/tasks/{id}           -> task detail (404 when absent)
//	GET  /api/tasks/{id}/trace     -> per-run timeline (steps/verifications/budget)
//	POST /api/tasks/{id}/command   -> {verb: resume|cancel} -> commands row
func New(st *state.Store, cfg *config.Config) *Server {
	s := &Server{st: st, cfg: cfg}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("GET /api/overview", s.handleOverview)
	mux.HandleFunc("GET /api/tasks/{id}", s.handleDetail)
	mux.HandleFunc("GET /api/tasks/{id}/trace", s.handleTrace)
	mux.HandleFunc("POST /api/tasks/{id}/command", s.handleCommand)

	// Static assets served from the embedded FS — the binary needs nothing
	// beside itself to render the dashboard.
	if sub, err := fs.Sub(staticFiles, "static"); err == nil {
		mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(sub)))
	}

	s.mux = mux
	return s
}

// Handler returns the configured mux as an http.Handler.
func (s *Server) Handler() http.Handler { return s.mux }

// ListenAndServe serves the dashboard on addr, blocking until the listener
// returns. Used by the `loop-eng web` command.
func (s *Server) ListenAndServe(addr string) error {
	return http.ListenAndServe(addr, s.mux)
}

// handleIndex serves the embedded SPA shell. ServeMux "GET /" is the catch-all
// root, so an unexpected path here is treated as 404 rather than silently
// returning the SPA — keeps unknown routes honest (and matches the contract's
// "unknown route -> 404").
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := staticFiles.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "index not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

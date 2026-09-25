package setupui

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop"
	"github.com/authzed/openagentprimitives/pkg/x/safehttp"
)

// staticFS embeds the self-contained setup page (inline CSS/JS, no
// external/CDN resources — it must work fully offline behind the loopback
// server and inside a webview). See static/index.html for the page's own
// notes on how it consumes the routes below.
//
//go:embed static/index.html
var staticFS embed.FS

// OnConfigFunc is invoked with a validated desktop.Config when the setup
// form is submitted via POST /config, plus the plaintext admin password
// submitted alongside it. password is carried as a SEPARATE argument
// rather than a Config field on purpose: desktop.Config is what gets
// persisted to config.json (see desktop.Config.Save) and is also what a
// later Timeline/log line might stringify for debugging — a plaintext
// field living on that struct is one accidental %+v away from landing on
// disk or in a log. The implementation (onConfigSubmit) hashes password
// via passwordkind.HashPassword and sets the result on
// Config.AdminPasswordHash before ever persisting or logging anything.
// An error means the config could not be applied (e.g. an empty password,
// or persisting it to disk failed); the HTTP layer surfaces that as a 500
// response to the caller.
type OnConfigFunc func(cfg desktop.Config, password string) error

// OnOpenDashboardFunc is invoked when the ready screen's "Open dashboard"
// button POSTs to /open-dashboard. A webview page has no shell access, so
// actually opening a browser window (or focusing an existing one) has to
// happen on the Go side; an error means that failed and is surfaced to the
// caller as a 500.
type OnOpenDashboardFunc func() error

// configRequest is the POST /config body: the shape of desktop.Config's
// Model field, flattened (no first-run form field exists yet for Channel
// or Ngrok — see desktop.knownExternalChannelKinds, which is empty today),
// plus the plaintext admin Password. Password is deliberately never copied
// onto a desktop.Config anywhere in this file — see OnConfigFunc's doc —
// it is read here and handed straight to onConfig as its own argument.
type configRequest struct {
	Provider string `json:"provider"`
	APIKey   string `json:"apiKey"`
	// Model is optional: blank means the selected provider's recommended
	// model (see desktop.Config.EffectiveModel).
	Model    string `json:"model"`
	Password string `json:"password"`
}

// Server is the setup UI's local HTTP server: it serves the (placeholder,
// for now) setup page, exposes a Timeline over SSE (/events) and one-shot
// JSON (/progress), and accepts the first-run config form (/config). It
// binds to 127.0.0.1 only — see Start.
type Server struct {
	timeline        *Timeline
	onConfig        OnConfigFunc
	onOpenDashboard OnOpenDashboardFunc

	ln      net.Listener
	httpSrv *http.Server
	errCh   chan error
}

// New builds a Server over timeline. onConfig may be nil (POST /config
// still validates and returns 200, but nothing is invoked) — this is
// mainly a testing convenience; production wiring always supplies one.
func New(timeline *Timeline, onConfig OnConfigFunc) *Server {
	return &Server{
		timeline: timeline,
		onConfig: onConfig,
		errCh:    make(chan error, 1),
	}
}

// SetOnOpenDashboard registers the callback invoked by POST
// /open-dashboard (see OnOpenDashboardFunc). Optional: with none
// registered, the route responds 404 — this mirrors onConfig's nil-is-fine
// testing convenience, since production wiring (a later task) always sets
// one. Call before Start; the field is read without a lock, matching
// onConfig's own set-once-before-serving treatment.
func (s *Server) SetOnOpenDashboard(fn OnOpenDashboardFunc) {
	s.onOpenDashboard = fn
}

// Start binds a loopback-only listener on 127.0.0.1:port (port 0 = OS
// picks one; read it back via Addr/URL) and begins serving in the
// background. It never binds 0.0.0.0 or any other interface — the address
// is hard-coded to the 127.0.0.1 literal, not merely documented as such,
// so a caller cannot accidentally widen it by passing a different host.
func (s *Server) Start(port int) error {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("setupui: listen: %w", err)
	}
	s.ln = ln

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/events", s.handleEvents)
	mux.HandleFunc("/progress", s.handleProgress)
	mux.HandleFunc("/config", s.handleConfig)
	mux.HandleFunc("/open-dashboard", s.handleOpenDashboard)

	srv := &http.Server{Handler: desktop.LoopbackGuard(mux)}
	safehttp.HardenServer(srv)
	s.httpSrv = srv

	go func() {
		if serveErr := srv.Serve(ln); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			select {
			case s.errCh <- serveErr:
			default:
			}
		}
	}()
	return nil
}

// Errors returns a channel that receives at most one unexpected serve
// error. A graceful Shutdown/Close never sends on it (http.ErrServerClosed
// is filtered out) — callers select on this alongside their own lifecycle
// context, mirroring the loopback-server error-channel pattern in
// cmd/oap/internal/clilogin/login.go.
func (s *Server) Errors() <-chan error { return s.errCh }

// Addr returns the bound "127.0.0.1:port" address. Valid only after Start.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// URL returns the server's base URL. Valid only after Start.
func (s *Server) URL() string { return "http://" + s.Addr() }

// Shutdown gracefully stops the server, waiting for in-flight requests
// (including long-lived /events SSE connections, which observe ctx
// cancellation via their handler's request context) to finish or ctx to
// expire.
func (s *Server) Shutdown(ctx context.Context) error { return s.httpSrv.Shutdown(ctx) }

// Close stops the server immediately, without waiting for in-flight
// requests.
func (s *Server) Close() error { return s.httpSrv.Close() }

// handleIndex serves the setup page at the exact path "/".
// http.ServeMux's "/" pattern otherwise matches every unregistered path,
// so this checks the path itself and 404s anything else.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "setupui: index page missing (embed error)", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}

// handleProgress is the one-shot JSON poll fallback for clients that don't
// use /events.
func (s *Server) handleProgress(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.timeline.Snapshot())
}

// handleEvents streams TimelineState snapshots over Server-Sent Events:
// the current snapshot immediately on connect, then a fresh one on every
// Timeline mutation, until the client disconnects (request context done)
// or the Timeline's subscriber channel is torn down.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "setupui: streaming unsupported", http.StatusInternalServerError)
		return
	}

	// Subscribe BEFORE writing the initial snapshot so a mutation racing
	// the handshake is never missed between the two.
	ch, cancel := s.timeline.Subscribe()
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	write := func(st TimelineState) bool {
		data, err := json.Marshal(st)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return false
		}
		fl.Flush()
		return true
	}

	if !write(s.timeline.Snapshot()) {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case st, ok := <-ch:
			if !ok {
				return
			}
			if !write(st) {
				return
			}
		}
	}
}

// handleConfig accepts the first-run config form: {"provider":...,
// "apiKey":..., "model":..., "password":...}, where "model" is optional (see
// configRequest). It validates via desktop.Config.Validate()
// (the same fail-closed known-provider gate the CLI's desktop.Load/Save
// path uses — see cmd/oap/internal/desktop/config.go) and, once valid,
// invokes s.onConfig with the plaintext password kept OUT of the
// desktop.Config value (see OnConfigFunc's doc — onConfig hashes it before
// anything is persisted or logged). Returns 400 on a decode or validation
// failure, 500 if onConfig itself errors (which includes an empty/invalid
// password — onConfig is the one that checks that), 200 on success.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req configRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("decode request body: %v", err), http.StatusBadRequest)
		return
	}

	cfg := desktop.Config{Model: desktop.ModelConfig{Provider: req.Provider, APIKey: req.APIKey, Name: req.Model}}
	if err := cfg.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if s.onConfig != nil {
		if err := s.onConfig(cfg, req.Password); err != nil {
			http.Error(w, fmt.Sprintf("apply config: %v", err), http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleOpenDashboard accepts the ready screen's "Open dashboard" button
// POST. It has no body. 404 (no-op) when no OnOpenDashboardFunc has been
// registered via SetOnOpenDashboard, 500 if the callback errors, 200 on
// success.
func (s *Server) handleOpenDashboard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.onOpenDashboard == nil {
		http.NotFound(w, r)
		return
	}
	if err := s.onOpenDashboard(); err != nil {
		http.Error(w, fmt.Sprintf("open dashboard: %v", err), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

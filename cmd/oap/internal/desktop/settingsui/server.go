package settingsui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
	"github.com/authzed/openagentprimitives/pkg/web/webui/webassets"
	"github.com/authzed/openagentprimitives/pkg/x/safehttp"
)

// defaultPollInterval is how often GET /api/events polls Deps.State for a
// change, absent a test override (see Server.pollInterval).
const defaultPollInterval = 2 * time.Second

// keepaliveInterval is how often GET /api/events writes an SSE comment line
// to keep an idle connection (and any intermediary) from timing it out.
const keepaliveInterval = 25 * time.Second

// Server is the desktop settings server: a loopback-only, token-gated HTTP
// server that serves the "settings" web UI app shell and its small
// JSON/SSE API. Bind with Start; New only constructs and mints the launch
// token.
type Server struct {
	deps  Deps
	token string

	// appKey is the webassets manifest entry rendered at GET /. It defaults to
	// the built "settings" entry; it stays overridable (lowercase, unexported —
	// tests only) so a test can point the render path at a different existing
	// entry without shipping a second app.
	appKey string

	// pollInterval is how often GET /api/events re-reads Deps.State.
	// Defaults to defaultPollInterval; tests shorten it so the SSE test
	// doesn't need to wait 2s per assertion.
	pollInterval time.Duration

	// applyFn performs the SSA apply of the ClusterAgentSettings CR (see
	// cluster.go's applyClusterSettings). Defaults to kube.Apply; the
	// cluster-settings tests override it because the dynamic fake client's
	// ApplyPatchType support is too limited to exercise an apply against an
	// ALREADY-EXISTING object (see cluster_test.go's fake-apply doc).
	applyFn func(ctx context.Context, dyn dynamic.Interface, obj *unstructured.Unstructured, fieldManager string) error

	ln      net.Listener
	httpSrv *http.Server
	errCh   chan error
}

// New validates deps and mints the server's one-shot launch token. It fails
// closed on the seams a request path dereferences UNCONDITIONALLY — State,
// Logf, SupportDir, and Clients (the cluster routes call it with no nil guard,
// see cluster.go / installinfo.go) — so a missing one is a construction error
// rather than a nil-deref waiting to happen on the first request. The optional
// seams are nil-guarded at their own call sites and are not checked here.
func New(deps Deps) (*Server, error) {
	if deps.State == nil {
		return nil, errors.New("settingsui: Deps.State is required")
	}
	if deps.Logf == nil {
		return nil, errors.New("settingsui: Deps.Logf is required")
	}
	if deps.SupportDir == "" {
		return nil, errors.New("settingsui: Deps.SupportDir is required")
	}
	if deps.Clients == nil {
		return nil, errors.New("settingsui: Deps.Clients is required")
	}
	token, err := newToken()
	if err != nil {
		return nil, err
	}
	return &Server{
		deps:         deps,
		token:        token,
		appKey:       "settings",
		pollInterval: defaultPollInterval,
		errCh:        make(chan error, 1),
		applyFn:      kube.Apply,
	}, nil
}

// Start binds a loopback-only listener on 127.0.0.1:port (port 0 = OS
// picks one; read it back via Addr/URL) and begins serving in the
// background. It never binds 0.0.0.0 or any other interface — the address
// is hard-coded to the 127.0.0.1 literal, not merely documented as such, so
// a caller cannot accidentally widen it by passing a different host.
func (s *Server) Start(port int) error {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("settingsui: listen: %w", err)
	}
	s.ln = ln

	assetsFS, err := fs.Sub(webassets.FS(), "dist")
	if err != nil {
		return fmt.Errorf("settingsui: sub dist fs: %w", err)
	}
	assetServer := http.StripPrefix("/assets/", http.FileServerFS(assetsFS))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleIndex)
	mux.Handle("GET /assets/", s.withImmutableCache(assetServer))
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("GET /api/config", s.handleConfigGet)
	mux.HandleFunc("PUT /api/config", s.handleConfigPut)
	mux.HandleFunc("GET /api/cluster/settings", s.handleClusterSettingsGet)
	mux.HandleFunc("POST /api/cluster/settings/validate", s.handleClusterSettingsValidate)
	mux.HandleFunc("PUT /api/cluster/settings", s.handleClusterSettingsPut)
	mux.HandleFunc("GET /api/cluster/install-info", s.handleClusterInstallInfoGet)
	mux.HandleFunc("POST /api/kubectl/use", s.handleKubectlUse)
	mux.HandleFunc("POST /api/reveal/config", s.handleRevealConfig)
	mux.HandleFunc("POST /api/reveal/logs", s.handleRevealLogs)

	// GET /auth is the only route reachable without the session cookie — it
	// IS the credential exchange (see auth.go) — so it sits outside
	// requireAuth but still inside the loopback guard.
	authMux := http.NewServeMux()
	authMux.HandleFunc("GET /auth", s.handleAuth)
	authMux.Handle("/", s.requireAuth(mux))

	srv := &http.Server{Handler: desktop.LoopbackGuard(authMux)}
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
// context, mirroring setupui.Server.Errors.
func (s *Server) Errors() <-chan error { return s.errCh }

// Addr returns the bound "127.0.0.1:port" address. Valid only after Start.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// URL returns the one-shot launch URL: the caller opens this once to
// exchange the token for the session cookie (see handleAuth). Valid only
// after Start.
func (s *Server) URL() string {
	return fmt.Sprintf("http://%s/auth?token=%s", s.Addr(), s.token)
}

// Close stops the server immediately, without waiting for in-flight
// requests (including any open /api/events SSE connections).
func (s *Server) Close() error { return s.httpSrv.Close() }

// withImmutableCache marks a response as long-lived, content-hashed and
// immutable — safe because every asset URL under /assets/ embeds a build
// hash, so a changed file is always a changed URL.
func (s *Server) withImmutableCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		next.ServeHTTP(w, r)
	})
}

// handleIndex serves the settings app shell at the exact path "/".
// http.ServeMux's "GET /" pattern otherwise matches every unregistered
// path, so this checks the path itself and 404s anything else.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	props := map[string]string{"apiBase": "/api"}
	if err := webui.RenderStandaloneApp(w, s.appKey, "OAP Desktop Settings", props); err != nil {
		s.deps.Logf("settingsui: render index: %v", err)
		http.Error(w, "settings: failed to render", http.StatusInternalServerError)
		return
	}
}

// currentState returns Deps.State()'s snapshot overlaid with the live
// kubectl-context check from Deps.KubectlCurrent (a nil seam overlays
// false). The overlay lives here — the one place both handleState and
// handleEvents read State from — rather than inside Deps.State itself, so
// Deps.State stays a pure snapshot and kubectl-context detection has a
// single call site regardless of which route asked.
func (s *Server) currentState() State {
	st := s.deps.State()
	st.KubectlCurrent = s.deps.KubectlCurrent != nil && s.deps.KubectlCurrent()
	return st
}

// handleState serves the current desktop lifecycle snapshot as JSON.
func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(s.currentState()); err != nil {
		s.deps.Logf("settingsui: encode state: %v", err)
	}
}

// handleEvents streams State snapshots over Server-Sent Events: the current
// snapshot immediately on connect, then a fresh one whenever Deps.State's
// value changes (polled every s.pollInterval), until the client disconnects
// (request context done). A ": keepalive" comment line is written every
// keepaliveInterval so an idle connection (no state change) isn't mistaken
// for a dead one by the client or an intermediary.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "settingsui: streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	writeState := func(st State) bool {
		data, err := json.Marshal(st)
		if err != nil {
			s.deps.Logf("settingsui: marshal state for SSE: %v", err)
			return false
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	writeKeepalive := func() bool {
		if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
			return false
		}
		fl.Flush()
		return true
	}

	last := s.currentState()
	if !writeState(last) {
		return
	}

	pollTicker := time.NewTicker(s.pollInterval)
	defer pollTicker.Stop()
	keepaliveTicker := time.NewTicker(keepaliveInterval)
	defer keepaliveTicker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-pollTicker.C:
			cur := s.currentState()
			if cur != last {
				if !writeState(cur) {
					return
				}
				last = cur
			}
		case <-keepaliveTicker.C:
			if !writeKeepalive() {
				return
			}
		}
	}
}

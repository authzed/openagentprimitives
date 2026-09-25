package webui

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/x/safehttp"
)

// Server mounts registered WebUIs onto two origin muxes and dispatches
// requests by Host header.
type Server struct {
	// authFn verifies a request's session and returns the canonical subject.
	// Passed by the binary; nil means "never authenticated" (the authenticate
	// method applies that nil-check).
	authFn func(r *http.Request) (subject string, ok bool)
	// beginLogin returns the location to 302 to (identityd's /oidc/login) for an
	// unauthenticated request on an AuthLoginIfNecessary route — see wrap's
	// AuthLoginIfNecessary case. A nil beginLogin, or an (loc, false) return,
	// falls through to the 401 system page: fail-closed, never a silent pass.
	beginLogin func(r *http.Request) (loc string, ok bool)
	deps       Deps
	// trustedHost and sandboxHost are resolved PER REQUEST, not frozen at
	// construction, so dispatch follows a changing host (e.g. an ngrok URL
	// rotation) without a server rebuild. Each may return a full URL or a
	// bare host; hostOnly normalizes. An empty return ⇒ that origin is not
	// configured yet (idle-but-ready ⇒ 404, never a route).
	trustedHost func() string
	sandboxHost func() string
	// sharedOriginOK gates the INSECURE shared-origin debug mode, evaluated PER
	// REQUEST against the LIVE host: the two origins must resolve to the SAME
	// host AND the predicate must accept it, and that one host then serves BOTH
	// route sets — artifact content shares the auth origin, so the session cookie
	// is reachable by content. The binary owns the policy (webd accepts ngrok
	// hosts only). Nil ⇒ never shared: each origin serves only its own routes and
	// equal hosts fail closed, sandbox never served on the trusted host.
	// Production has distinct hosts ⇒ never shared.
	sharedOriginOK func(host string) bool
	trustedMux     *http.ServeMux
	sandboxMux     *http.ServeMux
	dev            devConfig
	manifest       manifest
}

// NewServer validates and mounts every WebUI's routes. The framework-level auth
// funcs (authenticate, beginLogin) are passed directly — they are not part of
// the opaque per-component deps.
//
// trustedHost and sandboxHost are per-request GETTERS (see the struct fields);
// both must be non-nil.
//
// The trusted/sandbox distinctness rule is enforced at DISPATCH time, not here:
// because the hosts are dynamic, equality can change between requests. See the
// sharedOriginOK field for when the two collapse onto one host.
//
// Validation: an AuthNone or AuthLoginIfNecessary route may declare only
// GET/HEAD methods.
func NewServer(
	authenticate func(r *http.Request) (subject string, ok bool),
	beginLogin func(r *http.Request) (loc string, ok bool),
	trustedHost, sandboxHost func() string,
	sharedOriginOK func(host string) bool,
	deps Deps,
	uis []WebUI,
) (*Server, error) {
	if trustedHost == nil || sandboxHost == nil {
		return nil, fmt.Errorf("webui: trustedHost and sandboxHost getters are required (got nil)")
	}
	s := &Server{
		authFn:         authenticate,
		beginLogin:     beginLogin,
		deps:           deps,
		trustedHost:    trustedHost,
		sandboxHost:    sandboxHost,
		sharedOriginOK: sharedOriginOK,
		trustedMux:     http.NewServeMux(),
		sandboxMux:     http.NewServeMux(),
	}
	if mf, err := loadManifest(); err == nil {
		s.manifest = mf
	} else {
		// No manifest (assets not built): pages render a generic 500 system page
		// rather than panic. Log so an operator sees the cause.
		fmt.Fprintln(os.Stderr, "webui: web asset manifest unavailable:", err.Error())
	}
	for _, ui := range uis {
		for _, rt := range ui.Routes(deps) {
			if err := s.mount(ui.Name(), rt); err != nil {
				return nil, err
			}
		}
	}
	// Framework-level static route for the embedded UI bundles (public, GET).
	s.trustedMux.Handle("/assets/", assetHandler())
	return s, nil
}

// SetWebDev enables dev-mode document rendering (scripts served from a Vite dev
// server at url). Off by default; internal/cmd/webd sets this from --web-dev.
//
// newDevConfig sanitizes url here, at the ONE place a live devConfig is built,
// rather than at each site that later emits it — see devConfig.
func (s *Server) SetWebDev(url string) { s.dev = newDevConfig(url) }

func (s *Server) mount(uiName string, rt Route) error {
	if (rt.Handler == nil) == (rt.Page == nil) {
		return fmt.Errorf("webui %q: route %q must set exactly one of Handler or Page", uiName, rt.Pattern)
	}
	if rt.Page != nil && rt.Page.App == "" {
		return fmt.Errorf("webui %q: route %q has a Page with an empty Page.App", uiName, rt.Pattern)
	}
	if rt.Page != nil && rt.Page.Build == nil {
		return fmt.Errorf("webui %q: route %q has a Page with a nil Build func", uiName, rt.Pattern)
	}
	methods := rt.Methods
	if len(methods) == 0 {
		methods = []string{http.MethodGet}
	}
	if rt.Auth == AuthNone || rt.Auth == AuthLoginIfNecessary {
		for _, m := range methods {
			if m != http.MethodGet && m != http.MethodHead {
				return fmt.Errorf("webui %q: route %q may not serve %s — %s routes are GET/HEAD only", uiName, rt.Pattern, m, rt.Auth)
			}
		}
	}
	if rt.Auth == AuthAuthorized && rt.Authorize == nil {
		return fmt.Errorf("webui %q: route %q is AuthAuthorized but has no Authorize func", uiName, rt.Pattern)
	}
	switch rt.Auth {
	case AuthNone, AuthAuthenticated, AuthAuthorized, AuthLoginIfNecessary, AuthHandlerManaged:
		// known levels
	default:
		return fmt.Errorf("webui %q: route %q has unknown AuthLevel %d", uiName, rt.Pattern, rt.Auth)
	}
	h := s.wrap(rt, methods)
	switch rt.Origin {
	case OriginTrusted:
		s.trustedMux.Handle(rt.Pattern, h)
	case OriginSandbox:
		s.sandboxMux.Handle(rt.Pattern, h)
	default:
		return fmt.Errorf("webui %q: route %q has unknown origin %d", uiName, rt.Pattern, rt.Origin)
	}
	return nil
}

func (s *Server) wrap(rt Route, methods []string) http.Handler {
	allowed := map[string]bool{}
	for _, m := range methods {
		allowed[m] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowed[r.Method] {
			w.Header().Set("Allow", strings.Join(methods, ", "))
			s.renderSystem(w, &PageError{Status: http.StatusMethodNotAllowed, Kind: "error",
				Title: "Method not allowed", Message: "That method is not supported here."})
			return
		}
		switch rt.Auth {
		case AuthNone, AuthHandlerManaged:
			// No framework auth: AuthNone is GET/HEAD-only (mount enforces);
			// AuthHandlerManaged permits any declared method and authenticates
			// the request itself.
			s.serveRoute(w, r, rt)
		case AuthAuthenticated:
			subject, ok := s.authenticate(r)
			if !ok {
				s.renderSystem(w, unauthorizedErr())
				return
			}
			s.serveRoute(w, r.WithContext(withSubject(r.Context(), subject)), rt)
		case AuthAuthorized:
			subject, ok := s.authenticate(r)
			if !ok {
				s.renderSystem(w, unauthorizedErr())
				return
			}
			if err := rt.Authorize(r.Context(), subject, r); err != nil {
				s.renderAuthorizeFailure(w, err)
				return
			}
			s.serveRoute(w, r.WithContext(withSubject(r.Context(), subject)), rt)
		case AuthLoginIfNecessary:
			subject, ok := s.authenticate(r)
			if ok {
				if rt.Authorize != nil {
					if err := rt.Authorize(r.Context(), subject, r); err != nil {
						s.renderAuthorizeFailure(w, err)
						return
					}
				}
				s.serveRoute(w, r.WithContext(withSubject(r.Context(), subject)), rt)
				return
			}
			if s.beginLogin != nil {
				if loc, lok := s.beginLogin(r); lok {
					http.Redirect(w, r, loc, http.StatusFound)
					return
				}
			}
			s.renderSystem(w, unauthorizedErr()) // fail-closed
		default:
			// Unreachable in practice (mount rejects unknown AuthLevel); fail-closed.
			s.renderSystem(w, &PageError{Status: http.StatusInternalServerError, Kind: "error",
				Title: "Internal error", Message: "Unexpected auth configuration."})
		}
	})
}

// unauthorizedErr is the shared 401 system page (sign-in required).
func unauthorizedErr() *PageError {
	return &PageError{Status: http.StatusUnauthorized, Kind: "unauthorized",
		Title: "Sign in required", Message: "You must be signed in to view this page."}
}

// renderAuthorizeFailure maps an Authorize error to a response: a *PageError
// keeps its own status (lets authorizers distinguish a SpiceDB ERROR (500) from
// a denial (403) — per the no-silent-errors rule an infra failure must never
// read as "denied"); anything else is the standard 403.
func (s *Server) renderAuthorizeFailure(w http.ResponseWriter, err error) {
	var pe *PageError
	if errors.As(err, &pe) {
		s.renderSystem(w, pe)
		return
	}
	s.renderSystem(w, &PageError{Status: http.StatusForbidden, Kind: "forbidden",
		Title: "Access denied", Message: "You do not have access to this resource."})
}

// serveRoute dispatches a wrapped route to either its raw handler or its Page
// renderer (auth subject already injected into ctx by the caller).
func (s *Server) serveRoute(w http.ResponseWriter, r *http.Request, rt Route) {
	if rt.Page == nil {
		rt.Handler.ServeHTTP(w, r)
		return
	}
	nonce, err := newNonce()
	if err != nil {
		s.renderSystem(w, &PageError{Status: http.StatusInternalServerError, Kind: "error",
			Title: "Internal error", Message: "Could not generate a security nonce."})
		return
	}
	props, meta, berr := rt.Page.Build(r.Context(), r)
	if berr != nil {
		s.renderSystem(w, toPageError(berr))
		return
	}
	// A manifest miss (unregistered app, or the manifest failed to load at
	// startup) must not fall through to renderDocument with a zero-value entry:
	// Scripts/CSS would come back empty and the response would be a 200 with a
	// bare <div id="root"> and nothing to hydrate it — a blank page with NOTHING
	// in the logs, the exact silent-failure shape the no-silent-errors rule
	// forbids. Every registered Page.App is SUPPOSED to resolve here (internal/cmd/webd's
	// TestRegisteredPageAppsResolveInBuiltManifest asserts it over the real
	// blank-import set, which an in-package test cannot: a plugin's own
	// deps.(Deps) cast makes ui.Routes(nil) return no routes at all), but the
	// entry can still be absent for real — a fresh `oap install` predating a
	// `mage web:build` for a newly-added page. Log loudly and serve the styled
	// system page instead.
	entry, ok := s.manifest[rt.Page.App]
	if !ok {
		log.FromContext(r.Context()).Info("webui: page app absent from web asset manifest; serving error page",
			"app", rt.Page.App, "pattern", r.URL.Path)
		s.renderSystem(w, &PageError{Status: http.StatusInternalServerError, Kind: "error",
			Title: "Page unavailable", Message: "This page isn't available right now."})
		return
	}
	// renderDocument only errors when props fail to JSON-marshal, which happens
	// BEFORE any byte is written — so falling back to a system page is safe.
	if derr := renderDocument(w, docInput{
		App:                  rt.Page.App,
		Title:                meta.Title,
		Props:                props,
		Scripts:              entry.Scripts,
		CSS:                  entry.CSS,
		Nonce:                nonce,
		CSP:                  buildCSP(nonce, rt.Page.FramesSandbox, s.sandboxHost(), s.dev, rt.Page.EmbeddableSameOrigin, rt.Page.FramesSameOrigin),
		Dev:                  s.dev,
		EmbeddableSameOrigin: rt.Page.EmbeddableSameOrigin,
	}); derr != nil {
		s.renderSystem(w, &PageError{Status: http.StatusInternalServerError, Kind: "error",
			Title: "Internal error", Message: "The page could not be rendered."})
	}
}

// renderSystem renders a styled system page with a fresh nonce.
func (s *Server) renderSystem(w http.ResponseWriter, pe *PageError) {
	nonce, err := newNonce()
	if err != nil {
		http.Error(w, pe.Title, pe.Status) // last-resort; nonce gen should never fail
		return
	}
	renderSystemPage(w, s.manifest, nonce, s.dev, pe)
}

func (s *Server) authenticate(r *http.Request) (string, bool) {
	if s.authFn == nil {
		return "", false
	}
	return s.authFn(r)
}

// ServeHTTP dispatches by Host header to the matching origin mux.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Host-agnostic liveness: answer /healthz regardless of Host so k8s
	// probes (which hit the pod IP, not the configured trusted host) pass.
	if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
		return
	}

	// Make the framework renderer available to every handler (so raw Handlers
	// with custom control flow can render React pages / system errors).
	r = r.WithContext(withRenderer(r.Context(), s))

	host := r.Host
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	// Resolved per request so dispatch follows a live host change with no
	// rebuild; the getters may return full URLs, hostOnly normalizes.
	th := hostOnly(s.trustedHost())
	sh := hostOnly(s.sandboxHost())
	if th == "" && sh == "" {
		// Neither origin configured yet (providers haven't reported URLs).
		// Idle-but-ready: 404 everything rather than route against an empty host.
		s.renderSystem(w, &PageError{Status: http.StatusNotFound, Kind: "notFound",
			Title: "Not found", Message: "No page is configured at this address."})
		return
	}
	// Partial config (one getter empty) falls through to the switch below, where
	// the th != ""/sh != "" guards keep the unconfigured side unreachable.
	//
	// Shared-origin debug (INSECURE — see the sharedOriginOK field): trusted and
	// sandbox collapse to ONE host serving BOTH route sets. Gated PER REQUEST
	// against the LIVE host, so it engages the moment a PublicEndpoint publishes
	// the tunnel's URL — no restart — and can never engage in production, where
	// the two origins are distinct (and non-ngrok) hosts.
	//
	// A LOOPBACK REQUEST COUNTS AS THAT SHARED HOST even once the two getters
	// name somewhere else. On a kind whose profile allows a shared origin, webd
	// serves both route sets from one address the operator reaches over a
	// port-forward — and a PublicEndpoint moves the ADVERTISED address to the
	// tunnel while leaving that local one serving. Requiring host == th there
	// meant the desktop's own /admin, /sessions and chat 404'd from the moment
	// the tunnel came up: the feature succeeding is what broke the UI.
	//
	// This can only widen a kind that ALREADY collapses the two origins: the
	// predicate is nil unless webd was started with --allow-shared-origin, which
	// `oap install` injects only for a profile whose AllowsSharedOrigin is true,
	// and it answers false for a loopback host on every other profile. The
	// distinct-origin case below is untouched, and sandbox content still reaches
	// the trusted host on exactly the kinds where it already did.
	if th != "" && th == sh && s.sharedOriginOK != nil &&
		(host == th || IsLoopbackHost(host)) && s.sharedOriginOK(host) {
		if _, pattern := s.trustedMux.Handler(r); pattern != "" {
			s.trustedMux.ServeHTTP(w, r)
			return
		}
		s.sandboxMux.ServeHTTP(w, r)
		return
	}
	// Distinct origins (the normal, secure case): each host serves only its own
	// route set. Equal hosts the predicate rejects land here too and fail closed
	// — the trusted case matches first, so sandbox content is never served on the
	// trusted host; there is no implicit merge.
	switch {
	case th != "" && host == th:
		s.trustedMux.ServeHTTP(w, r)
	case sh != "" && host == sh:
		s.sandboxMux.ServeHTTP(w, r)
	default:
		s.renderSystem(w, &PageError{Status: http.StatusNotFound, Kind: "notFound",
			Title: "Not found", Message: "No page is configured at this address."})
		return
	}
}

// IsLoopbackHost reports whether host — a bare host with no port, as hostOnly
// yields — names the machine the request arrived on.
//
// Exported because two things ask the same question and must not disagree:
// ServeHTTP's shared-origin branch, which serves a loopback request on a kind
// that collapses the two origins, and webd's own sharedOriginPredicate, which
// decides whether such a kind allows that at all. A second spelling of
// "loopback" is how one of them ends up serving what the other refuses.
//
// net.ParseIP rather than a prefix test: "127.0.0.1" and "::1" are not the only
// loopback addresses (127.9.9.9 is one), and a host that merely starts with one
// of them is not loopback at all. "localhost" is the one name here — RFC 6761
// reserves it for the resolving machine, whatever it resolves to.
func IsLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// hostOnly reduces a full URL OR a bare host to a bare hostname (no scheme, no
// port), so the webd host getters can stay trivial and just return URLs:
//   - a URL with a scheme ⇒ its Hostname() (drops scheme + port);
//   - a bare host or host:port ⇒ itself with any :port stripped;
//   - empty in ⇒ empty out.
//
// Raw bracketed IPv6 literals are NOT supported inputs — the host getters never
// produce them.
func hostOnly(s string) string {
	if s == "" {
		return ""
	}
	if u, err := url.Parse(s); err == nil {
		if h := u.Hostname(); h != "" {
			return h
		}
	}
	// url.Parse("trusted.example") or "trusted.example:8080" puts the value in
	// Path (no scheme ⇒ no Host). Treat a slash-free input as a bare host and
	// strip any :port.
	if !strings.ContainsRune(s, '/') {
		if i := strings.IndexByte(s, ':'); i >= 0 {
			return s[:i]
		}
		return s
	}
	return ""
}

// newHTTPServer builds the hardened http.Server that Run serves. Its
// BaseContext returns ctx so every request context carries what ctx carries —
// notably the logger webd injects via log.IntoContext. Without it,
// log.FromContext(r.Context()) falls back to controller-runtime's global
// delegating logger, which a transitive spicedb init() poisons with a Nop sink
// before main() runs (SetLogger is first-write-wins), and every
// webui/identityd handler log silently vanishes.
func newHTTPServer(ctx context.Context, addr string, h http.Handler) *http.Server {
	srv := &http.Server{
		Addr:        addr,
		Handler:     h,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	safehttp.HardenServer(srv)
	return srv
}

// Run serves until ctx is cancelled (mirrors identityd's lifecycle).
func (s *Server) Run(ctx context.Context, addr string) error {
	srv := newHTTPServer(ctx, addr, s)
	errCh := make(chan error, 1)
	go func() {
		err := srv.ListenAndServe()
		if err != nil && err != http.ErrServerClosed {
			errCh <- err
			return
		}
		errCh <- nil
	}()
	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		return <-errCh
	case err := <-errCh:
		return err
	}
}

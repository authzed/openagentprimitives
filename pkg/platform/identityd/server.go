package identityd

import (
	"context"
	"net/http"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
	"github.com/authzed/openagentprimitives/pkg/x/safehttp"
)

// Deps are the runtime dependencies the server needs. Constructed by main at
// startup; NewServer panics when K8s, LinkSigner or ExternalBaseURL is nil,
// since none of the three is optional.
type Deps struct {
	// K8s is the controller-runtime client identityd uses to look up
	// AgentSessions (for the gate) + write SessionUserIdentity status +
	// create/update UserIdentity + master Secrets.
	K8s client.Client

	// LinkSigner mints (for state-store entries) and verifies (for /link
	// + cookies) HMAC-signed payloads. Same Signer the channelsd watcher
	// uses to mint links — they share the underlying Secret key.
	LinkSigner *passthroughlink.Signer

	// ExternalBaseURL returns identityd's externally reachable URL, e.g.
	// "https://identityd.example.com". A getter (not a string) so a
	// tunnel URL can change without restarting the pod.
	ExternalBaseURL func() string

	// Authenticators maps channel-kind name → its WebAuthenticator. A kind
	// missing here (its WebAuthenticator returned nil) makes
	// /oidc/callback/<kind> a 404.
	Authenticators map[string]channelkinds.WebAuthenticator

	// IconHandler serves GET /icon/<credName>. Optional: nil leaves the
	// route unregistered, so /icon/ returns 404.
	IconHandler http.Handler

	// InsecureTrustLinks, when true, accepts a signed link as proof of
	// identity on authenticated paths with no configured authenticator.
	// Dev-only; warned loudly at startup.
	InsecureTrustLinks bool

	// AgentIdentityAuthz answers the ONE permission question identityd asks
	// about an agent's OWN shared credential: may this signed-in human replace
	// it? Consulted at CLICK time on a credential_update link resolving to an
	// AgentIdentity — the monitoring-channel variant of that link is
	// deliberately not subject-bound (a monitoring channel has no single
	// addressee), so this check is the only control between the URL and a
	// credential every session of the agent authenticates with.
	//
	// Nil is FAIL-CLOSED: every agent-owned credential update is refused,
	// loudly logged, and the visitor told the platform could not check.
	// Declared as the interface type so the zero value is a genuine nil
	// interface — see AGENTS.md's "Nil interfaces" note.
	//
	// UX gate only; the AUTHORITATIVE check of the same permission runs in the
	// operator, on the subject identityd asserts.
	AgentIdentityAuthz AgentIdentityAuthz

	// AgentCredentialWriter submits an agent-owned credential replacement to
	// the OPERATOR, which authorizes it independently and performs the write.
	// identityd never writes that Secret itself: its own Secret access is
	// confined to IdentitiesNamespace, and the cluster-wide grant this would
	// need is standing RBAC on the browser-facing pod, outliving every request
	// that justified it.
	//
	// Nil is FAIL-CLOSED — refused with an explicit "no operator client wired"
	// message rather than anything reading as a permission problem, because the
	// fix is wiring, not a grant.
	AgentCredentialWriter AgentCredentialWriter
}

// Server is the identityd HTTP server. One per process.
type Server struct {
	deps           Deps
	mux            *http.ServeMux
	stateStore     *linkStateStore
	consumedLinks  *consumedLinkStore
	suggestedCache *suggestedCache
	oauthState     *oauthStateStore
	idp            *idpLoader
	cliCodes       *cliCodeStore
}

// NewServer builds a Server with the given deps + registers routes.
// Panics if a required dep is missing — those are programmer errors,
// not runtime conditions.
func NewServer(d Deps) *Server {
	if d.K8s == nil {
		panic("identityd: NewServer: K8s is required")
	}
	if d.LinkSigner == nil {
		panic("identityd: NewServer: LinkSigner is required")
	}
	if d.ExternalBaseURL == nil {
		panic("identityd: NewServer: ExternalBaseURL getter is required")
	}
	s := &Server{
		deps:       d,
		mux:        http.NewServeMux(),
		stateStore: newLinkStateStore(),
		// A bit past the 30m link expiry, and Secret-backed so single-use
		// survives an identityd restart (see consumedLinkStore).
		consumedLinks:  newConsumedLinkStore(35*time.Minute, newSecretConsumedLinkBackend(d.K8s)),
		suggestedCache: newSuggestedCache(suggestedCacheTTL),
		oauthState:     newOAuthStateStore(10 * time.Minute),
		idp: &idpLoader{
			k8s:         d.K8s,
			externalURL: d.ExternalBaseURL,
			cacheTTL:    30 * time.Second,
			now:         time.Now,
		},
		cliCodes: newCLICodeStore(),
	}
	s.registerRoutes()
	return s
}

// Handler exposes the server's mux for tests + for embedding into other
// listeners (e.g. an httptest.Server in the E2E).
func (s *Server) Handler() http.Handler { return s.mux }

// routes is identityd's SINGLE route registration, consumed by BOTH the
// standalone mux (registerRoutes → Handler(), used by tests and the e2e /oidc
// sub-mount) and webd (ui.Routes() → the webui framework). Adding a route here
// mounts it on every surface, so the two can never drift.
//
// Origin is always trusted (these routes read/set the idd_session cookie). Auth
// is enforced by webd's webui framework as defense-in-depth; the standalone mux
// enforces only the method set, because every handler self-verifies the
// idd_session cookie via checkOIDCCookie. /healthz is deliberately absent —
// webd's health WebUI owns it, and registerRoutes adds it standalone-only.
func (s *Server) routes() []webui.Route {
	r := func(pattern string, methods []string, auth webui.AuthLevel, h http.HandlerFunc) webui.Route {
		return webui.Route{Origin: webui.OriginTrusted, Pattern: pattern, Methods: methods, Auth: auth, Handler: h}
	}
	get := []string{http.MethodGet}
	post := []string{http.MethodPost}
	routes := []webui.Route{
		// AuthNone (GET, no cookie): the link form, the generic OIDC begin,
		// the OIDC + OAuth callbacks, the portal landing, and the CLI login
		// begin. Each handler self-gates (signed link / state token / cookie).
		r("/link", get, webui.AuthNone, s.handleLinkGet),
		// /link/agent-oauth/ is AuthNone (not AuthAuthenticated, unlike
		// /link/oauth/ below) because — like /link itself — its own no-cookie
		// case redirects into /oidc/login carrying the signed link forward,
		// rather than the framework's bare 401.
		r("/link/agent-oauth/", get, webui.AuthNone, s.handleLinkAgentOAuthGet),
		r("/oidc/login", get, webui.AuthNone, s.handleOIDCLogin),
		r("/oidc/callback/", get, webui.AuthNone, s.handleOIDCCallback),
		r("/oauth/callback/", get, webui.AuthNone, s.handleOAuthCallbackGet),
		// /oauth/agent-callback/ is the AGENT (workshop-credential) flow's OWN
		// callback — see handlers_agentoauth.go's package doc for why it must
		// never share a redirect_uri with the per-user one above.
		r("/oauth/agent-callback/", get, webui.AuthNone, s.handleOAuthAgentCallbackGet),
		r("/my/accounts", get, webui.AuthNone, s.handlePortalGet),
		r("/cli/login", get, webui.AuthNone, s.handleCLILogin),
		// /cli/exchange is a no-cookie POST: the CLI authenticates with the
		// single-use code in the request body (no ambient cookie ⇒ no CSRF
		// vector), so the framework performs no auth here.
		r("/cli/exchange", post, webui.AuthHandlerManaged, s.handleCLIExchange),
		// /password/login and /password/verify back the "password" idp.Kind
		// (pkg/platform/identity/idp/passwordkind), a non-redirect local IdP.
		// Handler-managed: the GET form-render self-gates on a not-yet-consumed
		// state token, and the POST verify is /cli/exchange's shape (a
		// single-use token stands in for a cookie ⇒ no ambient-cookie CSRF).
		r("/password/login", get, webui.AuthNone, s.handlePasswordLogin),
		r("/password/verify", post, webui.AuthHandlerManaged, s.handlePasswordVerify),
		// AuthAuthenticated (cookie-gated): link submit, the portal
		// subrouter (form/submit/revoke), the heartbeat, and the per-cred
		// OAuth begin.
		r("/link/submit", post, webui.AuthAuthenticated, s.handleLinkSubmit),
		r("/my/accounts/", []string{http.MethodGet, http.MethodPost}, webui.AuthAuthenticated, s.handlePortalSubrouter),
		r("/heartbeat", post, webui.AuthAuthenticated, s.handleHeartbeat),
		r("/link/oauth/", get, webui.AuthAuthenticated, s.handleLinkOAuthGet),
	}
	// /icon/<credName> — public favicons; only when an IconHandler is wired.
	if s.deps.IconHandler != nil {
		routes = append(routes, webui.Route{
			Origin: webui.OriginTrusted, Pattern: "/icon/", Methods: get,
			Auth: webui.AuthNone, Handler: s.deps.IconHandler,
		})
	}
	return routes
}

// registerRoutes mounts routes() onto the standalone mux (Handler()), plus
// /healthz. It enforces only each route's method set (with the same styled
// 405 page) — auth is the handlers' own responsibility here; see routes().
func (s *Server) registerRoutes() {
	s.mux.HandleFunc("/healthz", s.handleHealthz)
	for _, rt := range s.routes() {
		s.mux.Handle(rt.Pattern, s.standaloneRoute(rt))
	}
}

// standaloneRoute adapts a webui.Route to the standalone mux: it rejects any
// method outside the route's declared set with a readable 405 page, then
// dispatches to the handler. webd applies the same method gate plus the
// route's Auth level through the webui framework.
func (s *Server) standaloneRoute(rt webui.Route) http.Handler {
	methods := rt.Methods
	h := rt.Handler
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(methods) > 0 {
			allowed := false
			for _, m := range methods {
				if r.Method == m {
					allowed = true
					break
				}
			}
			if !allowed {
				w.Header().Set("Allow", strings.Join(methods, ", "))
				s.writeError(w, r, http.StatusMethodNotAllowed, "Method not allowed",
					"This endpoint only accepts "+strings.Join(methods, ", ")+" requests.")
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}

// Run starts the HTTP server on addr and blocks until ctx is cancelled.
// Returns the listener / shutdown error (nil on graceful shutdown).
func (s *Server) Run(ctx context.Context, addr string) error {
	srv := &http.Server{Addr: addr, Handler: s.mux}
	safehttp.HardenServer(srv)
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
		_ = srv.Shutdown(shutCtx) // best effort; the goroutine above reports the listener err
		return <-errCh
	case err := <-errCh:
		return err
	}
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

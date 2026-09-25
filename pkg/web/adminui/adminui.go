package adminui

import (
	"context"
	"net/http"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
	"github.com/authzed/openagentprimitives/pkg/web/webui/registry"
)

// currentUser is the header chip identity the React shell renders top-right.
type currentUser struct {
	// Email is the display form decoded from the auth subject, falling back to
	// the raw subject when it cannot be decoded.
	Email string `json:"email"`
	// Role is fixed copy, not a lookup: reaching this page proves the
	// view_overview platform check passed.
	Role string `json:"role"`
}

type props struct {
	// APIBase is the same-origin prefix the SPA issues its API calls against.
	APIBase string `json:"apiBase"`
	// CurrentUser is the logged-in platform admin (decoded from the auth
	// subject). Nil when the subject is absent — the client falls back to its
	// default chip.
	CurrentUser *currentUser `json:"currentUser,omitempty"`
}

type ui struct{}

// New returns the admin WebUI plugin.
func New() webui.WebUI { return ui{} }

func (ui) Name() string { return "admin" }

func (ui) Routes(deps webui.Deps) []webui.Route {
	d, ok := deps.(Deps)
	if !ok || d.AdmindBaseURL() == "" || d.AdmindToken() == "" {
		return nil // fail-closed: admin UI not configured
	}

	authorize := func(permission string) func(ctx context.Context, subject string, r *http.Request) error {
		return func(ctx context.Context, subject string, r *http.Request) error {
			// The webd cookie subject. Verified by webd's own authenticate
			// before this handler runs; the header it forwards to admind is a
			// separate, unsigned hop.
			canonical := identity.CanonicalFromTrusted(strings.TrimPrefix(subject, "user:"),
				"webd cookie subject, verified by webd's authenticate")
			ok, err := d.CheckPlatformPermission(ctx, permission, canonical, false)
			if err != nil {
				// A SpiceDB ERROR must never read as a denial — surface 500.
				d.Logger().Info("adminui: platform permission check errored",
					"permission", permission, "subject", subject, "err", err.Error())
				return &webui.PageError{Status: http.StatusInternalServerError, Kind: "error",
					Title:   "Authorization unavailable",
					Message: "The permission check failed. Try again; if it persists, check the operator's SpiceDB connection."}
			}
			if !ok {
				return &webui.PageError{Status: http.StatusForbidden, Kind: "forbidden",
					Title: "Access denied", Message: "You do not have platform admin access."}
			}
			return nil
		}
	}
	// /admin/api/sessions/{ns}/{name}: GET = view, DELETE = kill.
	sessionsAuthorize := func(ctx context.Context, subject string, r *http.Request) error {
		permission := "view_sessions"
		if r.Method == http.MethodDelete {
			permission = "kill_session"
		}
		return authorize(permission)(ctx, subject, r)
	}

	// /admin/api/workshops/{ns}/{name}[/install|/decline]: POST = install/decline
	// (install_agent), DELETE = kill (kill_session) — matching admind's own gates
	// on the same paths. TestAProxiedRouteAsksForTheSamePermissionAdmindDoes only
	// checks EXACT proxy patterns, so this subtree's split lives in the guard
	// TestRoutes_ShapeAndAuthorize exercises, exactly like sessionsAuthorize.
	workshopsAuthorize := func(ctx context.Context, subject string, r *http.Request) error {
		permission := "install_agent"
		if r.Method == http.MethodDelete {
			permission = "kill_session"
		}
		return authorize(permission)(ctx, subject, r)
	}

	// buildPage renders the admin SPA document. Shared by the exact /admin
	// landing route and the /admin/{path...} deep-link catch-all so a browser
	// refresh or a shared deep link (e.g. /admin/agent/default/x) loads the
	// same SPA — the client router then parses window.location.
	buildPage := func(ctx context.Context, _ *http.Request) (any, webui.PageMeta, error) {
		p := props{APIBase: "/admin/api"}
		// Best-effort: decode the authenticated subject (user:<canonical>)
		// back to its email for the header chip. DecodeForDisplay falls
		// back to the raw subject on any decode failure and never mangles.
		// Role is fixed — reaching Build means the view_overview platform
		// check passed. An absent subject leaves CurrentUser nil so the
		// client renders its default chip.
		if user := identity.DecodeForDisplay(webui.SubjectFromContext(ctx)); user != "" {
			p.CurrentUser = &currentUser{Email: user, Role: "platform admin"}
		}
		return p, webui.PageMeta{Title: "Open Agent Primitives · Admin"}, nil
	}
	adminPage := func(pattern string) webui.Route {
		return webui.Route{Origin: webui.OriginTrusted, Pattern: pattern, Methods: []string{http.MethodGet},
			Auth: webui.AuthLoginIfNecessary, Authorize: authorize("view_overview"),
			Page: &webui.Page{App: "admin", Build: buildPage}}
	}

	// Every proxied route goes through the Origin pin: a no-op for the GET
	// reads, and the gate for every state-changing one -- so a mutating route
	// added later is covered without anyone remembering to wrap it.
	px := pinOrigin(d, proxyHandler(d))
	return []webui.Route{
		adminPage("/admin"),
		// Catch-all so client-routed deep links load the SPA. The /admin/api/*
		// proxy routes below are more specific and win under Go 1.22 ServeMux
		// precedence; only non-api deep links fall through to this page.
		adminPage("/admin/{path...}"),
		// /admin/logout expires the auth cookie and returns to /admin. AuthNone
		// by design — logging out must succeed even without a valid session —
		// and more specific than the /admin/{path...} catch-all, so it wins.
		{Origin: webui.OriginTrusted, Pattern: "/admin/logout", Methods: []string{http.MethodGet},
			Auth: webui.AuthNone, Handler: http.HandlerFunc(logoutHandler)},
		{Origin: webui.OriginTrusted, Pattern: "/admin/api/overview", Methods: []string{http.MethodGet},
			Auth: webui.AuthAuthorized, Authorize: authorize("view_overview"), Handler: px},
		{Origin: webui.OriginTrusted, Pattern: "/admin/api/health", Methods: []string{http.MethodGet},
			Auth: webui.AuthAuthorized, Authorize: authorize("view_overview"), Handler: px},
		{Origin: webui.OriginTrusted, Pattern: "/admin/api/cluster", Methods: []string{http.MethodGet},
			Auth: webui.AuthAuthorized, Authorize: authorize("view_overview"), Handler: px},
		{Origin: webui.OriginTrusted, Pattern: "/admin/api/budget", Methods: []string{http.MethodGet},
			Auth: webui.AuthAuthorized, Authorize: authorize("view_overview"), Handler: px},
		{Origin: webui.OriginTrusted, Pattern: "/admin/api/toolcalls", Methods: []string{http.MethodGet},
			Auth: webui.AuthAuthorized, Authorize: authorize("view_live"), Handler: px},
		{Origin: webui.OriginTrusted, Pattern: "/admin/api/approvals", Methods: []string{http.MethodGet},
			Auth: webui.AuthAuthorized, Authorize: authorize("view_live"), Handler: px},
		{Origin: webui.OriginTrusted, Pattern: "/admin/api/sessions", Methods: []string{http.MethodGet},
			Auth: webui.AuthAuthorized, Authorize: authorize("view_sessions"), Handler: px},
		{Origin: webui.OriginTrusted, Pattern: "/admin/api/sessions/", Methods: []string{http.MethodGet, http.MethodDelete},
			Auth: webui.AuthAuthorized, Authorize: sessionsAuthorize, Handler: px},
		{Origin: webui.OriginTrusted, Pattern: "/admin/api/audit/", Methods: []string{http.MethodGet, http.MethodPost},
			Auth: webui.AuthAuthorized, Authorize: authorize("view_audit"), Handler: px},
		{Origin: webui.OriginTrusted, Pattern: "/admin/api/artifacts", Methods: []string{http.MethodGet},
			Auth: webui.AuthAuthorized, Authorize: authorize("view_audit"), Handler: px},
		// Subtree for /admin/api/artifacts/{ns}/{name} — the artifact-detail
		// endpoint. The exact route above lists; this one serves detail. More
		// specific than the /admin/{path...} SPA catch-all, so it proxies.
		{Origin: webui.OriginTrusted, Pattern: "/admin/api/artifacts/", Methods: []string{http.MethodGet},
			Auth: webui.AuthAuthorized, Authorize: authorize("view_audit"), Handler: px},
		{Origin: webui.OriginTrusted, Pattern: "/admin/api/memory", Methods: []string{http.MethodGet},
			Auth: webui.AuthAuthorized, Authorize: authorize("view_audit"), Handler: px},
		{Origin: webui.OriginTrusted, Pattern: "/admin/api/kg/", Methods: []string{http.MethodGet},
			Auth: webui.AuthAuthorized, Authorize: authorize("view_audit"), Handler: px},
		{Origin: webui.OriginTrusted, Pattern: "/admin/api/access", Methods: []string{http.MethodGet},
			Auth: webui.AuthAuthorized, Authorize: authorize("view_config"), Handler: px},
		// The workshops page: GET lists (view_sessions); the subtree fronts
		// POST .../install + .../decline (install_agent) and DELETE .../{ns}/{name}
		// (kill_session), resolved by workshopsAuthorize above. Both are derived
		// from admind.Routes() by TestEveryBrowserFacingAdmindRouteIsProxied.
		{Origin: webui.OriginTrusted, Pattern: "/admin/api/workshops", Methods: []string{http.MethodGet},
			Auth: webui.AuthAuthorized, Authorize: authorize("view_sessions"), Handler: px},
		{Origin: webui.OriginTrusted, Pattern: "/admin/api/workshops/", Methods: []string{http.MethodPost, http.MethodDelete},
			Auth: webui.AuthAuthorized, Authorize: workshopsAuthorize, Handler: px},
		// Subtree for /admin/api/config/*: the generic resource LIST
		// (/config/{resource}) AND the per-resource DETAIL
		// (/config/{resource}/{ns}/{name} or /config/{resource}/{name}). The
		// proxy rewrite is a pure prefix swap, so a multi-segment detail path
		// forwards to /admin/v1/config/... unchanged. More specific than the
		// /admin/{path...} SPA catch-all, so it proxies.
		{Origin: webui.OriginTrusted, Pattern: "/admin/api/config/", Methods: []string{http.MethodGet},
			Auth: webui.AuthAuthorized, Authorize: authorize("view_config"), Handler: px},
		// The install-and-wire routes. Every one of these was registered in
		// admind and reachable from nowhere: the console's own TypeScript has
		// POSTed /admin/api/agents/oap-install since the install dialog shipped
		// (ui/lib/api.ts, postOapInstall), and with no entry here that POST
		// matched only the GET-only /admin/{path...} SPA catch-all — so the
		// install button had never worked.
		// TestEveryBrowserFacingAdmindRouteIsProxied now derives this coverage
		// from admind.Routes() so the next one cannot be added silently.
		{Origin: webui.OriginTrusted, Pattern: "/admin/api/agents/oap-install", Methods: []string{http.MethodPost},
			Auth: webui.AuthAuthorized, Authorize: authorize("install_agent"), Handler: px},
		{Origin: webui.OriginTrusted, Pattern: "/admin/api/agents/channel-setup", Methods: []string{http.MethodPost},
			Auth: webui.AuthAuthorized, Authorize: authorize("install_agent"), Handler: px},
		{Origin: webui.OriginTrusted, Pattern: "/admin/api/agents/channel-handoff", Methods: []string{http.MethodPost},
			Auth: webui.AuthAuthorized, Authorize: authorize("install_agent"), Handler: px},
		// The handoff CALLBACK, which an external service redirects a browser
		// into.
		//
		// AuthLoginIfNecessary, and it costs NO authorization: the framework
		// runs Authorize whenever it is set, on this level as much as on
		// AuthAuthorized (webui/server.go's AuthLoginIfNecessary arm), and the
		// GET/HEAD restriction it carries is satisfied — a redirect is a GET.
		// So this route is gated on install_agent exactly like its siblings.
		//
		// What differs is the LAPSED-SESSION path, and it lands on the worst
		// case there is. By the time the service redirects, the App already
		// exists; AuthAuthorized would answer a 401 and leave it orphaned —
		// precisely the case admind's begin log line exists to make
		// diagnosable. This redirects to login and back with the query intact,
		// and the exchange completes.
		{Origin: webui.OriginTrusted, Pattern: "/admin/api/agents/channel-handoff/callback", Methods: []string{http.MethodGet},
			Auth: webui.AuthLoginIfNecessary, Authorize: authorize("install_agent"), Handler: px},
	}
}

// adminSessionCookie is the identityd-minted auth cookie (pkg/platform/identityd
// cookieName). /admin/logout expires it so "Sign Out" ends the session
// server-side — an HttpOnly cookie cannot be cleared from JS.
const adminSessionCookie = "idd_session"

// logoutHandler clears the idd_session cookie and redirects to /admin. The
// expiry Set-Cookie mirrors the mint's Name/Path/HttpOnly/SameSite so the
// browser overwrites the same cookie; Secure tracks the request scheme so the
// delete cookie is accepted on both the https prod origin and http local dev.
//
// It serves only a top-level NAVIGATION. This is a GET with a side effect, and
// markdown image syntax fires a same-origin GET — so an image reference to this
// path in any message a browser surface renders (a transcript, an approval
// card's excerpt, an agent's own reply) logged out every viewer who rendered it,
// with nothing clicked.
//
// Sec-Fetch-Dest is the right discriminator and the framework's Origin pin is
// not: a top-level navigation sends no Origin header, so pinning on it would
// refuse the real Sign Out button. An <img> sends "image", a fetch sends
// "empty", an iframe "iframe" — only a navigation sends "document", which is
// exactly what the button (window.location.assign) produces.
//
// A request with NO Sec-Fetch-Dest fails closed. Such a browser predates the
// header and the threat model; the cost is that it cannot sign out from the
// button, and the other direction is the hole this closes.
//
// Of the ten unauthenticated admin routes this is the only one with a side
// effect, which is why this is a one-route change rather than a class
// migration.
func logoutHandler(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Sec-Fetch-Dest") != "document" {
		http.Error(w, "sign out must be a navigation", http.StatusForbidden)
		return
	}
	secure := r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	http.SetCookie(w, &http.Cookie{
		Name:     adminSessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/admin", http.StatusFound)
}

func init() { registry.Register(ui{}) }

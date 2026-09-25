// Package webui is the framework for webd's browser UIs: a registry of
// scoped, two-origin WebUI plug-ins served by one host binary. Each UI
// declares HTTP routes, each scoped to an origin (trusted vs sandbox) and
// an auth level. The framework enforces that public routes are GET/HEAD
// only, dispatches requests to the right origin by Host header, and wraps
// handlers with the auth middleware.
package webui

import (
	"context"
	"net/http"
	"strings"
)

// Origin is which of webd's two hostnames a route serves on. The trusted
// origin holds the auth cookie; the sandbox origin serves semi-trusted
// artifact content and never receives the cookie.
type Origin int

const (
	OriginTrusted Origin = iota
	OriginSandbox
)

func (o Origin) String() string {
	switch o {
	case OriginTrusted:
		return "trusted"
	case OriginSandbox:
		return "sandbox"
	default:
		return "unknown"
	}
}

// AuthLevel gates a route. None = public (GET/HEAD only); Authenticated =
// a valid OIDC cookie is required; Authorized = authenticated AND the
// route's Authorize check passes.
type AuthLevel int

const (
	AuthNone AuthLevel = iota
	AuthAuthenticated
	AuthAuthorized
	AuthLoginIfNecessary
	// AuthHandlerManaged: the framework performs NO authentication and does NOT
	// restrict the HTTP method — the handler authenticates by its own means (e.g.
	// a single-use code in the request body, not the session cookie). Unlike
	// AuthNone it permits POST, so use it only where the credential is carried in
	// the request itself and there is no ambient-cookie CSRF vector.
	AuthHandlerManaged
)

func (a AuthLevel) String() string {
	switch a {
	case AuthNone:
		return "none"
	case AuthAuthenticated:
		return "authenticated"
	case AuthAuthorized:
		return "authorized"
	case AuthLoginIfNecessary:
		return "login-if-necessary"
	case AuthHandlerManaged:
		return "handler-managed"
	default:
		return "unknown"
	}
}

// Route is one HTTP route a WebUI serves.
type Route struct {
	Origin  Origin
	Pattern string
	Methods []string // allowed HTTP methods; empty ⇒ {http.MethodGet}
	Handler http.Handler
	// Page is the declarative React-page alternative to Handler. Exactly one of
	// Handler or Page must be set. When set, the framework renders the shared
	// document and mounts the named React app with Page.Build's props.
	Page *Page
	Auth AuthLevel
	// Authorize is required when Auth==AuthAuthorized: given the
	// authenticated canonical subject + request, returns nil to allow.
	Authorize func(ctx context.Context, subject string, r *http.Request) error
}

// WebUI is one registerable browser UI. Registered at init() via
// pkg/web/webui/registry and blank-imported by internal/cmd/webd.
type WebUI interface {
	Name() string
	Routes(deps Deps) []Route
}

// Deps is the opaque per-component dependency value the binary hands to the
// server. The framework never inspects it; each WebUI casts it to its own
// interface in Routes()/handlers (e.g. `d, ok := deps.(mypkg.Deps)`).
// Framework-level auth funcs are NOT here — they go to NewServer directly.
// A WebUI whose cast fails (nil or unexpected concrete type) should return no
// routes, so an unconfigured component fails closed rather than serving.
type Deps interface{}

type subjectKeyT struct{}

var subjectKey subjectKeyT

// SubjectFromContext returns the authenticated canonical subject the auth
// middleware injected (empty if the route was AuthNone or unauthenticated).
func SubjectFromContext(ctx context.Context) string {
	s, _ := ctx.Value(subjectKey).(string)
	return s
}

// withSubject returns ctx carrying subject (used by the server middleware).
func withSubject(ctx context.Context, subject string) context.Context {
	return context.WithValue(ctx, subjectKey, subject)
}

// WithSubjectForTest injects an authenticated subject into ctx. It exists so
// tests of handlers that read SubjectFromContext OUTSIDE the server middleware
// (e.g. the adminui reverse proxy) can simulate an authenticated request.
// Production code never calls this — the middleware uses the unexported
// withSubject.
func WithSubjectForTest(ctx context.Context, subject string) context.Context {
	return withSubject(ctx, subject)
}

// TrustedOriginMatch is the CSRF Origin pin every BROWSER-ONLY route in this
// codebase shares: the request's Origin header must be present and must equal
// the trusted origin (trailing slash trimmed, since a browser sends none).
//
// A blank trustedOrigin REFUSES EVERYTHING, which is the whole reason this lives
// in one place. Written inline as `r.Header.Get("Origin") != trusted`, an unset
// trusted URL makes `"" != ""` false and the pin silently passes for every
// request that sends no Origin at all — a CSRF guard turned into a no-op exactly
// when the deployment is misconfigured. A route that cannot know its own origin
// cannot decide that a caller came from it.
//
// Every browser-only mutating route uses this one helper, so none of them can
// drift into a laxer local rule.
func TrustedOriginMatch(r *http.Request, trustedOrigin string) bool {
	trusted := strings.TrimRight(trustedOrigin, "/")
	if trusted == "" {
		return false // fail closed: an unknown trusted origin can vouch for nothing
	}
	return r.Header.Get("Origin") == trusted
}

package webui

import (
	"context"
	"net/http"
)

// Page is the declarative React-page variant of a Route. The framework owns the
// HTML document, CSP, nonce, and manifest lookup; the plugin supplies only a
// server-side props builder that runs AFTER the auth middleware.
type Page struct {
	// App is the appKey; it must exist in the embedded web manifest (internal/cmd/webd's
	// TestRegisteredPageAppsResolveInBuiltManifest asserts every registered one
	// does).
	App string
	// FramesSandbox, when true, adds `frame-src <live sandbox URL>` to this
	// page's CSP (the artifact-view shell frames sandbox content).
	FramesSandbox bool
	// EmbeddableSameOrigin, when true, relaxes this page's anti-framing headers
	// to permit SAME-ORIGIN framing only: frame-ancestors 'self' (instead of
	// 'none') and X-Frame-Options: SAMEORIGIN (instead of DENY). Cross-origin
	// framing stays blocked. Set only on pages meant to be embedded by another
	// first-party page (the session-view page, embedded by ap:session_view).
	EmbeddableSameOrigin bool
	// FramesSameOrigin, when true, adds `frame-src 'self'` so this page may embed
	// a same-origin iframe. Set on the session shell, whose ap:session_view
	// component embeds the same-origin /session-view/{ns}/{name} page.
	FramesSameOrigin bool
	// Build runs inside the auth middleware (SubjectFromContext is available).
	// It returns JSON-serializable props + page meta. A returned *PageError maps
	// to its HTTP status and renders the matching styled system page; any other
	// error renders a generic 500 system page.
	Build func(ctx context.Context, r *http.Request) (props any, meta PageMeta, err error)
}

// PageMeta carries per-page document metadata.
type PageMeta struct {
	Title string
}

// PageError is a typed error a Page.Build (or the framework) returns to render a
// styled system page with a specific HTTP status.
type PageError struct {
	Status  int    // 401, 403, 404, 410, 422, 500, 502, ...
	Kind    string // unauthorized | forbidden | notFound | expired | error | info
	Title   string
	Message string
}

func (e *PageError) Error() string { return e.Title }

// Action is an optional button/link rendered on a system page (e.g. "Sign in").
type Action struct {
	Label string `json:"label"`
	Href  string `json:"href"`
}

// systemProps is the bootstrap shape the built-in `system` app consumes.
type systemProps struct {
	Status  int      `json:"status"`            // HTTP status this page reports, also shown in the body
	Kind    string   `json:"kind"`              // which system-page variant to render; see PageError.Kind
	Title   string   `json:"title"`             // headline
	Message string   `json:"message"`           // one-sentence explanation under the headline
	Actions []Action `json:"actions,omitempty"` // buttons to offer; empty ⇒ the page is a dead end
}

// toPageError coerces an arbitrary Build error into a *PageError (generic 500
// when it is not already one).
func toPageError(err error) *PageError {
	if pe, ok := err.(*PageError); ok {
		return pe
	}
	return &PageError{Status: http.StatusInternalServerError, Kind: "error",
		Title: "Something went wrong", Message: "The page could not be rendered."}
}

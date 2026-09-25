package sessions_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/web/browsersession"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
	"github.com/authzed/openagentprimitives/pkg/web/webui/sessions"
)

// fakeDeps implements sessions.Deps via per-behavior func fields, mirroring
// pkg/web/webui/agentui's own fakeDeps. shellPageBuild DOES call
// LookupInteractableSessions and Logger on every request (via
// buildSessionList, list.go) — every test in this file reaches both. The
// zero-value lookupInteractableSessions func answers an empty
// InteractableSessions{}, which leaves buildSessionList's Get fan-out empty,
// which is the ONLY reason k8s can stay a genuinely nil client.Client here
// without panicking: nothing ever calls d.K8s().Get. Any case in this file
// that starts seeding lookupInteractableSessions with non-empty Refs MUST
// also set k8s to a real (e.g. fake.NewClientBuilder()) client — otherwise
// the fan-out dereferences a nil client.Client and panics. list_test.go's
// newListDeps always seeds a real fake client for exactly this reason.
type fakeDeps struct {
	lookupInteractableSessions func(ctx context.Context, canonicalID identity.CanonicalUserID,
		limit uint32, fullyConsistent bool) (spicedb.InteractableSessions, error)
	checkInteract          func(ctx context.Context, ns, name, subject string) (bool, error)
	lookupStartableClasses func(ctx context.Context, canonicalID identity.CanonicalUserID,
		limit uint32, fullyConsistent bool) (spicedb.StartableClasses, error)
	k8s                 client.Client
	startBrowserSession browsersession.StartFunc
	trustedOrigin       string
	// live is the fake live-session table. Declared as the INTERFACE and left
	// unset by every test in this file, so its zero value is a genuine nil
	// interface — never a typed-nil pointer promoted into a non-nil one, which
	// would pass the start handler's nil check and then panic.
	live   sessions.LiveSessions
	logger logr.Logger
}

func (f *fakeDeps) LookupInteractableSessions(ctx context.Context, canonicalID identity.CanonicalUserID,
	limit uint32, fullyConsistent bool,
) (spicedb.InteractableSessions, error) {
	if f.lookupInteractableSessions == nil {
		return spicedb.InteractableSessions{}, nil
	}
	return f.lookupInteractableSessions(ctx, canonicalID, limit, fullyConsistent)
}

// LookupStartableClasses is the start set's bootstrap arm. Defaults to an
// empty set — no bootstrap grant — so these route-level tests exercise the
// derived arm alone; lookupStartableClasses overrides it where a test needs
// the holder or the indeterminate case.
func (f *fakeDeps) LookupStartableClasses(ctx context.Context, canonicalID identity.CanonicalUserID,
	limit uint32, fullyConsistent bool,
) (spicedb.StartableClasses, error) {
	if f.lookupStartableClasses == nil {
		return spicedb.StartableClasses{}, nil
	}
	return f.lookupStartableClasses(ctx, canonicalID, limit, fullyConsistent)
}

func (f *fakeDeps) CheckInteract(ctx context.Context, ns, name, subject string) (bool, error) {
	if f.checkInteract == nil {
		return true, nil
	}
	return f.checkInteract(ctx, ns, name, subject)
}
func (f *fakeDeps) K8s() client.Client                            { return f.k8s }
func (f *fakeDeps) StartBrowserSession() browsersession.StartFunc { return f.startBrowserSession }
func (f *fakeDeps) TrustedOrigin() string                         { return f.trustedOrigin }
func (f *fakeDeps) LiveSessions() sessions.LiveSessions           { return f.live }
func (f *fakeDeps) StartableNamespaces() []string                 { return []string{"demo-ns"} }
func (f *fakeDeps) WorkshopNamespacesFor(context.Context, string) ([]string, error) {
	return nil, nil
}
func (f *fakeDeps) Logger() logr.Logger { return f.logger }

func fakeSessionsDeps() *fakeDeps { return &fakeDeps{logger: logr.Discard()} }

// loggerOnlyDeps implements ONLY Logger() — it deliberately does NOT satisfy
// sessions.Deps (no LookupInteractableSessions, no CheckInteract, no K8s, no
// StartBrowserSession). It stands in for a webd umbrella that is missing a
// required collaborator but still carries a logger — the realistic
// misconfiguration internal/cmd/webd's identity-only *webdDeps produces when the
// viewer's prerequisites are unconfigured (see pkg/web/webui/agentui's own
// loggerOnlyDeps for the precedent this mirrors).
type loggerOnlyDeps struct{ logger logr.Logger }

func (d *loggerOnlyDeps) Logger() logr.Logger { return d.logger }

// TestRoutes_DepsCastFails_NoRoutes proves the fail-closed contract for the
// simplest cast failure: a nil deps value yields no routes, never a panic.
func TestRoutes_DepsCastFails_NoRoutes(t *testing.T) {
	ui := sessions.New()
	assert.Nil(t, ui.Routes(nil))
}

// TestRoutes_DepsCastFails_WrongConcreteType_NoRoutesAndLogsLoudly is the
// mutation-check target: if Routes stopped logging on a cast failure, this
// test's log assertion fails while the routes assertion still passes — proof
// that the log half is not redundant with the routes-nil half and must not be
// trimmed as such.
func TestRoutes_DepsCastFails_WrongConcreteType_NoRoutesAndLogsLoudly(t *testing.T) {
	var mu sync.Mutex
	var logged []string
	capLogger := funcr.New(func(_, args string) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, args)
	}, funcr.Options{})

	d := &loggerOnlyDeps{logger: capLogger}
	ui := sessions.New()

	routes := ui.Routes(d)

	assert.Nil(t, routes, "a deps value missing required collaborators must yield no routes")

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, logged, "a cast failure that can still reach a logger must log, not stay silent")
	joined := strings.Join(logged, "\n")
	assert.Contains(t, joined, "sessions", "log must identify the failing plugin")
	assert.Contains(t, joined, "cast", "log must identify this as a deps-cast failure")
}

// TestRoutes_HappyPath_ReturnsExpectedRouteSet asserts the exact route
// pattern set (by string equality against a literal slice) and both routes'
// full shape: the page route (OriginTrusted, GET-only, AuthLoginIfNecessary
// with a nil Authorize — every authenticated subject may open the list, see
// sessions.go's Routes doc comment for why there is no narrower gate — and a
// declarative Page naming appKey "sessions" with a non-nil Build), and the
// list API route (OriginTrusted, GET-only, AuthAuthenticated with a nil
// Authorize — same "no single resource to gate" reasoning — and a raw
// Handler, not a Page).
func TestRoutes_HappyPath_ReturnsExpectedRouteSet(t *testing.T) {
	d := fakeSessionsDeps()
	ui := sessions.New()

	routes := ui.Routes(d)

	gotPatterns := make([]string, len(routes))
	for i, rt := range routes {
		gotPatterns[i] = rt.Pattern
	}
	assert.Equal(t, []string{"/sessions", "/sessions/api/sessions"}, gotPatterns,
		"the exact route pattern set this plugin mounts")

	require.Len(t, routes, 2)
	page := routes[0]
	assert.Equal(t, webui.OriginTrusted, page.Origin)
	assert.Equal(t, []string{http.MethodGet}, page.Methods)
	assert.Equal(t, webui.AuthLoginIfNecessary, page.Auth)
	assert.Nil(t, page.Authorize, "AuthLoginIfNecessary with a nil Authorize is a supported, checked configuration")
	require.NotNil(t, page.Page, "the route is a declarative Page, not a raw Handler")
	assert.Equal(t, "sessions", page.Page.App)
	assert.NotNil(t, page.Page.Build, "Page.Build must be set so the framework can render")
	// This flag is what puts `frame-src 'self'` in the shell's CSP
	// (webui.Page.FramesSameOrigin; pkg/web/webui/document_test.go's
	// TestBuildCSPFramingFlags pins the directive it emits). Two things the
	// shell renders live or die by it: ap:session_view's /session-view page and
	// ap:chat's /chat-embed/{ns}/{name} iframe (the test chat inside the builder
	// page's Test card). Narrowing the CSP must fail a test that names both.
	assert.True(t, page.Page.FramesSameOrigin, "session shell may frame same-origin: ap:session_view and ap:chat's /chat-embed page depend on it")

	api := routes[1]
	assert.Equal(t, webui.OriginTrusted, api.Origin)
	assert.Equal(t, []string{http.MethodGet}, api.Methods)
	assert.Equal(t, webui.AuthAuthenticated, api.Auth)
	assert.Nil(t, api.Authorize, "AuthAuthenticated with a nil Authorize is a supported, checked configuration")
	assert.Nil(t, api.Page, "the list API route is a raw Handler, not a declarative Page")
	assert.NotNil(t, api.Handler, "Handler must be set so the framework can dispatch to it")
}

// TestName_ReturnsSessions proves the registry key + route pattern agree — a
// mismatch here would let the WebUI register under one name while serving
// paths implying another.
func TestName_ReturnsSessions(t *testing.T) {
	assert.Equal(t, "sessions", sessions.New().Name())
}

// fixtureSubjectDisplay/fixtureSubjectCanonical are a genuinely base64-encoded
// canonical-subject pair — unlike a bare "user:alice" literal, whose failed
// decode falls back to the input unchanged (identity.DecodeForDisplay's own
// doc comment) and so cannot tell a forwarded canonical subject apart from a
// properly decoded one. This pair's canonical form decodes to a DIFFERENT
// string than itself, so the assertions below can tell the two apart.
const fixtureSubjectDisplay = "alice@example.com"

var fixtureSubjectCanonical = "user:" + base64.RawURLEncoding.EncodeToString([]byte(fixtureSubjectDisplay))

// TestShellPageBuild_SubjectInContext_UsesDisplayForm calls the route's own
// Page.Build directly (not through a full webui.Server) with a canonical
// subject injected into context, and proves the emitted props carry the
// DISPLAY form and never the canonical string. This is the mutation-check
// target for "return the canonical subject instead of the display form":
// that mutation fails this assertion but would NOT fail against a subject
// whose display/canonical forms happen to coincide (an un-encoded literal
// like "user:alice") — see the fixture comment above.
func TestShellPageBuild_SubjectInContext_UsesDisplayForm(t *testing.T) {
	d := fakeSessionsDeps()
	routes := sessions.New().Routes(d)
	require.Len(t, routes, 2)

	ctx := webui.WithSubjectForTest(context.Background(), fixtureSubjectCanonical)
	r := httptest.NewRequest(http.MethodGet, "/sessions", nil)

	props, _, err := routes[0].Page.Build(ctx, r)
	require.NoError(t, err)

	raw, err := json.Marshal(props)
	require.NoError(t, err)

	assert.Contains(t, string(raw), `"subject":"`+fixtureSubjectDisplay+`"`)
	assert.NotContains(t, string(raw), fixtureSubjectCanonical,
		"the canonical SpiceDB subject form must never reach the browser")
}

// TestShellPageBuild_NoSubjectInContext_RendersErrorNoPanic covers the case
// the framework itself never reaches in production (an AuthLoginIfNecessary
// route's Build only ever runs once s.authenticate has already succeeded —
// see webui.Server.wrap): calling Build with no subject in context must not
// panic. buildSessionList's empty-canonical guard means this is no longer the
// empty-props happy path it used to be — an empty Sessions slice here would
// be indistinguishable from "you have no sessions" (the exact silent-failure
// shape AGENTS.md's no-silent-errors rule forbids), so this now renders a
// *webui.PageError rather than a fabricated empty list.
func TestShellPageBuild_NoSubjectInContext_RendersErrorNoPanic(t *testing.T) {
	d := fakeSessionsDeps()
	routes := sessions.New().Routes(d)
	require.Len(t, routes, 2)

	r := httptest.NewRequest(http.MethodGet, "/sessions", nil)

	require.NotPanics(t, func() {
		props, _, err := routes[0].Page.Build(context.Background(), r)
		require.Error(t, err, "an empty subject must render an error page, never an empty session list")
		assert.Nil(t, props)

		var pe *webui.PageError
		require.ErrorAs(t, err, &pe, "Build must return a *webui.PageError so the framework renders a styled page")
		assert.Equal(t, http.StatusUnauthorized, pe.Status)
	})
}

// TestShellPage_RefusalRendersTheStyledSystemPage is this plugin's own
// plugin-level pin that a *webui.PageError becomes a rendered system page,
// through a REAL webui.Server rather than a direct Build call.
//
// The framework guarantees it generically, and both sibling page plugins
// (sessionview, artifactview) keep a pin of their own. The shell is the one
// page whose refusals a viewer will actually meet — a selection they may not
// interact with, a SpiceDB outage — so a raw body or a blank 403 here is the
// most visible version of that failure and the least covered.
//
// The two rows are the pair the framework must never conflate: a clean denial
// is 403, and an authorization ERROR is 500, never a denial.
func TestShellPage_RefusalRendersTheStyledSystemPage(t *testing.T) {
	const trusted = "trusted.example"
	subject := "user:" + base64.RawURLEncoding.EncodeToString([]byte(fixtureSubjectDisplay))

	cases := []struct {
		name          string
		checkInteract func(ctx context.Context, ns, name, subject string) (bool, error)
		wantStatus    int
	}{
		{
			name:          "a selection the viewer may not interact with: 403, styled",
			checkInteract: func(context.Context, string, string, string) (bool, error) { return false, nil },
			wantStatus:    http.StatusForbidden,
		},
		{
			name: "an authorization error: 500, styled, never a denial",
			checkInteract: func(context.Context, string, string, string) (bool, error) {
				return false, assert.AnError
			},
			wantStatus: http.StatusInternalServerError,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := fakeSessionsDeps()
			d.checkInteract = tc.checkInteract

			s, err := webui.NewServer(
				func(*http.Request) (string, bool) { return subject, true }, nil,
				func() string { return trusted }, func() string { return "sandbox.example" },
				nil, d, []webui.WebUI{sessions.New()})
			require.NoError(t, err)

			rec := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "http://"+trusted+"/sessions?session=demo-ns/demo-session", nil)
			r.Host = trusted
			s.ServeHTTP(rec, r)

			assert.Equal(t, tc.wantStatus, rec.Code)
			assert.Contains(t, rec.Body.String(), `data-app="system"`,
				"a refusal must render the styled system page, never a raw body")
			assert.NotContains(t, rec.Body.String(), "assert.AnError",
				"and must not carry the underlying cause to the browser")
		})
	}
}

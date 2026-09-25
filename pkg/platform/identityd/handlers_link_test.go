package identityd

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientpkg "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	fakekind "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
)

// stubVerify pins the live-probe outcome for the test's lifetime. Every
// submit test whose credential resolves to a provider with an HTTP
// verify: probe (e.g. "github-token" → github-pat) MUST call this before
// posting — otherwise builtins.VerifyCredential falls through to the real
// safehttp client and the test either hits the network or hangs on
// safehttp's loopback block.
func stubVerify(t *testing.T, status int, body string) {
	t.Helper()
	builtins.SetVerifyHTTPClient(func() *http.Client {
		return &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: status,
				Body:       io.NopCloser(strings.NewReader(body)),
				Header:     http.Header{"Content-Type": []string{"application/json"}},
			}, nil
		})}
	})
	t.Cleanup(func() { builtins.SetVerifyHTTPClient(nil) })
}

// stubVerdictFlow is a registered builtin flow that answers a fixed verdict.
// It is named for github-pat's builtin: key so builtins.VerifyCredential
// resolves to it and short-circuits the HTTP probe — the only seam through
// which a test can hand these handlers a verdict VerifyHTTPBearer would never
// produce on its own.
type stubVerdictFlow struct{ res builtins.VerifyResult }

func (stubVerdictFlow) Name() string { return "github-pat" }
func (stubVerdictFlow) Screens(context.Context, builtins.Request) ([]tui.Screen, error) {
	return nil, errors.New("stubVerdictFlow asks nothing; it exists for its Verify")
}
func (stubVerdictFlow) Result(context.Context, builtins.Request, *tui.State) error {
	return errors.New("stubVerdictFlow stores nothing; it exists for its Verify")
}
func (f stubVerdictFlow) Verify(context.Context, builtins.VerifyRequest) (builtins.VerifyResult, error) {
	return f.res, nil
}

// stubVerifyVerdict makes the credential's live check answer res, whatever
// res is. The registry is emptied again on cleanup: no other test in this
// package registers a flow, and several rely on there being none.
func stubVerifyVerdict(t *testing.T, res builtins.VerifyResult) {
	t.Helper()
	builtins.Reset()
	builtins.Register(stubVerdictFlow{res: res})
	t.Cleanup(builtins.Reset)
}

// roundTripFunc adapts a function to http.RoundTripper — the single
// declaration for the whole identityd test package (handlers_portal_test.go
// reuses this one rather than redeclaring it).
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// testTrustedHost is the host the test webui.Server serves the trusted
// (auth) origin on. Requests routed through fxHandler must carry this as
// their Host header so the framework dispatches to identityd's routes.
const testTrustedHost = "identityd.test"

// fxWebDeps adapts a linkFixture to the identityd.WebDeps interface so a
// real webui.Server can mount identityd's routes against the SAME fake
// K8s client + signer the fixture seeded. This routes browser-page tests
// through webui.Server.ServeHTTP, which injects the framework renderer
// (RendererFromContext) — without it the page handlers have no renderer
// and produce no React document.
type fxWebDeps struct{ fx linkFixture }

func (d fxWebDeps) K8s() clientpkg.Client               { return d.fx.c }
func (d fxWebDeps) LinkSigner() *passthroughlink.Signer { return d.fx.signer }
func (d fxWebDeps) ExternalBaseURL() string             { return "https://identityd.example.org" }
func (d fxWebDeps) IconHandler() http.Handler           { return nil }
func (d fxWebDeps) Authenticators() map[string]channelkinds.WebAuthenticator {
	return d.fx.srv.deps.Authenticators
}
func (d fxWebDeps) InsecureTrustLinks() bool { return d.fx.srv.deps.InsecureTrustLinks }

// buildWebHandler builds the webui.Server mounting identityd's routes
// against the fixture's deps. Called ONCE per fixture so the internal
// identityd Server's state stores persist across a test's requests.
//
// The framework's AuthAuthenticated gate is satisfied by a permissive
// authenticate func (always returns a subject) — identityd's handlers do
// their OWN idd_session cookie gating internally, which is what the auth
// tests assert. The webui subject is orthogonal to the cookie subject;
// making the framework gate permissive lets the request reach the
// handler so its real cookie checks run.
func buildWebHandler(t *testing.T, fx linkFixture) http.Handler {
	t.Helper()
	srv, err := webui.NewServer(
		func(*http.Request) (string, bool) { return "framework-subject", true },
		nil,
		func() string { return testTrustedHost },
		func() string { return "sandbox.test" },
		nil,
		fxWebDeps{fx: fx},
		[]webui.WebUI{ui{}},
	)
	require.NoError(t, err, "identityd routes must mount on the webui framework")
	return srv
}

// serveWeb dispatches req through the fixture's webui.Server, setting the
// trusted Host so the framework routes to identityd. Returns the recorder.
func serveWeb(t *testing.T, fx linkFixture, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	require.NotNil(t, fx.web, "fixture must be built with a webui handler (newLinkFixture*)")
	req.Host = testTrustedHost
	rec := httptest.NewRecorder()
	fx.web.ServeHTTP(rec, req)
	return rec
}

// signerKey is reused across tests so all minted links share an HMAC
// key with the server under test. A short value is fine — HMAC-SHA256
// doesn't care about key length, only that it's the same on both sides.
var signerKey = []byte("test-link-signing-key")

// linkFixture is the shared setup the handler-table tests build on.
// All cases need a Server, a Signer (same key as the Server), and a
// fake K8s client. Some cases pre-seed an AgentSession; some don't.
//
// web is a single webui.Server mounting identityd's routes against the
// SAME deps, built once so its internal identityd Server (and its
// per-process state stores — consumedLinks, oauthState, …) persists
// across the multiple requests a test makes. Browser-page + redirect
// tests dispatch through it; the oauth/oidc tests still drive srv
// directly (they don't need the framework renderer).
type linkFixture struct {
	srv    *Server
	signer *passthroughlink.Signer
	c      clientpkg.Client
	web    http.Handler
}

// newScheme returns a runtime.Scheme registered for the APIs our
// fake client touches: core/v1 (Secret), spicebox v1alpha1 (CRDs).
func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s))
	return s
}

// newLinkFixture builds a fixture with the given pre-seeded objects.
// Tests pass nil or omit objects entirely when they want a virgin
// fake client.
func newLinkFixture(t *testing.T, objs ...clientpkg.Object) linkFixture {
	t.Helper()
	scheme := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	signer := passthroughlink.New(signerKey)
	srv := NewServer(Deps{
		K8s:             c,
		LinkSigner:      signer,
		ExternalBaseURL: func() string { return "https://identityd.example.org" },
		Authenticators: map[string]channelkinds.WebAuthenticator{
			"fake": fakeAuthenticator{externalBaseURL: "https://identityd.example.org"},
		},
	})
	fx := linkFixture{srv: srv, signer: signer, c: c}
	fx.web = buildWebHandler(t, fx)
	return fx
}

// newLinkFixtureNoAuth builds a fixture with NO authenticators and
// InsecureTrustLinks=true — the pre-IdP trust-link behavior for tests
// that exercise the OIDC-off path. In production, this flag is false;
// tests here are explicitly exercising the insecure-trust fallback.
func newLinkFixtureNoAuth(t *testing.T, objs ...clientpkg.Object) linkFixture {
	t.Helper()
	scheme := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	signer := passthroughlink.New(signerKey)
	srv := NewServer(Deps{
		K8s:                c,
		LinkSigner:         signer,
		ExternalBaseURL:    func() string { return "https://identityd.example.org" },
		Authenticators:     map[string]channelkinds.WebAuthenticator{}, // empty → OIDC-off
		InsecureTrustLinks: true,                                       // dev-only; tests use this path explicitly
	})
	fx := linkFixture{srv: srv, signer: signer, c: c}
	fx.web = buildWebHandler(t, fx)
	return fx
}

// fakeAuthenticator is a hand-rolled fake WebAuthenticator: tests
// don't need the package-global state of channelkinds/fake, just a
// predictable Begin URL. (We still import the real one above so the
// state-store seeding path works for end-to-end style tests.)
type fakeAuthenticator struct{ externalBaseURL string }

func (a fakeAuthenticator) Begin(_ context.Context, state string) (string, error) {
	return a.externalBaseURL + "/oidc/callback/fake?state=" + url.QueryEscape(state), nil
}
func (a fakeAuthenticator) Complete(_ context.Context, _ channelkinds.CallbackParams) (string, error) {
	return "", nil
}

// mintLink returns the "<b64>.<sig>" form for a link with the given
// fields. ExpiresAt defaults to 5min in the future when in == 0; pass
// a past time explicitly to test the expired path.
func mintLink(t *testing.T, sgn *passthroughlink.Signer, sessRef string, subject identity.Subject, creds []string, exp time.Time) string {
	t.Helper()
	if exp.IsZero() {
		exp = time.Now().Add(5 * time.Minute)
	}
	// Tests share one Signer for both producer + consumer; explicitly
	// stamp iss=channelsd on deep-links so identityd's verifier accepts
	// them (it expects iss=channelsd, aud=identityd on /link inputs).
	raw, err := sgn.Mint(passthroughlink.Payload{
		Issuer:              passthroughlink.IssuerChannelsd,
		Audience:            passthroughlink.AudienceIdentityd,
		SessionRef:          sessRef,
		Subject:             subject,
		RequiredCredentials: creds,
		ExpiresAt:           exp.Unix(),
	})
	require.NoError(t, err)
	return raw
}

// mintCookie mints a cookie-shaped payload (Subject + ExpiresAt only)
// the way identityd's /oidc/callback (D3) will. SessionRef is empty by
// the convention documented on the cookieName constant.
func mintCookie(t *testing.T, sgn *passthroughlink.Signer, subject identity.Subject, exp time.Time) string {
	t.Helper()
	if exp.IsZero() {
		exp = time.Now().Add(10 * time.Minute)
	}
	// Tests share one Signer for both producer + consumer; explicitly
	// stamp iss=identityd on cookies so checkOIDCCookie accepts them.
	raw, err := sgn.Mint(passthroughlink.Payload{
		Issuer:    passthroughlink.IssuerIdentityd,
		Audience:  passthroughlink.AudienceIdentityd,
		Subject:   subject,
		ExpiresAt: exp.Unix(),
	})
	require.NoError(t, err)
	return raw
}

// makeAgentSession returns a parked AgentSession annotated with the
// canonical starter — the gate the /link recipient check exercises.
func makeAgentSession(ns, name, starterCanonical string) *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Annotations: map[string]string{
				spiceboxv1alpha1.AnnotationStartedByCanonicalID: starterCanonical,
			},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:  "pass-cls",
			Prompt: spiceboxv1alpha1.PromptSource{Inline: "do the work"},
		},
	}
}

// doGET sends a GET to /link with raw split into d + sig + optional
// extra query and the optional cookie. Routes through the webui
// framework (so the page renderer is injected). Returns the recorder.
func doGET(t *testing.T, fx linkFixture, raw, extraQuery, cookieValue string) *httptest.ResponseRecorder {
	t.Helper()
	d, sig, ok := splitRaw(raw)
	q := url.Values{}
	if ok {
		q.Set("d", d)
		q.Set("sig", sig)
	}
	if extraQuery != "" {
		for k, vs := range parseQuery(t, extraQuery) {
			for _, v := range vs {
				q.Add(k, v)
			}
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/link?"+q.Encode(), nil)
	if cookieValue != "" {
		req.AddCookie(&http.Cookie{Name: cookieName, Value: cookieValue})
	}
	return serveWeb(t, fx, req)
}

// splitRaw lifts the "<b64>.<sig>" structure to the d/sig query
// params /link expects.
func splitRaw(raw string) (d, sig string, ok bool) {
	parts := strings.SplitN(raw, ".", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func parseQuery(t *testing.T, q string) url.Values {
	t.Helper()
	v, err := url.ParseQuery(q)
	require.NoError(t, err)
	return v
}

// === GET /link tests =========================================================

func TestHandleLinkGet_HappyPath(t *testing.T) {
	const (
		ns         = "default"
		name       = "sess-1"
		canonical  = "user:alice@example.com"
		credGitHub = "github-pat"
		credLinear = "linear-pat"
	)
	fx := newLinkFixture(t, makeAgentSession(ns, name, canonical))
	raw := mintLink(t, fx.signer, ns+"/"+name, canonical, []string{credGitHub, credLinear}, time.Time{})
	cookie := mintCookie(t, fx.signer, canonical, time.Time{})

	rec := doGET(t, fx, raw, "", cookie)
	require.Equal(t, http.StatusOK, rec.Code, "happy path must render the menu")
	body := rec.Body.String()
	// The menu is now the React identity-link app; the Go handler emits
	// the document mounting it + the JSON bootstrap props the app reads.
	assert.Contains(t, body, `data-app="identity-link"`, "mounts the identity-link React app")
	// Bootstrap props carry the session + one row per required credential.
	assert.Contains(t, body, `"sessionRef":`, "bootstrap carries the session ref")
	assert.Contains(t, body, `"rows":`, "bootstrap carries the per-credential rows")
	assert.Contains(t, body, `"credentialName":"github-pat"`, "github-pat row present in bootstrap")
	assert.Contains(t, body, `"credentialName":"linear-pat"`, "linear-pat row present in bootstrap")
	assert.Contains(t, body, `"signedLink":`, "bootstrap carries the signed link for the row Save forms")
	// The imperative appKey MUST resolve to a built manifest entry — the
	// module script tag under /assets/ is the only guard for that.
	assert.Contains(t, body, `<script type="module" src="/assets/`,
		"the resolved app must have a built entry script in the manifest")
}

func TestHandleLinkGet_ErrorCases(t *testing.T) {
	const (
		ns        = "default"
		name      = "sess-1"
		canonical = "user:alice@example.com"
	)
	otherCanonical := identity.Subject("user:bob@example.com")

	cases := []struct {
		name             string
		seedAgentSession bool
		linkSubject      identity.Subject
		linkSessRef      string
		linkExp          time.Time // zero → future
		tamperSig        bool
		cookieSubject    identity.Subject // "" → no cookie
		cookieExp        time.Time
		wantStatus       int
		wantContains     string
	}{
		{
			name:             "invalid signature: tampered sig → 400 Invalid link",
			seedAgentSession: true,
			linkSubject:      canonical,
			linkSessRef:      ns + "/" + name,
			tamperSig:        true,
			cookieSubject:    canonical,
			wantStatus:       http.StatusBadRequest,
			wantContains:     "Invalid link",
		},
		{
			name:             "expired link: ExpiresAt in past → 400 Link expired",
			seedAgentSession: true,
			linkSubject:      canonical,
			linkSessRef:      ns + "/" + name,
			linkExp:          time.Now().Add(-1 * time.Hour),
			cookieSubject:    canonical,
			wantStatus:       http.StatusBadRequest,
			wantContains:     "Link expired",
		},
		{
			name:             "session not found: no AgentSession → 404",
			seedAgentSession: false,
			linkSubject:      canonical,
			linkSessRef:      ns + "/" + name,
			cookieSubject:    canonical,
			wantStatus:       http.StatusNotFound,
			wantContains:     "Session not found",
		},
		{
			name:             "starter mismatch: AgentSession annotated for someone else → 403",
			seedAgentSession: true,
			linkSubject:      otherCanonical, // link claims bob; session starter is alice
			linkSessRef:      ns + "/" + name,
			cookieSubject:    otherCanonical, // make cookie pass-through to expose the gate
			cookieExp:        time.Now().Add(10 * time.Minute),
			wantStatus:       http.StatusForbidden,
			wantContains:     "Not your session",
		},
		{
			name:             "cookie subject mismatch: cookie != link subject → 403",
			seedAgentSession: true,
			linkSubject:      canonical,
			linkSessRef:      ns + "/" + name,
			cookieSubject:    otherCanonical,
			wantStatus:       http.StatusForbidden,
			wantContains:     "Not your session",
		},
		{
			name:             "malformed sessionRef: no slash → 400 Invalid link",
			seedAgentSession: false,
			linkSubject:      canonical,
			linkSessRef:      "no-slash-here",
			cookieSubject:    canonical,
			wantStatus:       http.StatusBadRequest,
			wantContains:     "Invalid link",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var fx linkFixture
			if tc.seedAgentSession {
				fx = newLinkFixture(t, makeAgentSession(ns, name, canonical))
			} else {
				fx = newLinkFixture(t)
			}
			raw := mintLink(t, fx.signer, tc.linkSessRef, tc.linkSubject, []string{"github-pat"}, tc.linkExp)
			if tc.tamperSig {
				// Flip a character in the sig portion. Either flipping
				// the leading hex digit or appending garbage produces an
				// HMAC mismatch.
				raw = raw + "00"
			}
			var cookieVal string
			if tc.cookieSubject != "" {
				cookieVal = mintCookie(t, fx.signer, tc.cookieSubject, tc.cookieExp)
			}
			rec := doGET(t, fx, raw, "", cookieVal)
			assert.Equal(t, tc.wantStatus, rec.Code, "status code mismatch")
			assert.Contains(t, rec.Body.String(), tc.wantContains, "error body mismatch")
		})
	}
}

// TestHandleLinkGet_OAuthRowRendersConnectButton pins the contract
// that the /link menu renders an OAuth-typed credential as a Connect
// button → /link/oauth/<credname>, NOT an inline PAT form. This is
// the failure mode a user reported: the Slack ephemeral linked to
// /link, the menu page showed a token input for Linear, defeating
// the whole OAuth flow.
//
// The MCPServer shape mirrors a real-world passthrough MCPServer
// (auth.type=oauth, provider=Linear, credential=linear-oauth).
func TestHandleLinkGet_OAuthRowRendersConnectButton(t *testing.T) {
	const (
		ns        = "default"
		name      = "sess-1"
		canonical = "user:alice@example.com"
		credName  = "linear-oauth"
	)
	mcpServer := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "linear-readonly-passthrough"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Name:    "linear-readonly-passthrough",
			Version: "1",
			Server: spiceboxv1alpha1.MCPServerServer{
				URL:       "https://mcp.linear.app/mcp",
				Transport: "streamable-http",
			},
			Auth: spiceboxv1alpha1.MCPServerAuth{
				Type:       "oauth",
				Provider:   "Linear",
				Credential: credName,
			},
		},
	}
	fx := newLinkFixture(t, makeAgentSession(ns, name, canonical), mcpServer)
	raw := mintLink(t, fx.signer, ns+"/"+name, canonical, []string{credName}, time.Time{})
	cookie := mintCookie(t, fx.signer, canonical, time.Time{})

	rec := doGET(t, fx, raw, "", cookie)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()

	// The OAuth-vs-PAT decision is now carried in the row's bootstrap
	// props (kind + oauthUrl + label); the React app renders the Connect
	// button vs the PAT form from them.
	assert.Contains(t, body, `data-app="identity-link"`)
	assert.Contains(t, body, `"kind":"oauth"`,
		"OAuth-typed credential MUST be marked kind=oauth in the bootstrap row")
	assert.Contains(t, body, `"oauthUrl":"https://identityd.example.org/link/oauth/`+credName+`"`,
		"OAuth row MUST carry the /link/oauth/<credname> URL the Connect button uses")
	assert.Contains(t, body, `"label":"Linear"`,
		"OAuth row MUST surface the provider name from Spec.Auth.Provider")
	assert.NotContains(t, body, `"kind":"pat"`,
		"the single OAuth credential MUST NOT be marked as a PAT row")
}

func TestHandleLinkGet_NoCookieRedirectsToOIDCLogin(t *testing.T) {
	const (
		ns        = "default"
		name      = "sess-1"
		canonical = "user:alice@example.com"
	)
	fx := newLinkFixture(t, makeAgentSession(ns, name, canonical))
	raw := mintLink(t, fx.signer, ns+"/"+name, canonical, []string{"github-pat"}, time.Time{})

	rec := doGET(t, fx, raw, "", "")
	require.Equal(t, http.StatusFound, rec.Code, "must redirect (302) when no cookie")
	loc := rec.Header().Get("Location")
	assert.True(t, strings.HasPrefix(loc, "/oidc/login?"), "cookieless /link delegates to /oidc/login, got %q", loc)
	assert.Contains(t, loc, "d=", "redirect carries the link payload")
	assert.Contains(t, loc, "sig=", "redirect carries the link signature")
}

func TestHandleLinkGet_NoCookieRedirectsEvenWithNoAuthenticator(t *testing.T) {
	// With no authenticators registered, cookieless /link still delegates to
	// /oidc/login (which owns trust-link / cluster-IdP / channel-kind) rather
	// than handling sign-in inline.
	const (
		ns        = "default"
		name      = "sess-1"
		canonical = "user:alice@example.com"
	)
	fx := newLinkFixtureNoAuth(t, makeAgentSession(ns, name, canonical))
	raw := mintLink(t, fx.signer, ns+"/"+name, canonical, []string{"demo-pat"}, time.Time{})

	rec := doGET(t, fx, raw, "", "")
	require.Equal(t, http.StatusFound, rec.Code, "cookieless → 302 to /oidc/login even with no authenticator")
	assert.True(t, strings.HasPrefix(rec.Header().Get("Location"), "/oidc/login?"))
}

func TestHandleLinkGet_CookieSubjectMismatchBlocked(t *testing.T) {
	// A present-but-mismatched cookie must be rejected 403 regardless of
	// OIDC mode. The subject-equality gate applies whenever a cookie IS
	// present — it is the cookie-present path that enforces signed-in user
	// == link subject == starter.
	const (
		ns           = "default"
		name         = "sess-1"
		canonical    = "user:alice@example.com"
		otherSubject = "user:bob@example.com"
	)
	fx := newLinkFixtureNoAuth(t, makeAgentSession(ns, name, canonical))
	raw := mintLink(t, fx.signer, ns+"/"+name, canonical, []string{"demo-pat"}, time.Time{})
	wrongCookie := mintCookie(t, fx.signer, otherSubject, time.Time{})

	rec := doGET(t, fx, raw, "", wrongCookie)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "Not your session")
}

func TestHandleLinkGet_MissingQueryParams(t *testing.T) {
	fx := newLinkFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/link", nil) // no d, no sig
	rec := serveWeb(t, fx, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	// Error renders the framework system page.
	assert.Contains(t, rec.Body.String(), `data-app="system"`)
	assert.Contains(t, rec.Body.String(), "Invalid link")
}

func TestHandleLinkGet_MethodNotAllowed(t *testing.T) {
	fx := newLinkFixture(t)
	req := httptest.NewRequest(http.MethodPost, "/link", nil)
	rec := serveWeb(t, fx, req)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	assert.Equal(t, http.MethodGet, rec.Header().Get("Allow"))
}

// === POST /link/submit tests =================================================

// doPOST sends a form-encoded submit with the given fields + optional
// cookie. Routes through the webui framework. Returns the recorder.
func doPOST(t *testing.T, fx linkFixture, form url.Values, cookieValue string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/link/submit", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookieValue != "" {
		req.AddCookie(&http.Cookie{Name: cookieName, Value: cookieValue})
	}
	return serveWeb(t, fx, req)
}

func TestHandleLinkSubmit_HappyPath(t *testing.T) {
	stubVerify(t, http.StatusOK, `{"login":"tester"}`)
	const (
		ns         = "default"
		name       = "sess-1"
		canonical  = "user:alice@example.com"
		credName   = "github-pat"
		tokenValue = "ghp_aliceTOKEN"
	)
	fx := newLinkFixture(t, makeAgentSession(ns, name, canonical))
	raw := mintLink(t, fx.signer, ns+"/"+name, canonical, []string{credName}, time.Time{})
	cookie := mintCookie(t, fx.signer, canonical, time.Time{})

	form := url.Values{"link": {raw}, "credential": {credName}, "token": {tokenValue}}
	rec := doPOST(t, fx, form, cookie)
	require.Equal(t, http.StatusFound, rec.Code, "success → 302 redirect back to /link menu")
	loc := rec.Header().Get("Location")
	assert.Contains(t, loc, "/link?d=", "redirect must point at the menu page so user sees the new row status")

	// Cluster state: UserIdentity + Secret should exist.
	uiName := useridentity.NameForSubject(canonical)
	var ui spiceboxv1alpha1.UserIdentity
	require.NoError(t, fx.c.Get(context.Background(), clientpkg.ObjectKey{Name: uiName}, &ui),
		"UserIdentity must be created")
	require.Len(t, ui.Spec.Credentials, 1)
	assert.Equal(t, credName, ui.Spec.Credentials[0].Name)
	assert.Equal(t, "static", ui.Spec.Credentials[0].Type)

	secName := useridentity.MasterSecretName(uiName, credName)
	var sec corev1.Secret
	require.NoError(t, fx.c.Get(context.Background(),
		clientpkg.ObjectKey{Namespace: spiceboxv1alpha1.IdentitiesNamespace, Name: secName}, &sec),
		"Master Secret must be created")
	assert.Equal(t, tokenValue, string(sec.Data["token"]),
		"the bearer token must be stored under data['token']")
}

// TestHandleLinkSubmit_VerifyRejectedRendersWarnPage — a format-valid but
// live-rejected token (the provider's verify: probe answers 401) must NOT be
// stored. Instead the handler renders the identity-verify-warn confirm page
// carrying the rejection detail + a re-submit form.
//
// "github-token" is the credential name the gh.yaml toolkit declares for
// GITHUB_TOKEN (provider: github-pat) — unlike the "github-pat" name used by
// the other submit tests in this file, it actually resolves through
// passthroughcatalog.ProviderForCredential, so verification really runs.
func TestHandleLinkSubmit_VerifyRejectedRendersWarnPage(t *testing.T) {
	stubVerify(t, http.StatusUnauthorized, `{"message":"Bad credentials"}`)
	const (
		ns         = "default"
		name       = "sess-1"
		canonical  = "user:alice@example.com"
		credName   = "github-token"
		tokenValue = "ghp_dead"
	)
	fx := newLinkFixture(t, makeAgentSession(ns, name, canonical))
	raw := mintLink(t, fx.signer, ns+"/"+name, canonical, []string{credName}, time.Time{})
	cookie := mintCookie(t, fx.signer, canonical, time.Time{})

	form := url.Values{"link": {raw}, "credential": {credName}, "token": {tokenValue}}
	rec := doPOST(t, fx, form, cookie)
	require.Equal(t, http.StatusOK, rec.Code, "rejected verification renders the warn page, not a redirect")
	body := rec.Body.String()
	assert.Contains(t, body, `data-app="identity-verify-warn"`, "mounts the identity-verify-warn React app")
	assert.Contains(t, body, "Bad credentials", "the provider's rejection detail must be surfaced")
	assert.Contains(t, body, `"verifyConfirm":"1"`, "the re-submit form carries verifyConfirm=1")

	uiName := useridentity.NameForSubject(canonical)
	var ui spiceboxv1alpha1.UserIdentity
	err := fx.c.Get(context.Background(), clientpkg.ObjectKey{Name: uiName}, &ui)
	assert.True(t, apierrors.IsNotFound(err), "a rejected token must NOT be stored; got err=%v", err)
}

// TestHandleLinkSubmit_VerifyForbiddenRendersWarnPage — the provider accepted
// the credential and refused this one check (403). Nothing is stored until the
// visitor explicitly confirms, and the page must tell them the truth: their
// credential authenticated, this check did not pass. Calling that a rejection
// is what makes people replace credentials that work.
func TestHandleLinkSubmit_VerifyForbiddenRendersWarnPage(t *testing.T) {
	stubVerify(t, http.StatusForbidden,
		`{"message":"Resource protected by organization SAML enforcement."}`)
	const (
		ns         = "default"
		name       = "sess-1"
		canonical  = "user:alice@example.com"
		credName   = "github-token"
		tokenValue = "ghp_liveButSSORestricted"
	)
	fx := newLinkFixture(t, makeAgentSession(ns, name, canonical))
	raw := mintLink(t, fx.signer, ns+"/"+name, canonical, []string{credName}, time.Time{})
	cookie := mintCookie(t, fx.signer, canonical, time.Time{})

	form := url.Values{"link": {raw}, "credential": {credName}, "token": {tokenValue}}
	rec := doPOST(t, fx, form, cookie)
	require.Equal(t, http.StatusOK, rec.Code,
		"a refused check renders the confirm page, not a store-and-redirect; body=%s", rec.Body.String())
	body := rec.Body.String()
	assert.Contains(t, body, `data-app="identity-verify-warn"`, "mounts the identity-verify-warn React app")
	assert.Contains(t, body, "authenticated but was refused for this check",
		"the page must say the credential authenticated and this check did not pass")
	assert.Contains(t, body, "SAML enforcement", "the provider's own reason must be surfaced")
	assert.Contains(t, body, `"verifyConfirm":"1"`, "the re-submit form carries verifyConfirm=1")

	uiName := useridentity.NameForSubject(canonical)
	var ui spiceboxv1alpha1.UserIdentity
	err := fx.c.Get(context.Background(), clientpkg.ObjectKey{Name: uiName}, &ui)
	assert.True(t, apierrors.IsNotFound(err),
		"nothing may be stored before the visitor confirms; got err=%v", err)
}

// TestHandleLinkSubmit_UnrecognizedVerdictRendersWarnPage covers the submit
// switch's default arm. A verdict this build has no branch for must not be
// read as permission to store: the browser surface can ask, so it asks, and
// nothing is written until the visitor says yes.
//
// TestHandleLinkSubmit_VerifyConfirmStores is this test's control — it proves
// the same warn page's "store anyway" re-submit does write — so a pass here
// cannot come from the write path being broken outright.
func TestHandleLinkSubmit_UnrecognizedVerdictRendersWarnPage(t *testing.T) {
	stubVerifyVerdict(t, builtins.VerifyResult{
		Status: "verdict-this-build-does-not-know", Detail: "a verdict from a later build",
	})
	const (
		ns         = "default"
		name       = "sess-1"
		canonical  = "user:alice@example.com"
		credName   = "github-token"
		tokenValue = "ghp_unknownverdict"
	)
	fx := newLinkFixture(t, makeAgentSession(ns, name, canonical))
	raw := mintLink(t, fx.signer, ns+"/"+name, canonical, []string{credName}, time.Time{})
	cookie := mintCookie(t, fx.signer, canonical, time.Time{})

	form := url.Values{"link": {raw}, "credential": {credName}, "token": {tokenValue}}
	rec := doPOST(t, fx, form, cookie)
	require.Equal(t, http.StatusOK, rec.Code,
		"an unrecognised verdict renders the confirm page, not a store-and-redirect; body=%s", rec.Body.String())
	body := rec.Body.String()
	assert.Contains(t, body, `data-app="identity-verify-warn"`, "mounts the identity-verify-warn React app")
	assert.Contains(t, body, `"verifyConfirm":"1"`, "the re-submit form carries verifyConfirm=1")

	uiName := useridentity.NameForSubject(canonical)
	var ui spiceboxv1alpha1.UserIdentity
	err := fx.c.Get(context.Background(), clientpkg.ObjectKey{Name: uiName}, &ui)
	assert.True(t, apierrors.IsNotFound(err),
		"a credential must NOT be stored on a verdict this build does not understand; got err=%v", err)
}

// TestHandleLinkSubmit_VerifyConfirmStores — a verifyConfirm=1 re-submit
// (the "Store anyway" button on the warn page) stores the token even though
// the same rejection would occur again, and the redirect notice tells the
// menu page the credential was linked unverified.
func TestHandleLinkSubmit_VerifyConfirmStores(t *testing.T) {
	stubVerify(t, http.StatusUnauthorized, `{"message":"Bad credentials"}`)
	const (
		ns         = "default"
		name       = "sess-1"
		canonical  = "user:alice@example.com"
		credName   = "github-token"
		tokenValue = "ghp_dead"
	)
	fx := newLinkFixture(t, makeAgentSession(ns, name, canonical))
	raw := mintLink(t, fx.signer, ns+"/"+name, canonical, []string{credName}, time.Time{})
	cookie := mintCookie(t, fx.signer, canonical, time.Time{})

	d, sig, ok := splitRaw(raw)
	require.True(t, ok)
	form := url.Values{"link": {raw}, "credential": {credName}, "token": {tokenValue}, "verifyConfirm": {"1"}}
	rec := doPOST(t, fx, form, cookie)
	require.Equal(t, http.StatusFound, rec.Code, "verifyConfirm=1 stores + redirects despite the rejection")
	assert.Equal(t, "/link?d="+d+"&sig="+sig+"&notice=linkedunverified%3Agithub-token", rec.Header().Get("Location"))

	uiName := useridentity.NameForSubject(canonical)
	var ui spiceboxv1alpha1.UserIdentity
	require.NoError(t, fx.c.Get(context.Background(), clientpkg.ObjectKey{Name: uiName}, &ui),
		"UserIdentity must be created on a confirmed store")
	require.Len(t, ui.Spec.Credentials, 1)
	assert.Equal(t, credName, ui.Spec.Credentials[0].Name)
}

// TestHandleLinkSubmit_VerifyValidStores — a token the provider's live probe
// accepts stores + redirects exactly like the pre-verification happy path.
func TestHandleLinkSubmit_VerifyValidStores(t *testing.T) {
	stubVerify(t, http.StatusOK, `{"login":"octocat"}`)
	const (
		ns         = "default"
		name       = "sess-1"
		canonical  = "user:alice@example.com"
		credName   = "github-token"
		tokenValue = "ghp_dead"
	)
	fx := newLinkFixture(t, makeAgentSession(ns, name, canonical))
	raw := mintLink(t, fx.signer, ns+"/"+name, canonical, []string{credName}, time.Time{})
	cookie := mintCookie(t, fx.signer, canonical, time.Time{})

	d, sig, ok := splitRaw(raw)
	require.True(t, ok)
	form := url.Values{"link": {raw}, "credential": {credName}, "token": {tokenValue}}
	rec := doPOST(t, fx, form, cookie)
	require.Equal(t, http.StatusFound, rec.Code, "a live-verified token stores + redirects")
	assert.Equal(t, "/link?d="+d+"&sig="+sig+"&notice=linked%3Agithub-token", rec.Header().Get("Location"))

	uiName := useridentity.NameForSubject(canonical)
	var ui spiceboxv1alpha1.UserIdentity
	require.NoError(t, fx.c.Get(context.Background(), clientpkg.ObjectKey{Name: uiName}, &ui))
	require.Len(t, ui.Spec.Credentials, 1)
	assert.Equal(t, credName, ui.Spec.Credentials[0].Name)
}

// TestHandleLinkSubmit_RejectsWrongTokenFormat — a token that doesn't match
// the credential's declared provider format is refused at the paste form (the
// live bug: a wrong anthropic-oauth value stored silently and only failed
// later as an opaque 401). The credential "anthropic-oauth" resolves to the
// anthropic-oauth provider via the embedded toolkit catalog, so no MCPServer
// seeding is needed. A bad paste must NOT write a Secret; a correctly-shaped
// one must.
func TestHandleLinkSubmit_RejectsWrongTokenFormat(t *testing.T) {
	const (
		ns        = "default"
		name      = "sess-fmt"
		canonical = "user:alice@example.com"
		credName  = "anthropic-oauth"
	)
	fx := newLinkFixture(t, makeAgentSession(ns, name, canonical))
	raw := mintLink(t, fx.signer, ns+"/"+name, canonical, []string{credName}, time.Time{})
	cookie := mintCookie(t, fx.signer, canonical, time.Time{})

	uiName := useridentity.NameForSubject(canonical)
	secName := useridentity.MasterSecretName(uiName, credName)
	secKey := clientpkg.ObjectKey{Namespace: spiceboxv1alpha1.IdentitiesNamespace, Name: secName}

	// A 92-byte value that is NOT an sk-ant-oat token — exactly the live bug.
	bad := url.Values{"link": {raw}, "credential": {credName}, "token": {"zkeI" + strings.Repeat("x", 88)}}
	rec := doPOST(t, fx, bad, cookie)
	require.Equal(t, http.StatusBadRequest, rec.Code, "wrong-format token must be rejected at the paste form")
	body := rec.Body.String()
	assert.Contains(t, body, "That doesn't look right", "the styled error page must explain the rejection")
	assert.Contains(t, body, "sk-ant-oat", "the rejection must surface the expected format hint")

	var sec corev1.Secret
	err := fx.c.Get(context.Background(), secKey, &sec)
	assert.True(t, apierrors.IsNotFound(err), "no Secret may be written for a rejected token; got err=%v", err)

	// A correctly-shaped token is accepted + stored.
	good := url.Values{"link": {raw}, "credential": {credName}, "token": {"sk-ant-oat01-GOODTOKEN"}}
	rec2 := doPOST(t, fx, good, cookie)
	require.Equal(t, http.StatusFound, rec2.Code, "correctly-formatted token stores + redirects")
	require.NoError(t, fx.c.Get(context.Background(), secKey, &sec), "Secret must be written for a valid token")
	assert.Equal(t, "sk-ant-oat01-GOODTOKEN", string(sec.Data["token"]))
}

// TestHandleLinkSubmit_PerCredentialSequence — the menu page is the
// driver: each PAT row's Save button submits one credential, and the
// redirect lands the user back on the menu where the next row is
// still missing. Two sequential POSTs persist both credentials.
func TestHandleLinkSubmit_PerCredentialSequence(t *testing.T) {
	const (
		ns        = "default"
		name      = "sess-1"
		canonical = "user:alice@example.com"
	)
	fx := newLinkFixture(t, makeAgentSession(ns, name, canonical))
	creds := []string{"github-pat", "linear-pat"}
	raw := mintLink(t, fx.signer, ns+"/"+name, canonical, creds, time.Time{})
	cookie := mintCookie(t, fx.signer, canonical, time.Time{})

	// Save first credential.
	rec1 := doPOST(t, fx,
		url.Values{"link": {raw}, "credential": {"github-pat"}, "token": {"gh-token"}}, cookie)
	require.Equal(t, http.StatusFound, rec1.Code, "first per-cred submit → 302")

	// Save second credential, same signed link.
	rec2 := doPOST(t, fx,
		url.Values{"link": {raw}, "credential": {"linear-pat"}, "token": {"lin-token"}}, cookie)
	require.Equal(t, http.StatusFound, rec2.Code, "second per-cred submit → 302 (signed link is reusable)")

	uiName := useridentity.NameForSubject(canonical)
	var ui spiceboxv1alpha1.UserIdentity
	require.NoError(t, fx.c.Get(context.Background(), clientpkg.ObjectKey{Name: uiName}, &ui))
	assert.Len(t, ui.Spec.Credentials, 2, "both per-cred submits persisted")
}

// TestHandleLinkSubmit_RejectsCredentialOutsidePayload — even a holder
// of both the signed link AND the cookie cannot overwrite a credential
// the session never asked for. The handler enforces "credential ∈
// payload.RequiredCredentials" so a tampered form can't side-channel
// arbitrary credential writes.
func TestHandleLinkSubmit_RejectsCredentialOutsidePayload(t *testing.T) {
	const (
		ns        = "default"
		name      = "sess-1"
		canonical = "user:alice@example.com"
	)
	fx := newLinkFixture(t, makeAgentSession(ns, name, canonical))
	// Link covers only "github-pat".
	raw := mintLink(t, fx.signer, ns+"/"+name, canonical, []string{"github-pat"}, time.Time{})
	cookie := mintCookie(t, fx.signer, canonical, time.Time{})

	rec := doPOST(t, fx,
		url.Values{"link": {raw}, "credential": {"linear-pat"}, "token": {"sneaky"}}, cookie)
	assert.Equal(t, http.StatusForbidden, rec.Code, "credential outside payload → 403")
	assert.Contains(t, rec.Body.String(), "Credential not allowed")

	uiName := useridentity.NameForSubject(canonical)
	var ui spiceboxv1alpha1.UserIdentity
	err := fx.c.Get(context.Background(), clientpkg.ObjectKey{Name: uiName}, &ui)
	assert.Error(t, err, "no UserIdentity may be created from an out-of-scope credential submit")
}

func TestHandleLinkSubmit_GateFailures(t *testing.T) {
	const (
		ns        = "default"
		name      = "sess-1"
		canonical = "user:alice@example.com"
	)
	otherCanonical := identity.Subject("user:bob@example.com")

	cases := []struct {
		name          string
		stripLink     bool // omit the hidden link field
		tamperSig     bool
		linkExp       time.Time
		cookieSubject identity.Subject
		cookieExp     time.Time
		wantStatus    int
		wantContains  string
	}{
		{
			name:          "no cookie → 403 Not your session",
			cookieSubject: "",
			wantStatus:    http.StatusForbidden,
			wantContains:  "Not your session",
		},
		{
			name:          "wrong cookie subject → 403",
			cookieSubject: otherCanonical,
			wantStatus:    http.StatusForbidden,
			wantContains:  "Not your session",
		},
		{
			name:          "tampered link → 400 Invalid link",
			tamperSig:     true,
			cookieSubject: canonical,
			wantStatus:    http.StatusBadRequest,
			wantContains:  "Invalid link",
		},
		{
			name:          "expired link → 400 Link expired",
			linkExp:       time.Now().Add(-1 * time.Hour),
			cookieSubject: canonical,
			wantStatus:    http.StatusBadRequest,
			wantContains:  "Link expired",
		},
		{
			name:          "missing link field → 400 Invalid form",
			stripLink:     true,
			cookieSubject: canonical,
			wantStatus:    http.StatusBadRequest,
			wantContains:  "Invalid form",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newLinkFixture(t, makeAgentSession(ns, name, canonical))
			raw := mintLink(t, fx.signer, ns+"/"+name, canonical, []string{"github-pat"}, tc.linkExp)
			if tc.tamperSig {
				raw = raw + "00"
			}
			form := url.Values{"credential": {"github-pat"}, "token": {"some-token"}}
			if !tc.stripLink {
				form.Set("link", raw)
			}
			var cookieVal string
			if tc.cookieSubject != "" {
				cookieVal = mintCookie(t, fx.signer, tc.cookieSubject, tc.cookieExp)
			}
			rec := doPOST(t, fx, form, cookieVal)
			assert.Equal(t, tc.wantStatus, rec.Code)
			assert.Contains(t, rec.Body.String(), tc.wantContains)

			// In every failure case, no UserIdentity must be created.
			uiName := useridentity.NameForSubject(canonical)
			var ui spiceboxv1alpha1.UserIdentity
			err := fx.c.Get(context.Background(), clientpkg.ObjectKey{Name: uiName}, &ui)
			assert.Error(t, err, "no credential must be persisted on a gate failure")
		})
	}
}

func TestHandleLinkSubmit_MethodNotAllowed(t *testing.T) {
	fx := newLinkFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/link/submit", nil)
	rec := serveWeb(t, fx, req)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	assert.Equal(t, http.MethodPost, rec.Header().Get("Allow"))
}

// TestHandleLinkSubmit_ReSubmitOverwrites — the menu-driven flow needs
// the signed link to be reusable across per-credential submits. A
// re-submit of the same credential overwrites the prior token rather
// than rejecting (idempotent UX: "I mistyped, let me try again"). The
// cookie + signature gates are the real defense against link
// forwarding; single-use enforcement on /link/submit would just
// frustrate a legitimate user mid-flow.
func TestHandleLinkSubmit_ReSubmitOverwrites(t *testing.T) {
	const (
		ns        = "default"
		name      = "sess-1"
		canonical = "user:alice@example.com"
		credName  = "github-pat"
	)
	fx := newLinkFixture(t, makeAgentSession(ns, name, canonical))
	raw := mintLink(t, fx.signer, ns+"/"+name, canonical, []string{credName}, time.Time{})
	cookie := mintCookie(t, fx.signer, canonical, time.Time{})

	form1 := url.Values{"link": {raw}, "credential": {credName}, "token": {"first"}}
	rec1 := doPOST(t, fx, form1, cookie)
	require.Equal(t, http.StatusFound, rec1.Code, "first POST → 302")

	form2 := url.Values{"link": {raw}, "credential": {credName}, "token": {"second"}}
	rec2 := doPOST(t, fx, form2, cookie)
	require.Equal(t, http.StatusFound, rec2.Code, "second POST → 302 (overwrite, not reject)")

	uiName := useridentity.NameForSubject(canonical)
	secName := useridentity.MasterSecretName(uiName, credName)
	var sec corev1.Secret
	require.NoError(t, fx.c.Get(context.Background(),
		clientpkg.ObjectKey{Namespace: spiceboxv1alpha1.IdentitiesNamespace, Name: secName}, &sec))
	assert.Equal(t, "second", string(sec.Data["token"]),
		"the resubmit must overwrite the prior token")
}

// Sanity-check the fakekind import path renders something usable for D3 tests.
var _ = fakekind.SetCanonicalForState

// TestHandleLinkSubmit_RecordsTheAttestedAccount drives the REAL link path —
// signed link, cookie gate, format gate, live probe, PutToken — and asserts
// the master Secret comes out carrying the provider account the probe
// observed. That is the precondition the UserIdentity reconciler's identity
// edge is built on (pkg/controllers/useridentity/attested_edge.go).
//
// It is written against the handler rather than against PutToken because the
// defect it exists to catch was a JOIN failure: every endpoint worked, and
// nothing connected the verification that learned the id to the write that
// stores it. A fixture that stamps the annotation itself cannot see that.
func TestHandleLinkSubmit_RecordsTheAttestedAccount(t *testing.T) {
	const (
		ns        = "default"
		name      = "sess-1"
		canonical = "user:alice@example.com"
		// Resolves to the github-pat provider through the embedded toolkit
		// catalog, so the real probe (and its subjectIDField) runs.
		credName   = "github-token"
		tokenValue = "ghp_dead"
	)
	// attestationOf reads back what the submit stored on the master Secret.
	attestationOf := func(t *testing.T, fx linkFixture) (string, string, bool) {
		t.Helper()
		secName := useridentity.MasterSecretName(useridentity.NameForSubject(canonical), credName)
		var sec corev1.Secret
		require.NoError(t, fx.c.Get(context.Background(),
			clientpkg.ObjectKey{Namespace: spiceboxv1alpha1.IdentitiesNamespace, Name: secName}, &sec),
			"the master Secret must exist after a successful submit")
		return useridentity.Attestation(&sec)
	}
	submit := func(t *testing.T, fx linkFixture, extra url.Values) *httptest.ResponseRecorder {
		t.Helper()
		raw := mintLink(t, fx.signer, ns+"/"+name, canonical, []string{credName}, time.Time{})
		cookie := mintCookie(t, fx.signer, canonical, time.Time{})
		form := url.Values{"link": {raw}, "credential": {credName}, "token": {tokenValue}}
		for k, v := range extra {
			form[k] = v
		}
		return doPOST(t, fx, form, cookie)
	}

	t.Run("a live-verified token stores the provider's stable id, not its login", func(t *testing.T) {
		// login and id deliberately differ: the edge must key on the numeric
		// id, which survives a rename, and never on the mutable login.
		stubVerify(t, http.StatusOK, `{"login":"octocat","id":583231}`)
		fx := newLinkFixture(t, makeAgentSession(ns, name, canonical))
		require.Equal(t, http.StatusFound, submit(t, fx, nil).Code)

		providerID, subjectID, ok := attestationOf(t, fx)
		require.True(t, ok, "a verified link must record both halves of the attestation")
		assert.Equal(t, "github-pat", providerID,
			"the provider recorded must be the one that ran the probe")
		assert.Equal(t, "583231", subjectID,
			"the stable numeric id, never the login")
	})

	t.Run("a token stored on the visitor's say-so attests nothing", func(t *testing.T) {
		// The provider rejected it and the visitor pressed "store anyway", so
		// no account was ever established — recording one would be a claim
		// nobody made.
		stubVerify(t, http.StatusUnauthorized, `{"message":"Bad credentials"}`)
		fx := newLinkFixture(t, makeAgentSession(ns, name, canonical))
		require.Equal(t, http.StatusFound, submit(t, fx, url.Values{"verifyConfirm": {"1"}}).Code)

		_, _, ok := attestationOf(t, fx)
		assert.False(t, ok, "an unverified store must leave no attestation behind")
	})
}

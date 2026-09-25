package identityd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientpkg "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
)

// makeAgentSessionWithKind returns an AgentSession whose inputChannel.kind
// is set, which /oidc/login reads to pick the OIDC authenticator.
func makeAgentSessionWithKind(ns, name, starterCanonical, kind string) *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Annotations: map[string]string{
				spiceboxv1alpha1.AnnotationStartedByCanonicalID: starterCanonical,
			},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:        "pass-cls",
			Prompt:       spiceboxv1alpha1.PromptSource{Inline: "do the work"},
			InputChannel: &spiceboxv1alpha1.ChannelBinding{Name: "ch", Kind: kind},
		},
	}
}

// newOIDCLoginFixture builds a Server wired with the given authenticators and
// a fake k8s client pre-seeded with objs. externalBase controls the cookie
// Secure flag. insecureTrustLinks enables the pre-IdP trust-link fallback
// (dev-only; required by tests that exercise the OIDC-off path).
func newOIDCLoginFixture(t *testing.T, externalBase string, auths map[string]channelkinds.WebAuthenticator, objs ...clientpkg.Object) *Server {
	t.Helper()
	return newOIDCLoginFixtureOpts(t, externalBase, auths, false, objs...)
}

// newOIDCLoginFixtureInsecure is like newOIDCLoginFixture but enables
// InsecureTrustLinks so the OIDC-off trust-link path is active.
func newOIDCLoginFixtureInsecure(t *testing.T, externalBase string, auths map[string]channelkinds.WebAuthenticator, objs ...clientpkg.Object) *Server {
	t.Helper()
	return newOIDCLoginFixtureOpts(t, externalBase, auths, true, objs...)
}

func newOIDCLoginFixtureOpts(t *testing.T, externalBase string, auths map[string]channelkinds.WebAuthenticator, insecureTrustLinks bool, objs ...clientpkg.Object) *Server {
	t.Helper()
	scheme := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return NewServer(Deps{
		K8s:                c,
		LinkSigner:         passthroughlink.New(signerKey),
		ExternalBaseURL:    func() string { return externalBase },
		Authenticators:     auths,
		InsecureTrustLinks: insecureTrustLinks,
	})
}

// newOIDCLoginFixtureProdSigner builds a Server whose LinkSigner carries the
// SAME iss/aud identityd's own signer carries in production
// (internal/cmd/webd/main.go's cookieSigner). The other fixtures here leave both empty,
// which makes a minted cookie fail checkOIDCCookie's issuer pin for reasons
// that have nothing to do with the code under test — no good for a test whose
// subject is whether a minted cookie authenticates.
func newOIDCLoginFixtureProdSigner(t *testing.T, auths map[string]channelkinds.WebAuthenticator, objs ...clientpkg.Object) *Server {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(objs...).Build()
	return NewServer(Deps{
		K8s: c,
		LinkSigner: passthroughlink.New(signerKey,
			passthroughlink.WithIssuer(passthroughlink.IssuerIdentityd),
			passthroughlink.WithAudience(passthroughlink.AudienceIdentityd)),
		ExternalBaseURL: func() string { return "https://identityd.example.org" },
		Authenticators:  auths,
	})
}

// doOIDCLogin fires GET /oidc/login with the given query params.
func doOIDCLogin(t *testing.T, srv *Server, d, sig, next string) *httptest.ResponseRecorder {
	t.Helper()
	q := url.Values{}
	if d != "" {
		q.Set("d", d)
	}
	if sig != "" {
		q.Set("sig", sig)
	}
	if next != "" {
		q.Set("next", next)
	}
	rawURL := "/oidc/login"
	if len(q) > 0 {
		rawURL += "?" + q.Encode()
	}
	req := httptest.NewRequest(http.MethodGet, rawURL, nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// beginRecordingAuth captures the state it was started with so the test can
// assert the state store carried `next`. Its Begin URL is deterministic.
type beginRecordingAuth struct{ lastState string }

func (a *beginRecordingAuth) Begin(_ context.Context, state string) (string, error) {
	a.lastState = state
	return "https://idp.example/authorize?state=" + url.QueryEscape(state), nil
}
func (a *beginRecordingAuth) Complete(_ context.Context, _ channelkinds.CallbackParams) (string, error) {
	return "user:alice@example.com", nil
}

func TestHandleOIDCLogin(t *testing.T) {
	const (
		ns       = "default"
		name     = "s1"
		canon    = "user:alice@example.com"
		sessRef  = ns + "/" + name
		nextDest = "/artifacts/v/abc" // safe relative path — absolute URLs rejected by safeNext
	)

	t.Run("valid link + known kind → 302 to Begin URL; state carries next", func(t *testing.T) {
		auth := &beginRecordingAuth{}
		srv := newOIDCLoginFixture(t, "https://identityd.example.org",
			map[string]channelkinds.WebAuthenticator{"slack": auth},
			makeAgentSessionWithKind(ns, name, canon, "slack"))

		raw := mintLink(t, srv.deps.LinkSigner, sessRef, canon, []string{"cred"}, time.Time{})
		d, sig, ok := splitSignedLink(raw)
		require.True(t, ok)

		rec := doOIDCLogin(t, srv, d, sig, nextDest)
		require.Equal(t, http.StatusFound, rec.Code)
		loc := rec.Header().Get("Location")
		assert.True(t, strings.HasPrefix(loc, "https://idp.example/authorize"), "must redirect to the authenticator's Begin URL, got %q", loc)

		// The state the authenticator was started with must resolve, in the
		// store, to the `next` destination so the callback can use it.
		require.NotEmpty(t, auth.lastState, "Begin must have been called with a state token")
		bindingCookie := findCookie(rec, loginBindingCookie)
		require.NotNil(t, bindingCookie, "starting a login must bind the flow to this browser")
		_, gotNext, refusal := srv.stateStore.Consume(auth.lastState, bindingCookie.Value, "slack")
		require.Equal(t, stateAccepted, refusal, "state token must be present in the store")
		assert.Equal(t, nextDest, gotNext, "stored state must carry next")
	})

	t.Run("OIDC-off kind (absent from Authenticators) + InsecureTrustLinks → trust-link cookie + 302 to next", func(t *testing.T) {
		// No authenticators at all + InsecureTrustLinks → trust the link subject.
		srv := newOIDCLoginFixtureInsecure(t, "https://identityd.example.org",
			map[string]channelkinds.WebAuthenticator{},
			makeAgentSessionWithKind(ns, name, canon, "slack"))

		raw := mintLink(t, srv.deps.LinkSigner, sessRef, canon, []string{"cred"}, time.Time{})
		d, sig, ok := splitSignedLink(raw)
		require.True(t, ok)

		rec := doOIDCLogin(t, srv, d, sig, nextDest)
		require.Equal(t, http.StatusFound, rec.Code)
		assert.Equal(t, nextDest, rec.Header().Get("Location"), "OIDC-off path must redirect straight to next")

		c := findCookie(rec, cookieName)
		require.NotNil(t, c, "trust-link cookie must be set")
		assert.True(t, c.HttpOnly, "cookie must be HttpOnly")
		assert.Greater(t, c.MaxAge, 0, "cookie must have a positive MaxAge")
	})

	t.Run("OIDC-off kind with empty next + InsecureTrustLinks → 302 to reconstructed /link", func(t *testing.T) {
		srv := newOIDCLoginFixtureInsecure(t, "https://identityd.example.org",
			map[string]channelkinds.WebAuthenticator{},
			makeAgentSessionWithKind(ns, name, canon, "slack"))

		raw := mintLink(t, srv.deps.LinkSigner, sessRef, canon, []string{"cred"}, time.Time{})
		d, sig, ok := splitSignedLink(raw)
		require.True(t, ok)

		rec := doOIDCLogin(t, srv, d, sig, "")
		require.Equal(t, http.StatusFound, rec.Code)
		loc := rec.Header().Get("Location")
		assert.True(t, strings.HasPrefix(loc, "/link?"), "empty next falls back to /link, got %q", loc)
		assert.Contains(t, loc, "d=")
		assert.Contains(t, loc, "sig=")
	})

	t.Run("OIDC-off kind (absent from Authenticators) without InsecureTrustLinks → 403", func(t *testing.T) {
		// No authenticators + InsecureTrustLinks=false → fail closed with 403.
		srv := newOIDCLoginFixture(t, "https://identityd.example.org",
			map[string]channelkinds.WebAuthenticator{},
			makeAgentSessionWithKind(ns, name, canon, "slack"))

		raw := mintLink(t, srv.deps.LinkSigner, sessRef, canon, []string{"cred"}, time.Time{})
		d, sig, ok := splitSignedLink(raw)
		require.True(t, ok)

		rec := doOIDCLogin(t, srv, d, sig, nextDest)
		assert.Equal(t, http.StatusForbidden, rec.Code)
		assert.Contains(t, rec.Body.String(), "Sign-in unavailable")
	})

	t.Run("bad link (bad signature) → 403", func(t *testing.T) {
		srv := newOIDCLoginFixture(t, "https://identityd.example.org",
			map[string]channelkinds.WebAuthenticator{"slack": &beginRecordingAuth{}},
			makeAgentSessionWithKind(ns, name, canon, "slack"))

		rec := doOIDCLogin(t, srv, "tampered", "deadbeef", nextDest)
		assert.Equal(t, http.StatusForbidden, rec.Code)
		assert.Contains(t, rec.Body.String(), "Invalid link")
	})

	t.Run("missing d/sig → 403", func(t *testing.T) {
		srv := newOIDCLoginFixture(t, "https://identityd.example.org",
			map[string]channelkinds.WebAuthenticator{"slack": &beginRecordingAuth{}})
		rec := doOIDCLogin(t, srv, "", "", nextDest)
		assert.Equal(t, http.StatusForbidden, rec.Code)
	})

	t.Run("valid link but session not found → 500 Sign-in unavailable", func(t *testing.T) {
		// No AgentSession seeded → kind resolution Get fails.
		srv := newOIDCLoginFixture(t, "https://identityd.example.org",
			map[string]channelkinds.WebAuthenticator{"slack": &beginRecordingAuth{}})

		raw := mintLink(t, srv.deps.LinkSigner, sessRef, canon, []string{"cred"}, time.Time{})
		d, sig, ok := splitSignedLink(raw)
		require.True(t, ok)

		rec := doOIDCLogin(t, srv, d, sig, nextDest)
		assert.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.Contains(t, rec.Body.String(), "Sign-in unavailable")
	})

	t.Run("does not enforce subject-match: link subject ≠ session starter still starts OIDC", func(t *testing.T) {
		auth := &beginRecordingAuth{}
		srv := newOIDCLoginFixture(t, "https://identityd.example.org",
			map[string]channelkinds.WebAuthenticator{"slack": auth},
			// session starter is a DIFFERENT canonical than the link subject.
			makeAgentSessionWithKind(ns, name, "user:bob@example.com", "slack"))

		raw := mintLink(t, srv.deps.LinkSigner, sessRef, canon, []string{"cred"}, time.Time{})
		d, sig, ok := splitSignedLink(raw)
		require.True(t, ok)

		rec := doOIDCLogin(t, srv, d, sig, nextDest)
		assert.Equal(t, http.StatusFound, rec.Code, "/oidc/login must NOT gate on subject==starter")
		assert.True(t, strings.HasPrefix(rec.Header().Get("Location"), "https://idp.example/authorize"))
	})

	t.Run("method not allowed: POST → 405", func(t *testing.T) {
		srv := newOIDCLoginFixture(t, "https://identityd.example.org",
			map[string]channelkinds.WebAuthenticator{"slack": &beginRecordingAuth{}})
		req := httptest.NewRequest(http.MethodPost, "/oidc/login", nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	})

	t.Run("portal link with no CR and no authenticator → direct cookie, cookieTTL MaxAge (600)", func(t *testing.T) {
		// No CR, no authenticator, flag OFF: a session-bootstrap link must
		// STILL bootstrap. The strongest form of the claim — it cannot depend
		// on any other configured capability.
		srv := newOIDCLoginFixture(t, "https://identityd.example.org",
			map[string]channelkinds.WebAuthenticator{},
			makeAgentSessionWithKind(ns, name, canon, "slack"))

		raw := mintPortalLink(t, srv.deps.LinkSigner, canon, time.Time{})
		d, sig, ok := splitSignedLink(raw)
		require.True(t, ok)

		rec := doOIDCLogin(t, srv, d, sig, "/artifacts/v/abc")
		require.Equal(t, http.StatusFound, rec.Code)

		// Must redirect to next (safe relative path), NOT to /authorize.
		loc := rec.Header().Get("Location")
		assert.Equal(t, "/artifacts/v/abc", loc, "bootstrap → redirect to next, got %q", loc)
		assert.False(t, strings.HasPrefix(loc, "https://idp.example"), "must NOT redirect to IdP authorize URL")

		// Cookie must be set with cookieTTL = 600s MaxAge.
		c := findCookie(rec, cookieName)
		require.NotNil(t, c, "portal link must set idd_session cookie")
		assert.Equal(t, 600, c.MaxAge, "link-bootstrapped cookie uses cookieTTL (600s), not sessionTTL")
		assert.True(t, c.HttpOnly, "cookie must be HttpOnly")
	})

	t.Run("portal link bootstraps even when IdP configured: no Begin, no authorize redirect", func(t *testing.T) {
		// Even with a ClusterIdentityProvider configured (and an auth registered),
		// a session-bootstrap link goes straight to the cookie without starting OIDC.
		auth := &beginRecordingAuth{}
		srv := newOIDCLoginFixtureWithIdP(t, "https://identityd.example.org",
			map[string]channelkinds.WebAuthenticator{"slack": auth},
			makeAgentSessionWithKind(ns, name, canon, "slack"))

		raw := mintPortalLink(t, srv.deps.LinkSigner, canon, time.Time{})
		d, sig, ok := splitSignedLink(raw)
		require.True(t, ok)

		rec := doOIDCLogin(t, srv, d, sig, "/my/accounts")
		require.Equal(t, http.StatusFound, rec.Code)

		loc := rec.Header().Get("Location")
		assert.Equal(t, "/my/accounts", loc, "bootstrap must go to next, got %q", loc)
		assert.False(t, strings.HasPrefix(loc, "https://idp.example"), "must NOT redirect to IdP authorize URL")
		// The authenticator's Begin must NOT have been called.
		assert.Empty(t, auth.lastState, "Begin must NOT be invoked for a session-bootstrap link")

		c := findCookie(rec, cookieName)
		require.NotNil(t, c, "portal link must set idd_session cookie")
		assert.Equal(t, 600, c.MaxAge, "link-bootstrapped cookie uses cookieTTL (600s)")
	})

	t.Run("artifact_view link with IdP configured → real OIDC begin, no cookie", func(t *testing.T) {
		// The other side of the same policy: a link identityd will not trade
		// for a session is not refused outright — it is routed through the
		// real login chain, which here is the cluster IdP.
		srv := newOIDCLoginFixtureWithIdP(t, "https://identityd.example.org",
			map[string]channelkinds.WebAuthenticator{},
			makeAgentSessionWithKind(ns, name, canon, "slack"))

		raw, err := srv.deps.LinkSigner.Mint(passthroughlink.Payload{
			Issuer:          passthroughlink.IssuerChannelsd,
			Audience:        passthroughlink.AudienceWebd,
			Purpose:         passthroughlink.PurposeArtifactView,
			ArtifactID:      "art-1",
			SessionRef:      sessRef,
			Subject:         canon,
			SubjectVerified: true,
			ExpiresAt:       time.Now().Add(5 * time.Minute).Unix(),
		})
		require.NoError(t, err)
		d, sig, ok := splitSignedLink(raw)
		require.True(t, ok)

		rec := doOIDCLogin(t, srv, d, sig, "/artifact-view?d=x&sig=y")
		require.Equal(t, http.StatusFound, rec.Code, "body=%s", rec.Body.String())
		// The fake kind's Begin returns "<issuer>/authorize?state=<state>", and
		// defaultCR leaves Issuer empty.
		assert.Contains(t, rec.Header().Get("Location"), "authorize?state=",
			"must start the IdP flow, got %q", rec.Header().Get("Location"))
		assert.Nil(t, findCookie(rec, cookieName), "no session until the IdP proves who this is")
	})
}

// TestOIDCLoginRefusesSessionForNarrowLinks pins the security property that
// /oidc/login must NOT exchange a narrow, shareable, or foreign-audience link
// for an idd_session. The cookie identityd mints is a GENERAL authentication
// credential — Path=/, accepted by every cookie-gated route on this listener
// and by webd's framework-level authenticate (internal/cmd/webd/main.go), /admin
// included — so a link whose own grant is narrower than that must never mint
// one. Each case is a link shape a production minter really produces.
func TestOIDCLoginRefusesSessionForNarrowLinks(t *testing.T) {
	const (
		ns      = "default"
		name    = "s1"
		victim  = identity.Subject("user:alice@example.com")
		sessRef = ns + "/" + name
	)
	exp := time.Now().Add(5 * time.Minute).Unix()

	cases := []struct {
		name    string
		payload passthroughlink.Payload
	}{
		{
			name: "artifact_view link (aud=webd, shareable 'here is the live view') → no session cookie",
			payload: passthroughlink.Payload{
				Issuer:          passthroughlink.IssuerChannelsd,
				Audience:        passthroughlink.AudienceWebd,
				Purpose:         passthroughlink.PurposeArtifactView,
				ArtifactID:      "art-1",
				SessionRef:      sessRef,
				Subject:         victim,
				SubjectVerified: true,
				ExpiresAt:       exp,
			},
		},
		{
			name: "session_view link (7-day shareable transcript view) → no session cookie",
			payload: passthroughlink.Payload{
				Issuer:          passthroughlink.IssuerChannelsd,
				Audience:        passthroughlink.AudienceWebd,
				Purpose:         passthroughlink.PurposeSessionView,
				SessionRef:      sessRef,
				Subject:         victim,
				SubjectVerified: true,
				ExpiresAt:       exp,
			},
		},
		{
			name: "cli_identity assertion (aud=cli-identity, persisted to disk by oap login) → no session cookie",
			payload: passthroughlink.Payload{
				Issuer:          passthroughlink.IssuerIdentityd,
				Audience:        passthroughlink.AudienceCLIIdentity,
				Purpose:         passthroughlink.PurposeCLIIdentity,
				Subject:         victim,
				SubjectVerified: true,
				ExpiresAt:       exp,
			},
		},
		{
			name: "credential-request deep-link (empty purpose) → no session cookie",
			payload: passthroughlink.Payload{
				Issuer:              passthroughlink.IssuerChannelsd,
				Audience:            passthroughlink.AudienceIdentityd,
				SessionRef:          sessRef,
				Subject:             victim,
				RequiredCredentials: []string{"cred"},
				SubjectVerified:     true,
				ExpiresAt:           exp,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// No IdP, no authenticator, InsecureTrustLinks OFF: nothing here
			// may authenticate the visitor, so any cookie that comes back came
			// from the link itself.
			srv := newOIDCLoginFixtureProdSigner(t,
				map[string]channelkinds.WebAuthenticator{},
				makeAgentSessionWithKind(ns, name, victim.String(), "slack"))

			raw, err := srv.deps.LinkSigner.Mint(tc.payload)
			require.NoError(t, err)
			d, sig, ok := splitSignedLink(raw)
			require.True(t, ok)

			rec := doOIDCLogin(t, srv, d, sig, "/my/accounts")

			c := findCookie(rec, cookieName)
			if !assert.Nil(t, c, "this link must not mint an idd_session; only a real login may") {
				// Spell out the consequence so a regression reads as the
				// escalation it is: checkOIDCCookie is the same verification
				// (iss=identityd, aud=identityd, non-empty Subject) that webd's
				// authenticate performs for every AuthAuthenticated route.
				req := httptest.NewRequest(http.MethodGet, "/my/accounts", nil)
				req.AddCookie(&http.Cookie{Name: cookieName, Value: c.Value})
				got, authed := srv.checkOIDCCookie(req)
				assert.False(t, authed,
					"the minted cookie authenticates the link's bearer as %s on every cookie-gated route, /admin included", got)
			}
		})
	}
}

// TestOIDCLoginPortalLinkIsSingleUse pins the other half: the portal purpose —
// the one link kind whose whole grant IS "act as this subject on the identity
// surface" — may still bootstrap a session at /oidc/login, but only once.
// /my/accounts has enforced that since it was written (handlers_portal.go);
// aiming the same link at /oidc/login must not be a way around it.
func TestOIDCLoginPortalLinkIsSingleUse(t *testing.T) {
	const victim = identity.Subject("user:alice@example.com")
	srv := newOIDCLoginFixtureProdSigner(t, map[string]channelkinds.WebAuthenticator{})

	raw := mintPortalLink(t, srv.deps.LinkSigner, victim, time.Time{})
	d, sig, ok := splitSignedLink(raw)
	require.True(t, ok)

	rec1 := doOIDCLogin(t, srv, d, sig, "/my/accounts")
	require.Equal(t, http.StatusFound, rec1.Code, "first use bootstraps the session; body=%s", rec1.Body.String())
	c := findCookie(rec1, cookieName)
	require.NotNil(t, c, "portal link must mint the idd_session cookie")
	assert.Equal(t, 600, c.MaxAge, "link-bootstrapped cookie uses cookieTTL (600s), not the 12h IdP sessionTTL")

	rec2 := doOIDCLogin(t, srv, d, sig, "/my/accounts")
	assert.Equal(t, http.StatusBadRequest, rec2.Code,
		"replaying a consumed portal link must be refused; body=%s", rec2.Body.String())
	assert.Nil(t, findCookie(rec2, cookieName), "a consumed link must not mint a second session")
}

// mintVerifiedLink mints a SubjectVerified=true deep-link for the given
// sessRef and subject. Used by tests asserting the SubjectVerified shortcut.
func mintVerifiedLink(t *testing.T, sgn *passthroughlink.Signer, sessRef string, subject identity.Subject) string {
	t.Helper()
	raw, err := sgn.Mint(passthroughlink.Payload{
		Issuer:          passthroughlink.IssuerChannelsd,
		Audience:        passthroughlink.AudienceIdentityd,
		SessionRef:      sessRef,
		Subject:         subject,
		ExpiresAt:       time.Now().Add(5 * time.Minute).Unix(),
		SubjectVerified: true,
	})
	require.NoError(t, err)
	return raw
}

// TestSafeNext exercises the safeNext helper directly.
func TestSafeNext(t *testing.T) {
	cases := []struct {
		name string
		next string
		want bool
	}{
		{name: "simple relative path", next: "/x", want: true},
		{name: "relative path with query", next: "/a?b=c", want: true},
		{name: "empty string not honored (falls back)", next: "", want: false},
		{name: "protocol-relative double-slash rejected", next: "//evil.example", want: false},
		{name: "absolute https URL rejected", next: "https://evil.example", want: false},
		{name: "backslash escape rejected", next: "/\\evil", want: false},
		{name: "absolute http URL rejected", next: "http://evil.example/path", want: false},
		{name: "no leading slash rejected", next: "relative/path", want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := safeNext(tc.next)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestSafeNext_EndToEnd_MaliciousNextDropped asserts that a malicious absolute
// next supplied to /oidc/login is NOT followed — the 302 Location must be
// the reconstructed /link?d=...&sig=... fallback, not the attacker URL.
func TestSafeNext_EndToEnd_MaliciousNextDropped(t *testing.T) {
	const (
		ns      = "default"
		name    = "s1"
		canon   = "user:alice@example.com"
		sessRef = ns + "/" + name
	)
	srv := newOIDCLoginFixtureInsecure(t, "https://identityd.example.org",
		map[string]channelkinds.WebAuthenticator{},
		makeAgentSessionWithKind(ns, name, canon, "slack"))

	raw := mintVerifiedLink(t, srv.deps.LinkSigner, sessRef, canon)
	d, sig, ok := splitSignedLink(raw)
	require.True(t, ok)

	maliciousNext := "https://attacker.example/steal-tokens"
	rec := doOIDCLogin(t, srv, d, sig, maliciousNext)
	require.Equal(t, http.StatusFound, rec.Code)

	loc := rec.Header().Get("Location")
	assert.NotEqual(t, maliciousNext, loc, "malicious next must NOT be followed")
	assert.False(t, strings.HasPrefix(loc, "https://attacker.example"), "must not redirect to attacker URL")
	// Must fall back to /link reconstruction.
	assert.True(t, strings.HasPrefix(loc, "/link?"), "invalid next must fall back to /link, got %q", loc)
}

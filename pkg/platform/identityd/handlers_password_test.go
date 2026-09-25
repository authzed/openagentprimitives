package identityd

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/idp/fakekind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp/passwordkind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
)

// testPasswordPlain is the plaintext password backing every password-kind
// fixture in this file.
const testPasswordPlain = "correct horse battery staple"

// passwordCR builds a minimal, Valid=True ClusterIdentityProvider CR
// configured for kind=password, with clientID (the local admin identifier)
// pinned to a known value for assertions.
func passwordCR() *spiceboxv1alpha1.ClusterIdentityProvider {
	return &spiceboxv1alpha1.ClusterIdentityProvider{
		ObjectMeta: metav1.ObjectMeta{
			Name:       spiceboxv1alpha1.ClusterIdentityProviderName,
			Generation: 1,
		},
		Spec: spiceboxv1alpha1.ClusterIdentityProviderSpec{
			Kind:     "password",
			ClientID: "admin@ap.local",
			ClientSecretRef: spiceboxv1alpha1.ClusterSecretKeyRef{
				Namespace: "default",
				Name:      "idp-password",
				Key:       "client_secret",
			},
			AllowAnyEmail: true,
		},
		Status: spiceboxv1alpha1.ClusterIdentityProviderStatus{
			Conditions: []metav1.Condition{
				{
					Type:   spiceboxv1alpha1.ConditionIdPValid,
					Status: metav1.ConditionTrue,
					Reason: spiceboxv1alpha1.ReasonIdPReady,
				},
			},
		},
	}
}

// passwordSecret returns the Secret passwordCR references, holding a
// bcrypt hash of testPasswordPlain.
func passwordSecret(t *testing.T) *corev1.Secret {
	t.Helper()
	hash, err := passwordkind.HashPassword(testPasswordPlain)
	require.NoError(t, err)
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       "default",
			Name:            "idp-password",
			ResourceVersion: "1",
		},
		Data: map[string][]byte{"client_secret": []byte(hash)},
	}
}

// newPasswordFixture builds a Server wired with the password-kind CR +
// Secret above and no channel-kind authenticators.
func newPasswordFixture(t *testing.T) *Server {
	t.Helper()
	scheme := idpLoaderScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(passwordCR(), passwordSecret(t)).Build()
	return NewServer(Deps{
		K8s:             c,
		LinkSigner:      passthroughlink.New(signerKey),
		ExternalBaseURL: func() string { return "https://identityd.example.org" },
		Authenticators:  map[string]channelkinds.WebAuthenticator{},
	})
}

// primePasswordStateStore mints a state token bound to a placeholder
// admin-login-shaped linkRaw + next, mirroring what beginIdPLogin does in
// production (NewStateWithNext(raw, next)) before redirecting into the
// password kind's Begin().
func primePasswordStateStore(t *testing.T, srv *Server, next string) string {
	t.Helper()
	raw, err := srv.deps.LinkSigner.Mint(passthroughlink.Payload{
		Purpose:   passthroughlink.PurposeAdminLogin,
		ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
	})
	require.NoError(t, err)
	tok, err := srv.stateStore.NewStateWithNext(raw, next, testLoginBinding, "idp")
	require.NoError(t, err)
	return tok
}

func doPasswordLogin(srv *Server, state string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/password/login?state="+url.QueryEscape(state), nil)
	rec := httptest.NewRecorder()
	srv.handlePasswordLogin(rec, req)
	return rec
}

// doPasswordVerify posts the form as the browser that STARTED the flow: the
// binding cookie is what proves that, and primePasswordStateStore mints its
// state against testLoginBinding to match.
func doPasswordVerify(srv *Server, state, password string) *httptest.ResponseRecorder {
	form := url.Values{"state": {state}, "password": {password}}
	req := httptest.NewRequest(http.MethodPost, "/password/verify", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: loginBindingCookie, Value: testLoginBinding})
	rec := httptest.NewRecorder()
	srv.handlePasswordVerify(rec, req)
	return rec
}

// doPasswordVerifyAs posts the form as a browser holding an ARBITRARY binding —
// for tests that drove the real begin handler (which mints a random one) or
// that are following a retry form, whose state was minted against a fresh
// binding of its own.
func doPasswordVerifyAs(srv *Server, state, password, binding string) *httptest.ResponseRecorder {
	form := url.Values{"state": {state}, "password": {password}}
	req := httptest.NewRequest(http.MethodPost, "/password/verify", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if binding != "" {
		req.AddCookie(&http.Cookie{Name: loginBindingCookie, Value: binding})
	}
	rec := httptest.NewRecorder()
	srv.handlePasswordVerify(rec, req)
	return rec
}

// extractHiddenStateValue pulls the value of the form's hidden "state"
// input out of a rendered password-login page body. Fails the test if not
// found — every rendering of the form must carry this field.
func extractHiddenStateValue(t *testing.T, body string) string {
	t.Helper()
	const marker = `name="state" value="`
	i := strings.Index(body, marker)
	require.GreaterOrEqual(t, i, 0, "rendered form must contain a hidden state field")
	rest := body[i+len(marker):]
	j := strings.Index(rest, `"`)
	require.GreaterOrEqual(t, j, 0, "hidden state field value must be quoted")
	return rest[:j]
}

func TestHandlePasswordLogin_ValidState_RendersForm(t *testing.T) {
	srv := newPasswordFixture(t)
	stateTok := primePasswordStateStore(t, srv, "/admin")

	rec := doPasswordLogin(srv, stateTok)
	assert.Contains(t, rec.Body.String(), `<link rel="icon" type="image/svg+xml" href="data:image/svg+xml,`,
		"sign-in page carries the OAP favicon like every framework document")

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `action="/password/verify"`)
	assert.Contains(t, body, `type="password"`)
	assert.Equal(t, stateTok, extractHiddenStateValue(t, body), "GET must not consume the state token")

	// Dark mode must not leave the form unreadable (white-on-white): the page
	// declares color-scheme: light dark, so the browser flips text color in OS
	// dark mode — the stylesheet must supply a matching dark background/card/
	// input theme via prefers-color-scheme, not just the light-mode defaults.
	assert.Contains(t, body, "prefers-color-scheme: dark", "must ship a dark-mode theme")
	assert.Contains(t, body, "color-scheme: light dark", "must opt into browser dark-mode form controls")

	// GET must NOT consume the token — it must still be usable afterwards.
	assert.True(t, srv.stateStore.Valid(stateTok), "GET /password/login must not consume the state token")
}

func TestHandlePasswordLogin_MissingState_Errors(t *testing.T) {
	srv := newPasswordFixture(t)
	rec := doPasswordLogin(srv, "")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "Sign-in session expired")
}

func TestHandlePasswordLogin_InvalidState_Errors(t *testing.T) {
	srv := newPasswordFixture(t)
	rec := doPasswordLogin(srv, "this-state-token-was-never-minted")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "Sign-in session expired")
}

func TestHandlePasswordVerify_CorrectPassword_SetsCookieAndRedirects(t *testing.T) {
	srv := newPasswordFixture(t)
	stateTok := primePasswordStateStore(t, srv, "/admin")

	rec := doPasswordVerify(srv, stateTok, testPasswordPlain)

	require.Equal(t, http.StatusFound, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, "/admin", rec.Header().Get("Location"))

	c := findCookie(rec, cookieName)
	require.NotNil(t, c, "idd_session cookie must be set on correct password")
	assert.True(t, c.HttpOnly)
	assert.Equal(t, http.SameSiteLaxMode, c.SameSite)
	assert.Equal(t, int(defaultIdPSessionTTL/time.Second), c.MaxAge)

	payload, err := srv.deps.LinkSigner.Verify(c.Value)
	require.NoError(t, err)
	assert.Equal(t, identity.Subject("user:YWRtaW5AYXAubG9jYWw"), payload.Subject,
		"cookie Subject must be the stable local-admin canonical (base64url(\"admin@ap.local\"))")
}

func TestHandlePasswordVerify_WrongPassword_NoCookieAndReRendersForm(t *testing.T) {
	orig := passwordWrongAttemptDelay
	passwordWrongAttemptDelay = time.Millisecond // keep the test fast
	t.Cleanup(func() { passwordWrongAttemptDelay = orig })

	srv := newPasswordFixture(t)
	stateTok := primePasswordStateStore(t, srv, "/admin")

	rec := doPasswordVerify(srv, stateTok, "definitely-the-wrong-password")

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Nil(t, findCookie(rec, cookieName), "wrong password must NOT set idd_session")
	body := rec.Body.String()
	assert.Contains(t, body, "Incorrect password")

	// The original state was single-use and is now consumed...
	assert.False(t, srv.stateStore.Valid(stateTok), "the submitted state token must be consumed")
	// ...but the re-rendered form carries a FRESH, still-valid token so the
	// user can retry without navigating back to the beginning.
	retryTok := extractHiddenStateValue(t, body)
	assert.NotEqual(t, stateTok, retryTok, "retry form must carry a newly-minted state token")
	assert.True(t, srv.stateStore.Valid(retryTok), "retry state token must be valid")

	// The retry token works for a subsequent correct-password submission.
	// The retry response also re-bound the browser: a fresh state carries a
	// fresh binding, and the form the user is looking at got the cookie for it.
	rec2 := doPasswordVerifyAs(srv, retryTok, testPasswordPlain, bindingValue(t, rec))
	assert.Equal(t, http.StatusFound, rec2.Code, "retry token must support a follow-up correct attempt")
}

func TestHandlePasswordVerify_AppliesFixedDelayOnFailure(t *testing.T) {
	orig := passwordWrongAttemptDelay
	passwordWrongAttemptDelay = 50 * time.Millisecond
	t.Cleanup(func() { passwordWrongAttemptDelay = orig })

	srv := newPasswordFixture(t)
	stateTok := primePasswordStateStore(t, srv, "/admin")

	start := time.Now()
	doPasswordVerify(srv, stateTok, "wrong")
	assert.GreaterOrEqual(t, time.Since(start), 50*time.Millisecond,
		"a failed verify must incur the configured brute-force delay")
}

func TestHandlePasswordVerify_MissingState_Errors(t *testing.T) {
	srv := newPasswordFixture(t)
	rec := doPasswordVerify(srv, "", testPasswordPlain)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Nil(t, findCookie(rec, cookieName))
}

func TestHandlePasswordVerify_UnknownState_Errors(t *testing.T) {
	srv := newPasswordFixture(t)
	rec := doPasswordVerify(srv, "never-minted-token", testPasswordPlain)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "Sign-in session expired")
	assert.Nil(t, findCookie(rec, cookieName))
}

func TestHandlePasswordVerify_ReplayedState_Rejected(t *testing.T) {
	srv := newPasswordFixture(t)
	stateTok := primePasswordStateStore(t, srv, "/admin")

	rec1 := doPasswordVerify(srv, stateTok, testPasswordPlain)
	require.Equal(t, http.StatusFound, rec1.Code, "first submission must succeed")

	rec2 := doPasswordVerify(srv, stateTok, testPasswordPlain)
	assert.Equal(t, http.StatusBadRequest, rec2.Code, "a replayed (already-consumed) state must be rejected")
	assert.Contains(t, rec2.Body.String(), "Sign-in session expired")
	assert.Nil(t, findCookie(rec2, cookieName))
}

// TestHandlePasswordVerify_NonPasswordIdP_Errors asserts that when the
// current cluster IdP is configured with some OTHER kind (not password),
// /password/verify fails closed with a clear 500 rather than silently
// falling back to some other verification path.
func TestHandlePasswordVerify_NonPasswordIdP_Errors(t *testing.T) {
	scheme := idpLoaderScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(defaultCR("fake"), defaultSecret()).Build()
	srv := NewServer(Deps{
		K8s:             c,
		LinkSigner:      passthroughlink.New(signerKey),
		ExternalBaseURL: func() string { return "https://identityd.example.org" },
		Authenticators:  map[string]channelkinds.WebAuthenticator{},
	})
	stateTok := primePasswordStateStore(t, srv, "/admin")

	rec := doPasswordVerify(srv, stateTok, testPasswordPlain)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Contains(t, rec.Body.String(), "Sign-in unavailable")
	assert.Nil(t, findCookie(rec, cookieName))
}

// TestPasswordLoginRoundTrip_ThroughBeginIdPLogin exercises the full
// production path: a session-less admin-login /oidc/login hit resolves the
// password-kind cluster IdP via beginIdPLogin, redirects to
// /password/login?state=..., and submitting the correct password there
// reaches the same idd_session cookie handleIdPCallback would mint for any
// other kind.
func TestPasswordLoginRoundTrip_ThroughBeginIdPLogin(t *testing.T) {
	srv := newPasswordFixture(t)

	raw, err := srv.deps.LinkSigner.Mint(passthroughlink.Payload{
		Purpose:   passthroughlink.PurposeAdminLogin,
		ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
	})
	require.NoError(t, err)
	d, sig, ok := splitSignedLink(raw)
	require.True(t, ok)

	loginReq := httptest.NewRequest(http.MethodGet,
		"/oidc/login?d="+url.QueryEscape(d)+"&sig="+url.QueryEscape(sig)+"&next=%2Fadmin", nil)
	loginRec := httptest.NewRecorder()
	srv.handleOIDCLogin(loginRec, loginReq)
	require.Equal(t, http.StatusFound, loginRec.Code)

	loc := loginRec.Header().Get("Location")
	require.True(t, strings.HasPrefix(loc, "/password/login?state="),
		"admin login against a password-kind IdP must redirect to /password/login, got %q", loc)

	u, err := url.Parse(loc)
	require.NoError(t, err)
	stateTok := u.Query().Get("state")
	require.NotEmpty(t, stateTok)

	formRec := doPasswordLogin(srv, stateTok)
	require.Equal(t, http.StatusOK, formRec.Code)

	verifyRec := doPasswordVerifyAs(srv, stateTok, testPasswordPlain, bindingValue(t, loginRec))
	require.Equal(t, http.StatusFound, verifyRec.Code, "body: %s", verifyRec.Body.String())
	assert.Equal(t, "/admin", verifyRec.Header().Get("Location"))
	require.NotNil(t, findCookie(verifyRec, cookieName))
}

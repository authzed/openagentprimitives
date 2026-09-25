// pkg/platform/identityd/handlers_portal_test.go — coverage for the standing
// portal surface added in Slice 2.5 α3. Reuses the linkFixture builder
// + mintCookie helper from handlers_link_test.go.
package identityd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientpkg "sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	// Registers the static/oauth/federated credkind.Kinds so
	// portalCredentialsFromUserIdentity's credkindregistry.Get(cred.Type)
	// dispatch resolves in this package's tests (used by the Linked-list
	// NeedsRefresh projection tested below).
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/imports"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
)

// mintPortalLink mints a portal-purpose signed link the way channelsd
// will when publishing the "manage my accounts" envelope.
func mintPortalLink(t *testing.T, sgn *passthroughlink.Signer, subject identity.Subject, exp time.Time) string {
	t.Helper()
	if exp.IsZero() {
		exp = time.Now().Add(5 * time.Minute)
	}
	raw, err := sgn.Mint(passthroughlink.Payload{
		Issuer:    passthroughlink.IssuerChannelsd,
		Audience:  passthroughlink.AudienceIdentityd,
		Subject:   subject,
		Purpose:   "portal",
		ExpiresAt: exp.Unix(),
	})
	require.NoError(t, err)
	return raw
}

// doPortalGet sends GET /my/accounts with an optional cookie and
// optional d/sig query (the portal-link bootstrap path). Routes through
// the webui framework so the page renderer is injected.
func doPortalGet(t *testing.T, fx linkFixture, cookieValue, rawLink string) *httptest.ResponseRecorder {
	t.Helper()
	target := "/my/accounts"
	if rawLink != "" {
		d, sig, ok := splitRaw(rawLink)
		require.True(t, ok, "rawLink must be <b64>.<sig>")
		target += "?d=" + url.QueryEscape(d) + "&sig=" + url.QueryEscape(sig)
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if cookieValue != "" {
		req.AddCookie(&http.Cookie{Name: cookieName, Value: cookieValue})
	}
	return serveWeb(t, fx, req)
}

// makeUserIdentity returns a UserIdentity with the given credentials
// already linked. Used to seed the linked-list rendering tests.
func makeUserIdentity(subject string, credNames ...string) *spiceboxv1alpha1.UserIdentity {
	name := useridentity.NameForSubject(identity.Subject(subject))
	creds := make([]spiceboxv1alpha1.AgentCredential, 0, len(credNames))
	for _, c := range credNames {
		creds = append(creds, spiceboxv1alpha1.AgentCredential{
			Name: c,
			Type: "static",
			Static: &spiceboxv1alpha1.StaticCredentialSource{
				SecretRef: spiceboxv1alpha1.SecretKeyRef{
					Name: useridentity.MasterSecretName(name, c),
					Key:  "token",
				},
			},
		})
	}
	return &spiceboxv1alpha1.UserIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: spiceboxv1alpha1.UserIdentitySpec{
			Subject:     subject,
			Credentials: creds,
		},
	}
}

// makeMasterSecret returns the master Secret backing a (subject,
// credName) pair. Mirrors what useridentity.PutToken would create.
func makeMasterSecret(subject, credName, token string) *corev1.Secret {
	name := useridentity.NameForSubject(identity.Subject(subject))
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      useridentity.MasterSecretName(name, credName),
			Namespace: spiceboxv1alpha1.IdentitiesNamespace,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"token": []byte(token)},
	}
}

// getPortalBodyWithLinkedCred seeds a UserIdentity with one linked
// credential of the given Type ("oauth" | "static"), issues an
// authenticated GET /my/accounts, and returns the response body string.
// Modeled on makeUserIdentity + doPortalGet above; used to assert on the
// IsOAuth projection in the linked-credentials bootstrap.
func getPortalBodyWithLinkedCred(t *testing.T, credName, credType string) string {
	t.Helper()
	const canonical = "user:alice@example.com"
	name := useridentity.NameForSubject(canonical)
	ui := &spiceboxv1alpha1.UserIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: spiceboxv1alpha1.UserIdentitySpec{
			Subject: canonical,
			Credentials: []spiceboxv1alpha1.AgentCredential{
				{Name: credName, Type: credType},
			},
		},
	}
	fx := newLinkFixtureNoAuth(t, ui)
	cookie := mintCookie(t, fx.signer, canonical, time.Time{})

	rec := doPortalGet(t, fx, cookie, "")
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	return rec.Body.String()
}

func TestPortalGet_LinkedOAuthCredMarkedIsOAuth(t *testing.T) {
	credName := "linear"
	body := getPortalBodyWithLinkedCred(t, credName, "oauth")
	assert.Contains(t, body, `"name":"`+credName+`","label":`,
		"linked credential must serialize")
	assert.Contains(t, body, `"isOAuth":true`,
		"linked OAuth credential must be marked isOAuth=true so Replace routes to /link/oauth/<cred>")
}

// TestCredentialNeedsRefresh_UnregisteredTypeLogsAndDegradesToFalse is the
// R15-shaped case for the `cred.Type == "oauth"` → registry-dispatch
// migration: for a known type (oauth, static — see the two tests above) old
// and new code agree on the boolean, so neither the true nor the false case
// alone proves the registry is actually being consulted. An UNREGISTERED
// type is where they diverge — the OLD code compared a literal string and
// silently produced false with no trace; the NEW code must both degrade to
// false (the safe default: a nonexistent OAuth ceremony would be worse than
// an inapplicable paste form) AND log, which the old code never did at all.
func TestCredentialNeedsRefresh_UnregisteredTypeLogsAndDegradesToFalse(t *testing.T) {
	var mu sync.Mutex
	var logged []string
	capLogger := funcr.New(func(_, args string) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, args)
	}, funcr.Options{})

	got := credentialNeedsRefresh(spiceboxv1alpha1.AgentCredential{Name: "mystery", Type: "nosuch"}, capLogger)
	assert.False(t, got, "an unregistered type must route Replace to the PAT form, not a nonexistent OAuth ceremony")

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, logged, "an unregistered credential type must be logged, not silently dropped")
	joined := strings.Join(logged, "\n")
	assert.Contains(t, joined, "mystery", "log must name the credential")
	assert.Contains(t, joined, "nosuch", "log must name the unrecognized type")
}

func TestPortalGet_LinkedStaticCredMarkedNotOAuth(t *testing.T) {
	credName := "github"
	body := getPortalBodyWithLinkedCred(t, credName, "static")
	assert.Contains(t, body, `"isOAuth":false`,
		"linked static (PAT) credential must be marked isOAuth=false so Replace routes to /my/accounts/<cred>/link")
}

// === GET /my/accounts ======================================================

func TestHandlePortalGet_HappyPath(t *testing.T) {
	const canonical = "user:alice@example.com"
	fx := newLinkFixtureNoAuth(t,
		makeUserIdentity(canonical, "github-pat", "linear-pat"),
	)
	cookie := mintCookie(t, fx.signer, canonical, time.Time{})

	rec := doPortalGet(t, fx, cookie, "")
	require.Equal(t, http.StatusOK, rec.Code, "happy path → 200; body=%s", rec.Body.String())
	body := rec.Body.String()
	// The portal is now the React identity-portal app; the handler emits
	// the document mounting it + the JSON bootstrap of linked credentials.
	assert.Contains(t, body, `data-app="identity-portal"`, "mounts the identity-portal app")
	assert.Contains(t, body, `"displayName":`, "bootstrap carries the signed-in display name")
	assert.Contains(t, body, canonical, "subject rendered in the bootstrap")
	assert.Contains(t, body, `"linked":`, "bootstrap carries the linked-credentials list")
	assert.Contains(t, body, `"name":"github-pat"`, "github-pat present in the linked bootstrap")
	assert.Contains(t, body, `"name":"linear-pat"`, "linear-pat present in the linked bootstrap")
	assert.Contains(t, body, `<script type="module" src="/assets/`,
		"the resolved app must have a built entry script in the manifest")
}

func TestHandlePortalGet_PortalLinkBootstrap(t *testing.T) {
	const canonical = "user:alice@example.com"
	fx := newLinkFixtureNoAuth(t)
	raw := mintPortalLink(t, fx.signer, canonical, time.Time{})

	rec := doPortalGet(t, fx, "", raw)
	require.Equal(t, http.StatusOK, rec.Code, "valid portal link → 200; body=%s", rec.Body.String())

	// The handler must set the idd_session cookie so subsequent navigation
	// (link form, revoke) works without re-presenting the portal link.
	var iddCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == cookieName {
			iddCookie = c
		}
	}
	require.NotNil(t, iddCookie, "portal-link path must set idd_session cookie")
	p, err := fx.signer.Verify(iddCookie.Value)
	require.NoError(t, err)
	assert.Equal(t, identity.Subject(canonical), p.Subject, "cookie payload carries the portal-link Subject")
}

func TestHandlePortalGet_PortalLinkSingleUse(t *testing.T) {
	// α7: a portal-access link bootstraps the cookie exactly once.
	// Second visit with the SAME link must be blocked. The cookie
	// minted by the first visit continues to work on its own; the
	// rejection only applies to re-presenting the consumed link.
	const canonical = "user:alice@example.com"
	fx := newLinkFixtureNoAuth(t)
	raw := mintPortalLink(t, fx.signer, canonical, time.Time{})

	rec1 := doPortalGet(t, fx, "", raw)
	require.Equal(t, http.StatusOK, rec1.Code, "first visit succeeds; body=%s", rec1.Body.String())

	// Second visit with the same link (no cookie carried over) — blocked.
	rec2 := doPortalGet(t, fx, "", raw)
	require.Equal(t, http.StatusBadRequest, rec2.Code,
		"second visit with consumed link must be rejected; body=%s", rec2.Body.String())
	assert.Contains(t, rec2.Body.String(), "already been used")
}

func TestHandlePortalGet_WrongPurposeLinkRejected(t *testing.T) {
	// A link with Purpose == "" (the credential-request shape from
	// /link) MUST NOT be accepted at /my/accounts. The Purpose field is
	// what distinguishes a portal bootstrap from a session-credential
	// link being replayed against the portal.
	const canonical = "user:alice@example.com"
	fx := newLinkFixtureNoAuth(t)
	raw := mintLink(t, fx.signer, "default/sess-1", canonical, []string{"github-pat"}, time.Time{})

	rec := doPortalGet(t, fx, "", raw)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	// Error renders the framework system page; the message is in its
	// JSON bootstrap.
	assert.Contains(t, rec.Body.String(), `data-app="system"`)
	assert.Contains(t, rec.Body.String(), "portal-access link")
}

func TestHandlePortalGet_NoCookieNoLink_Unauthorized(t *testing.T) {
	fx := newLinkFixtureNoAuth(t)
	rec := doPortalGet(t, fx, "", "")
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), "Sign-in required")
}

func TestHandlePortalGet_NewUser_NoUserIdentityYet(t *testing.T) {
	// A user who has cookied but has never linked any credential should
	// still see the page (empty linked list, friendly empty-state).
	const canonical = "user:carol@example.com"
	fx := newLinkFixtureNoAuth(t) // no UserIdentity seeded
	cookie := mintCookie(t, fx.signer, canonical, time.Time{})

	rec := doPortalGet(t, fx, cookie, "")
	require.Equal(t, http.StatusOK, rec.Code,
		"missing UserIdentity is the new-user case, not an error; body=%s", rec.Body.String())
	body := rec.Body.String()
	// New user: the page still renders (the React empty-state is driven
	// by an empty linked list in the bootstrap, not server markup). A
	// new user has no UserIdentity, so the linked slice marshals as null.
	assert.Contains(t, body, `data-app="identity-portal"`, "page still renders for a new user")
	assert.Contains(t, body, `"linked":null`, "new user has no linked credentials in the bootstrap")
}

func TestHandlePortalGet_MethodNotAllowed(t *testing.T) {
	fx := newLinkFixtureNoAuth(t)
	req := httptest.NewRequest(http.MethodPost, "/my/accounts", nil)
	rec := serveWeb(t, fx, req)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

// === GET /my/accounts/<cred>/link ==========================================

func TestHandlePortalLinkForm_HappyPath(t *testing.T) {
	const (
		canonical = "user:alice@example.com"
		credName  = "github-pat"
	)
	fx := newLinkFixtureNoAuth(t)
	cookie := mintCookie(t, fx.signer, canonical, time.Time{})

	req := httptest.NewRequest(http.MethodGet, "/my/accounts/"+credName+"/link", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
	rec := serveWeb(t, fx, req)

	require.Equal(t, http.StatusOK, rec.Code, "form GET → 200; body=%s", rec.Body.String())
	body := rec.Body.String()
	// The PAT form is now the React identity-link-form app; the handler
	// emits the document mounting it + the credential-name bootstrap.
	assert.Contains(t, body, `data-app="identity-link-form"`, "mounts the identity-link-form app")
	assert.Contains(t, body, `"credentialName":"`+credName+`"`,
		"bootstrap carries the credential the form posts for")
	assert.Contains(t, body, `<script type="module" src="/assets/`,
		"the resolved app must have a built entry script in the manifest")
}

func TestHandlePortalLinkForm_NoCookie_Unauthorized(t *testing.T) {
	fx := newLinkFixtureNoAuth(t)
	req := httptest.NewRequest(http.MethodGet, "/my/accounts/github-pat/link", nil)
	rec := serveWeb(t, fx, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// === POST /my/accounts/<cred>/link/submit ==================================

func doPortalSubmit(t *testing.T, fx linkFixture, credName, token, cookieValue string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{}
	if token != "" {
		form.Set("token", token)
	}
	return doPortalSubmitForm(t, fx, credName, form, cookieValue)
}

// doPortalSubmitForm is doPortalSubmit generalized to an arbitrary form —
// used by the verify-confirm tests to add verifyConfirm=1 alongside token.
func doPortalSubmitForm(t *testing.T, fx linkFixture, credName string, form url.Values, cookieValue string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/my/accounts/"+credName+"/link/submit",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookieValue != "" {
		req.AddCookie(&http.Cookie{Name: cookieName, Value: cookieValue})
	}
	return serveWeb(t, fx, req)
}

func TestHandlePortalLinkSubmit_HappyPath(t *testing.T) {
	stubVerify(t, http.StatusOK, `{"login":"tester"}`)
	// "github-token" (not "github-pat") is used deliberately: it's the
	// credential name gh.yaml's toolkit entry actually declares, so it
	// resolves via passthroughcatalog.ProviderForCredential to the
	// github-pat provider and the stub above is genuinely exercised
	// (VerifyValid → "linked" notice). "github-pat" resolves to no
	// provider at all and would always verify as Indeterminate.
	const (
		canonical  = "user:alice@example.com"
		credName   = "github-token"
		tokenValue = "ghp_aliceTOKEN"
	)
	fx := newLinkFixtureNoAuth(t)
	cookie := mintCookie(t, fx.signer, canonical, time.Time{})

	rec := doPortalSubmit(t, fx, credName, tokenValue, cookie)
	require.Equal(t, http.StatusSeeOther, rec.Code,
		"submit success → 303 redirect (Post/Redirect/Get); body=%s", rec.Body.String())
	assert.Equal(t, "/my/accounts?notice=linked:"+credName, rec.Header().Get("Location"),
		"redirect lands back on the portal with a linked notice")

	// UserIdentity created + Secret created — the same shape PutToken
	// produces from the reactive /link/submit handler.
	uiName := useridentity.NameForSubject(canonical)
	var ui spiceboxv1alpha1.UserIdentity
	require.NoError(t, fx.c.Get(context.Background(), clientpkg.ObjectKey{Name: uiName}, &ui))
	require.Len(t, ui.Spec.Credentials, 1)
	assert.Equal(t, credName, ui.Spec.Credentials[0].Name)

	secName := useridentity.MasterSecretName(uiName, credName)
	var sec corev1.Secret
	require.NoError(t, fx.c.Get(context.Background(),
		clientpkg.ObjectKey{Namespace: spiceboxv1alpha1.IdentitiesNamespace, Name: secName}, &sec))
	assert.Equal(t, tokenValue, string(sec.Data["token"]))
}

// TestHandlePortalLinkSubmit_VerifyRejectedRendersWarnPage — mirrors
// TestHandleLinkSubmit_VerifyRejectedRendersWarnPage for the standing-portal
// submit path. "github-token" resolves to the github-pat provider via the
// gh.yaml toolkit's credential field, so the live verify: probe actually
// runs (unlike the "github-pat" name the other portal tests use).
func TestHandlePortalLinkSubmit_VerifyRejectedRendersWarnPage(t *testing.T) {
	stubVerify(t, http.StatusUnauthorized, `{"message":"Bad credentials"}`)
	const (
		canonical  = "user:alice@example.com"
		credName   = "github-token"
		tokenValue = "ghp_dead"
	)
	fx := newLinkFixtureNoAuth(t)
	cookie := mintCookie(t, fx.signer, canonical, time.Time{})

	rec := doPortalSubmit(t, fx, credName, tokenValue, cookie)
	require.Equal(t, http.StatusOK, rec.Code, "rejected verification renders the warn page, not a redirect")
	body := rec.Body.String()
	assert.Contains(t, body, `data-app="identity-verify-warn"`, "mounts the identity-verify-warn React app")
	assert.Contains(t, body, "Bad credentials", "the provider's rejection detail must be surfaced")
	assert.Contains(t, body, `"verifyConfirm":"1"`, "the re-submit form carries verifyConfirm=1")
	assert.Contains(t, body, `/my/accounts/`+credName+`/link/submit`, "the re-submit form posts back to this credential's portal submit action")

	uiName := useridentity.NameForSubject(canonical)
	var ui spiceboxv1alpha1.UserIdentity
	err := fx.c.Get(context.Background(), clientpkg.ObjectKey{Name: uiName}, &ui)
	assert.True(t, apierrors.IsNotFound(err), "a rejected token must NOT be stored; got err=%v", err)
}

// TestHandlePortalLinkSubmit_VerifyForbiddenRendersWarnPage mirrors
// TestHandleLinkSubmit_VerifyForbiddenRendersWarnPage for the standing portal:
// a refused check asks rather than stores, worded as an authenticated
// credential that this one check did not clear.
func TestHandlePortalLinkSubmit_VerifyForbiddenRendersWarnPage(t *testing.T) {
	stubVerify(t, http.StatusForbidden,
		`{"message":"Resource protected by organization SAML enforcement."}`)
	const (
		canonical  = "user:alice@example.com"
		credName   = "github-token"
		tokenValue = "ghp_liveButSSORestricted"
	)
	fx := newLinkFixtureNoAuth(t)
	cookie := mintCookie(t, fx.signer, canonical, time.Time{})

	rec := doPortalSubmit(t, fx, credName, tokenValue, cookie)
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

// TestHandlePortalLinkSubmit_UnrecognizedVerdictRendersWarnPage mirrors
// TestHandleLinkSubmit_UnrecognizedVerdictRendersWarnPage for the standing
// portal: the submit switch's default arm asks rather than stores.
func TestHandlePortalLinkSubmit_UnrecognizedVerdictRendersWarnPage(t *testing.T) {
	stubVerifyVerdict(t, builtins.VerifyResult{
		Status: "verdict-this-build-does-not-know", Detail: "a verdict from a later build",
	})
	const (
		canonical  = "user:alice@example.com"
		credName   = "github-token"
		tokenValue = "ghp_unknownverdict"
	)
	fx := newLinkFixtureNoAuth(t)
	cookie := mintCookie(t, fx.signer, canonical, time.Time{})

	rec := doPortalSubmit(t, fx, credName, tokenValue, cookie)
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

// TestHandlePortalLinkSubmit_VerifyConfirmStores — the "Store anyway"
// re-submit (verifyConfirm=1) stores despite the same rejection, and the
// redirect notice tells the portal the credential was linked unverified.
func TestHandlePortalLinkSubmit_VerifyConfirmStores(t *testing.T) {
	stubVerify(t, http.StatusUnauthorized, `{"message":"Bad credentials"}`)
	const (
		canonical  = "user:alice@example.com"
		credName   = "github-token"
		tokenValue = "ghp_dead"
	)
	fx := newLinkFixtureNoAuth(t)
	cookie := mintCookie(t, fx.signer, canonical, time.Time{})

	form := url.Values{"token": {tokenValue}, "verifyConfirm": {"1"}}
	rec := doPortalSubmitForm(t, fx, credName, form, cookie)
	require.Equal(t, http.StatusSeeOther, rec.Code, "verifyConfirm=1 stores + redirects despite the rejection")
	assert.Equal(t, "/my/accounts?notice=linkedunverified:"+credName, rec.Header().Get("Location"))

	uiName := useridentity.NameForSubject(canonical)
	var ui spiceboxv1alpha1.UserIdentity
	require.NoError(t, fx.c.Get(context.Background(), clientpkg.ObjectKey{Name: uiName}, &ui),
		"UserIdentity must be created on a confirmed store")
	require.Len(t, ui.Spec.Credentials, 1)
	assert.Equal(t, credName, ui.Spec.Credentials[0].Name)
}

// TestHandlePortalLinkSubmit_UnsupportedProviderStoresLinked proves that a
// credential with NO associated provider (no live verification available —
// e.g. a passthrough MCP provider like linear-oauth in production) stores
// quietly with the plain "linked" notice, NOT "linkedunverified". "github-pat"
// resolves to no provider in this fixture (see the HappyPath comment above:
// no toolkit or MCPServer maps that exact name), so verifyPastedToken hits
// the no-provider → Unsupported branch and no HTTP probe is ever attempted —
// stubVerify is deliberately NOT installed here.
func TestHandlePortalLinkSubmit_UnsupportedProviderStoresLinked(t *testing.T) {
	const (
		canonical  = "user:alice@example.com"
		credName   = "github-pat"
		tokenValue = "sometoken"
	)
	fx := newLinkFixtureNoAuth(t)
	cookie := mintCookie(t, fx.signer, canonical, time.Time{})

	rec := doPortalSubmit(t, fx, credName, tokenValue, cookie)
	require.Equal(t, http.StatusSeeOther, rec.Code,
		"submit success → 303 redirect (Post/Redirect/Get); body=%s", rec.Body.String())
	assert.Equal(t, "/my/accounts?notice=linked:"+credName, rec.Header().Get("Location"),
		"unsupported (no verifier) must NOT downgrade the notice to linkedunverified")

	uiName := useridentity.NameForSubject(canonical)
	var ui spiceboxv1alpha1.UserIdentity
	require.NoError(t, fx.c.Get(context.Background(), clientpkg.ObjectKey{Name: uiName}, &ui))
	require.Len(t, ui.Spec.Credentials, 1)
	assert.Equal(t, credName, ui.Spec.Credentials[0].Name)
}

func TestHandlePortalLinkSubmit_GateFailures(t *testing.T) {
	const (
		canonical = "user:alice@example.com"
		credName  = "github-pat"
	)

	cases := []struct {
		name         string
		cookie       string // "" → no cookie
		token        string
		wantStatus   int
		wantContains string
	}{
		{
			name:         "no cookie → 403",
			cookie:       "",
			token:        "some-token",
			wantStatus:   http.StatusForbidden,
			wantContains: "Not signed in",
		},
		{
			name:         "missing token → 400",
			token:        "",
			wantStatus:   http.StatusBadRequest,
			wantContains: "Missing token",
		},
		{
			name:         "whitespace-only token → 400",
			token:        "   ",
			wantStatus:   http.StatusBadRequest,
			wantContains: "Missing token",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newLinkFixtureNoAuth(t)
			cookieVal := tc.cookie
			if tc.name != "no cookie → 403" {
				cookieVal = mintCookie(t, fx.signer, canonical, time.Time{})
			}
			rec := doPortalSubmit(t, fx, credName, tc.token, cookieVal)
			assert.Equal(t, tc.wantStatus, rec.Code)
			assert.Contains(t, rec.Body.String(), tc.wantContains)

			// No UserIdentity should ever be created on a gate failure.
			uiName := useridentity.NameForSubject(canonical)
			var ui spiceboxv1alpha1.UserIdentity
			err := fx.c.Get(context.Background(), clientpkg.ObjectKey{Name: uiName}, &ui)
			assert.True(t, apierrors.IsNotFound(err),
				"no credential should be persisted on a gate failure")
		})
	}
}

// === POST /my/accounts/<cred>/revoke =======================================

func doPortalRevoke(t *testing.T, fx linkFixture, credName, cookieValue string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/my/accounts/"+credName+"/revoke", nil)
	if cookieValue != "" {
		req.AddCookie(&http.Cookie{Name: cookieName, Value: cookieValue})
	}
	return serveWeb(t, fx, req)
}

func TestHandlePortalRevoke_HappyPath(t *testing.T) {
	const (
		canonical = "user:alice@example.com"
		credName  = "github-pat"
	)
	fx := newLinkFixtureNoAuth(t,
		makeUserIdentity(canonical, credName, "linear-pat"),
		makeMasterSecret(canonical, credName, "ghp_old"),
		makeMasterSecret(canonical, "linear-pat", "lin_old"),
	)
	cookie := mintCookie(t, fx.signer, canonical, time.Time{})

	rec := doPortalRevoke(t, fx, credName, cookie)
	require.Equal(t, http.StatusSeeOther, rec.Code,
		"revoke success → 303 redirect (Post/Redirect/Get); body=%s", rec.Body.String())
	assert.Equal(t, "/my/accounts?notice=revoked:"+credName, rec.Header().Get("Location"),
		"redirect lands back on the portal with a revoked notice")

	// Master Secret for the revoked credential MUST be gone.
	uiName := useridentity.NameForSubject(canonical)
	secName := useridentity.MasterSecretName(uiName, credName)
	var sec corev1.Secret
	err := fx.c.Get(context.Background(),
		clientpkg.ObjectKey{Namespace: spiceboxv1alpha1.IdentitiesNamespace, Name: secName}, &sec)
	assert.True(t, apierrors.IsNotFound(err), "master Secret should be deleted")

	// UserIdentity entry for the revoked credential MUST be gone; the
	// other credential MUST remain.
	var ui spiceboxv1alpha1.UserIdentity
	require.NoError(t, fx.c.Get(context.Background(), clientpkg.ObjectKey{Name: uiName}, &ui))
	require.Len(t, ui.Spec.Credentials, 1, "only linear-pat should remain")
	assert.Equal(t, "linear-pat", ui.Spec.Credentials[0].Name)
}

func TestHandlePortalRevoke_IdempotentNoSecret(t *testing.T) {
	// The credential is listed on the UserIdentity but the master Secret
	// has already been deleted out-of-band (operator cleanup, GC). The
	// revoke must still succeed and remove the UserIdentity entry.
	const (
		canonical = "user:alice@example.com"
		credName  = "github-pat"
	)
	fx := newLinkFixtureNoAuth(t, makeUserIdentity(canonical, credName))
	cookie := mintCookie(t, fx.signer, canonical, time.Time{})

	rec := doPortalRevoke(t, fx, credName, cookie)
	require.Equal(t, http.StatusSeeOther, rec.Code,
		"NotFound on master Secret is fine → 303 redirect; body=%s", rec.Body.String())

	uiName := useridentity.NameForSubject(canonical)
	var ui spiceboxv1alpha1.UserIdentity
	require.NoError(t, fx.c.Get(context.Background(), clientpkg.ObjectKey{Name: uiName}, &ui))
	assert.Empty(t, ui.Spec.Credentials, "credential entry must be removed")
}

func TestHandlePortalRevoke_IdempotentNoUserIdentity(t *testing.T) {
	// No UserIdentity at all. Revoke is a no-op; success is correct
	// because the user's intent ("forget this credential") is met.
	const (
		canonical = "user:alice@example.com"
		credName  = "github-pat"
	)
	fx := newLinkFixtureNoAuth(t)
	cookie := mintCookie(t, fx.signer, canonical, time.Time{})

	rec := doPortalRevoke(t, fx, credName, cookie)
	require.Equal(t, http.StatusSeeOther, rec.Code,
		"missing UserIdentity is a no-op success → 303 redirect; body=%s", rec.Body.String())
	assert.Equal(t, "/my/accounts?notice=revoked:"+credName, rec.Header().Get("Location"))
}

func TestHandlePortalRevoke_NoCookie_Forbidden(t *testing.T) {
	fx := newLinkFixtureNoAuth(t)
	rec := doPortalRevoke(t, fx, "github-pat", "")
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "Not signed in")
}

// === Suggested section OAuth routing =======================================

func TestHandlePortalGet_SuggestedOAuthLinkHref(t *testing.T) {
	// When the suggested section contains an OAuth credential, the "Link"
	// button must point to /link/oauth/<credname>, not the PAT form.
	const (
		canonical = "user:alice@example.com"
		credName  = "linear-oauth"
	)
	fx := newLinkFixtureNoAuth(t,
		makePassthroughAgentClass("default", "bot-a", "linear-mcp"),
		makeMCPServerWithAuthType("default", "linear-mcp", credName, "Linear", "oauth"),
	)
	cookie := mintCookie(t, fx.signer, canonical, time.Time{})

	rec := doPortalGet(t, fx, cookie, "")
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	body := rec.Body.String()

	// The OAuth-vs-PAT routing decision is now carried by isOAuth in the
	// suggested credential's bootstrap; the React app builds the
	// /link/oauth/<credname> URL from it.
	assert.Contains(t, body, `"suggested":`, "bootstrap carries the suggested section")
	assert.Contains(t, body, `"name":"`+credName+`","label":"Linear","isOAuth":true`,
		"OAuth credential must be marked isOAuth=true so the app routes to /link/oauth/<credname>")
}

func TestHandlePortalGet_SuggestedPATLinkHref(t *testing.T) {
	// When the suggested section contains a PAT (static) credential, the
	// "Link" button must point to /my/accounts/<credname>/link, not the
	// OAuth path.
	const (
		canonical = "user:alice@example.com"
		credName  = "github-pat"
	)
	fx := newLinkFixtureNoAuth(t,
		makePassthroughAgentClass("default", "bot-a", "github-mcp"),
		makeMCPServerWithAuthType("default", "github-mcp", credName, "GitHub", "static"),
	)
	cookie := mintCookie(t, fx.signer, canonical, time.Time{})

	rec := doPortalGet(t, fx, cookie, "")
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	body := rec.Body.String()

	// A static (PAT) credential must be marked isOAuth=false so the React
	// app routes to the /my/accounts/<credname>/link PAT form, not OAuth.
	assert.Contains(t, body, `"suggested":`, "bootstrap carries the suggested section")
	assert.Contains(t, body, `"name":"`+credName+`","label":"GitHub","isOAuth":false`,
		"PAT credential must be marked isOAuth=false")
}

// === sub-router & helpers ==================================================

func TestPortalSubrouter_MethodMismatch(t *testing.T) {
	fx := newLinkFixtureNoAuth(t)
	// GET /my/accounts/<cred>/revoke is a POST-only path.
	req := httptest.NewRequest(http.MethodGet, "/my/accounts/github-pat/revoke", nil)
	rec := serveWeb(t, fx, req)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code,
		"GET on a POST-only sub-route → 405")
}

func TestPortalSubrouter_UnknownSubroute(t *testing.T) {
	fx := newLinkFixtureNoAuth(t)
	req := httptest.NewRequest(http.MethodGet, "/my/accounts/github-pat/unknown", nil)
	rec := serveWeb(t, fx, req)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code,
		"unknown sub-route under /my/accounts/ → 405 (the helpful Allow header points to /my/accounts)")
}

func TestPortalCredNameFromPath(t *testing.T) {
	cases := []struct {
		name     string
		path     string
		suffix   string
		wantOK   bool
		wantName string
	}{
		{name: "happy /link", path: "/my/accounts/github-pat/link", suffix: "/link", wantOK: true, wantName: "github-pat"},
		{name: "happy /link/submit", path: "/my/accounts/linear/link/submit", suffix: "/link/submit", wantOK: true, wantName: "linear"},
		{name: "happy /revoke", path: "/my/accounts/x/revoke", suffix: "/revoke", wantOK: true, wantName: "x"},
		{name: "wrong prefix", path: "/foo/github-pat/link", suffix: "/link", wantOK: false},
		{name: "wrong suffix", path: "/my/accounts/github-pat/revoke", suffix: "/link", wantOK: false},
		{name: "empty credential", path: "/my/accounts//link", suffix: "/link", wantOK: false},
		{name: "multi-segment credential rejected", path: "/my/accounts/foo/bar/link", suffix: "/link", wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := portalCredNameFromPath(tc.path, tc.suffix)
			assert.Equal(t, tc.wantOK, ok)
			if tc.wantOK {
				assert.Equal(t, tc.wantName, got)
			}
		})
	}
}

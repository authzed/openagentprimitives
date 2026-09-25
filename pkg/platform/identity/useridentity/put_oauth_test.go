package useridentity

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	// Registers the static/oauth/federated credkind.Kinds so
	// refresh.Run's credkindregistry.Get(cred.Type) dispatch resolves in
	// this package's tests (exercised via TestPutOAuthToken_PersistedOAuthCredentialIsRefreshable).
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/imports"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/refresh"
	"github.com/authzed/openagentprimitives/pkg/x/safehttp"
)

func newOAuthTestClient(t *testing.T) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))
	return fake.NewClientBuilder().WithScheme(s).Build()
}

func TestPutOAuthToken_NewSecretAndUserIdentity(t *testing.T) {
	ctx := context.Background()
	c := newOAuthTestClient(t)
	expiresAt := time.Now().Add(time.Hour).Unix()
	req := PutOAuthTokenRequest{
		Subject:        "user:alice@example.com",
		CredentialName: "linear-oauth",
		AccessToken:    "at-1",
		RefreshToken:   "rt-1",
		ExpiresAt:      expiresAt,
		TokenType:      "Bearer",
		Scope:          "read write",
		TokenEndpoint:  "https://idp.example.invalid/token",
		ClientID:       "dcr-client-id",
		ClientSecret:   "dcr-client-secret",
	}
	require.NoError(t, PutOAuthToken(ctx, c, req))

	uiName := NameForSubject(req.Subject)
	secName := MasterSecretName(uiName, req.CredentialName)
	var sec corev1.Secret
	require.NoError(t, c.Get(ctx, client.ObjectKey{
		Namespace: spiceboxv1alpha1.IdentitiesNamespace,
		Name:      secName,
	}, &sec))
	assert.Equal(t, []byte("at-1"), sec.Data["access_token"])
	assert.Equal(t, []byte("rt-1"), sec.Data["refresh_token"])
	assert.Equal(t, []byte("Bearer"), sec.Data["token_type"])
	assert.Equal(t, []byte("read write"), sec.Data["scope"])
	assert.NotEmpty(t, sec.Data["expires_at"])
	// Redemption material is NOT here: a `get` returns every key, and this is
	// the Secret a userPassthrough session's runner is granted.
	for _, key := range []string{"token_endpoint", "client_id", "client_secret"} {
		assert.NotContainsf(t, sec.Data, key, "master Secret must not carry %q", key)
	}

	// It lives in the sibling, which nothing grants the runner.
	var sib corev1.Secret
	require.NoError(t, c.Get(ctx, client.ObjectKey{
		Namespace: spiceboxv1alpha1.IdentitiesNamespace,
		Name:      refresh.MaterialSecretName(secName),
	}, &sib))
	assert.Equal(t, []byte("https://idp.example.invalid/token"), sib.Data["token_endpoint"])
	assert.Equal(t, []byte("dcr-client-id"), sib.Data["client_id"])
	assert.Equal(t, []byte("dcr-client-secret"), sib.Data["client_secret"])
	assert.Contains(t, sib.Labels, adoptguard.AdoptedLabel,
		"the operator's Secret informer is label-filtered; without this its refresh path cannot read the sibling")
	assert.Contains(t, sib.Labels, refresh.MaterialSecretLabel,
		"the derived name is a hint; the label is what identifies the Secret as this credential's material")
	require.Len(t, sib.OwnerReferences, 1, "unlinking the credential must reap the redemption material")
	assert.Equal(t, secName, sib.OwnerReferences[0].Name)
	assert.Equal(t, "Secret", sib.OwnerReferences[0].Kind)

	var ui spiceboxv1alpha1.UserIdentity
	require.NoError(t, c.Get(ctx, client.ObjectKey{Name: uiName}, &ui))
	require.Len(t, ui.Spec.Credentials, 1)
	assert.Equal(t, "linear-oauth", ui.Spec.Credentials[0].Name)
	assert.Equal(t, "oauth", ui.Spec.Credentials[0].Type)
	require.NotNil(t, ui.Spec.Credentials[0].OAuth)
	assert.Equal(t, secName, ui.Spec.Credentials[0].OAuth.SecretRef.Name)
}

func TestPutOAuthToken_OverwritesExistingSecret(t *testing.T) {
	ctx := context.Background()
	c := newOAuthTestClient(t)
	base := PutOAuthTokenRequest{
		Subject:        "user:alice@example.com",
		CredentialName: "linear-oauth",
		AccessToken:    "v1",
		RefreshToken:   "rv1",
		ExpiresAt:      time.Now().Add(time.Hour).Unix(),
		TokenType:      "Bearer",
		TokenEndpoint:  "https://idp.example.invalid/token",
	}
	require.NoError(t, PutOAuthToken(ctx, c, base))

	base.AccessToken = "v2"
	base.RefreshToken = "rv2"
	require.NoError(t, PutOAuthToken(ctx, c, base))

	uiName := NameForSubject(base.Subject)
	secName := MasterSecretName(uiName, base.CredentialName)
	var sec corev1.Secret
	require.NoError(t, c.Get(ctx, client.ObjectKey{
		Namespace: spiceboxv1alpha1.IdentitiesNamespace,
		Name:      secName,
	}, &sec))
	assert.Equal(t, []byte("v2"), sec.Data["access_token"])
	assert.Equal(t, []byte("rv2"), sec.Data["refresh_token"])

	var ui spiceboxv1alpha1.UserIdentity
	require.NoError(t, c.Get(ctx, client.ObjectKey{Name: uiName}, &ui))
	require.Len(t, ui.Spec.Credentials, 1, "still exactly one entry after replace")
}

func TestPutOAuthToken_OmitsEmptyFields(t *testing.T) {
	ctx := context.Background()
	c := newOAuthTestClient(t)
	// Provider doesn't issue a refresh token + no scope.
	req := PutOAuthTokenRequest{
		Subject:        "user:bob@example.com",
		CredentialName: "github-oauth",
		AccessToken:    "at-x",
		TokenType:      "Bearer",
		// RefreshToken: empty
		// Scope: empty
		// ExpiresAt: 0 (no expiry info)
	}
	require.NoError(t, PutOAuthToken(ctx, c, req))

	secName := MasterSecretName(NameForSubject(req.Subject), req.CredentialName)
	var sec corev1.Secret
	require.NoError(t, c.Get(ctx, client.ObjectKey{
		Namespace: spiceboxv1alpha1.IdentitiesNamespace,
		Name:      secName,
	}, &sec))
	assert.Equal(t, []byte("at-x"), sec.Data["access_token"])
	_, hasRefresh := sec.Data["refresh_token"]
	assert.False(t, hasRefresh, "refresh_token key should be absent when not provided")
	_, hasScope := sec.Data["scope"]
	assert.False(t, hasScope, "scope key should be absent when not provided")
	_, hasExpires := sec.Data["expires_at"]
	assert.False(t, hasExpires, "expires_at key should be absent when zero")
}

func TestPutOAuthToken_ValidatesRequired(t *testing.T) {
	c := newOAuthTestClient(t)
	cases := []struct {
		name string
		req  PutOAuthTokenRequest
	}{
		{"empty subject", PutOAuthTokenRequest{CredentialName: "x", AccessToken: "y"}},
		{"empty credential name", PutOAuthTokenRequest{Subject: "user:x", AccessToken: "y"}},
		{"empty access token", PutOAuthTokenRequest{Subject: "user:x", CredentialName: "y"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := PutOAuthToken(context.Background(), c, tc.req)
			require.Error(t, err)
		})
	}
}

// TestPutOAuthToken_PersistedOAuthCredentialIsRefreshable drives the exact
// shape the OAuth callback persists and then asks the question production
// asks at first expiry: can this credential actually be refreshed?
//
// The Secrets PutOAuthToken writes are the ONLY durable home for the refresh
// material — the DCR-minted client_id/client_secret and the discovered
// token_endpoint live otherwise only in identityd's in-process, single-use
// state store. If PutOAuthToken does not write them (here, into the sibling
// refresh Secret), refresh.Run refuses before issuing any request and the
// credential is permanently unrefreshable: the reconciler
// sets Refresh=False and every use fails with ErrExpired -> "jit refresh
// failed". Loud, but unactionable — only a manual re-link recovers.
//
// Must not be t.Parallel(): refresh.SetHTTPClient mutates package-level state.
func TestPutOAuthToken_PersistedOAuthCredentialIsRefreshable(t *testing.T) {
	ctx := context.Background()
	c := newOAuthTestClient(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// assert (not require): this runs on the server's goroutine.
		if !assert.NoError(t, r.ParseForm()) {
			return
		}
		assert.Equal(t, "refresh_token", r.Form.Get("grant_type"))
		assert.Equal(t, "rt-1", r.Form.Get("refresh_token"))
		assert.Equal(t, "dcr-client-id", r.Form.Get("client_id"))
		assert.Equal(t, "dcr-client-secret", r.Form.Get("client_secret"))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at-2", "refresh_token": "rt-2",
			"expires_in": 3600, "token_type": "Bearer",
		})
	}))
	t.Cleanup(srv.Close)
	// httptest serves on loopback, which refresh's SSRF-guarded dialer
	// refuses; swap in the server's own client for the test's lifetime.
	refresh.SetHTTPClient(srv.Client())
	t.Cleanup(func() { refresh.SetHTTPClient(safehttp.Client()) })

	req := PutOAuthTokenRequest{
		Subject:        "user:alice@example.com",
		CredentialName: "linear-oauth",
		AccessToken:    "at-1",
		RefreshToken:   "rt-1",
		ExpiresAt:      time.Now().Add(time.Hour).Unix(),
		TokenType:      "Bearer",
		Scope:          "read write",
		TokenEndpoint:  srv.URL,
		ClientID:       "dcr-client-id",
		ClientSecret:   "dcr-client-secret",
	}
	require.NoError(t, PutOAuthToken(ctx, c, req))

	uiName := NameForSubject(req.Subject)
	var ui spiceboxv1alpha1.UserIdentity
	require.NoError(t, c.Get(ctx, client.ObjectKey{Name: uiName}, &ui))
	require.Len(t, ui.Spec.Credentials, 1)

	// The credential the callback just persisted, refreshed exactly as the
	// reconciler and the JIT broker refresh it.
	require.NoError(t,
		refresh.Run(ctx, c, spiceboxv1alpha1.IdentitiesNamespace, ui.Spec.Credentials[0]),
		"the credential the OAuth callback persisted must be refreshable")

	var sec corev1.Secret
	require.NoError(t, c.Get(ctx, client.ObjectKey{
		Namespace: spiceboxv1alpha1.IdentitiesNamespace,
		Name:      MasterSecretName(uiName, req.CredentialName),
	}, &sec))
	assert.Equal(t, "at-2", string(sec.Data["access_token"]), "refresh rotated the access token")
	assert.Equal(t, "rt-2", string(sec.Data["refresh_token"]), "refresh rotated the refresh token")
}

// TestPutOAuthToken_RefusesRefreshTokenWithNoTokenEndpoint pins the
// fail-closed guard. A refresh token with no endpoint to redeem it at is
// never correct under RFC 6749 — it is exactly the permanently-unrefreshable
// credential above, and without the guard nothing stops a future caller from
// reintroducing it. Refusing at link time surfaces the misconfiguration where
// the user can act on it, instead of hours later at first expiry.
//
// A request with no refresh token at all is unaffected: an access-token-only
// credential is a legitimate shape (RFC 6749 does not require a refresh
// token), and the refresh controller never attempts to refresh one.
func TestPutOAuthToken_RefusesRefreshTokenWithNoTokenEndpoint(t *testing.T) {
	ctx := context.Background()
	c := newOAuthTestClient(t)

	err := PutOAuthToken(ctx, c, PutOAuthTokenRequest{
		Subject:        "user:alice@example.com",
		CredentialName: "linear-oauth",
		AccessToken:    "at-1",
		RefreshToken:   "rt-1",
		TokenType:      "Bearer",
	})
	require.Error(t, err,
		"storing a refresh token with no token_endpoint yields a credential that can never be refreshed")
	assert.Contains(t, err.Error(), "tokenEndpoint",
		"the error must name the missing field so the caller can fix it")

	// Nothing may be persisted by a refused request — a half-written
	// credential is the unrefreshable state the guard exists to prevent.
	var sec corev1.Secret
	getErr := c.Get(ctx, client.ObjectKey{
		Namespace: spiceboxv1alpha1.IdentitiesNamespace,
		Name:      MasterSecretName(NameForSubject("user:alice@example.com"), "linear-oauth"),
	}, &sec)
	assert.True(t, apierrors.IsNotFound(getErr), "refused request must not create the master Secret")

	// The access-token-only shape stays legal.
	assert.NoError(t, PutOAuthToken(ctx, c, PutOAuthTokenRequest{
		Subject:        "user:bob@example.com",
		CredentialName: "github-oauth",
		AccessToken:    "at-x",
		TokenType:      "Bearer",
	}), "a credential with no refresh token needs no token endpoint")
}

// TestPutOAuthToken_ValueChangeStampsRotationFingerprint proves the OAuth path
// closes the same same-name/different-value gap as PutToken: re-authorizing an
// already-linked OAuth credential (a new access token under the same name)
// changes the UserIdentity rotation fingerprint so the AgentSession UserIdentity
// watch fires. A same-token re-auth stays a no-op.
func TestPutOAuthToken_ValueChangeStampsRotationFingerprint(t *testing.T) {
	ctx := context.Background()
	c := newOAuthTestClient(t)
	req := PutOAuthTokenRequest{
		Subject:        "user:alice@example.com",
		CredentialName: "linear-oauth",
		AccessToken:    "at-1",
		RefreshToken:   "rt-1",
		TokenType:      "Bearer",
		TokenEndpoint:  "https://idp.example.invalid/token",
	}
	require.NoError(t, PutOAuthToken(ctx, c, req))
	uiName := NameForSubject(req.Subject)

	var ui1 spiceboxv1alpha1.UserIdentity
	require.NoError(t, c.Get(ctx, client.ObjectKey{Name: uiName}, &ui1))
	fp1 := ui1.Annotations[RotationFingerprintAnnotation]
	require.NotEmpty(t, fp1, "rotation fingerprint must be stamped on OAuth link")

	// Same tokens again → fingerprint unchanged (nothing to propagate).
	require.NoError(t, PutOAuthToken(ctx, c, req))
	var uiSame spiceboxv1alpha1.UserIdentity
	require.NoError(t, c.Get(ctx, client.ObjectKey{Name: uiName}, &uiSame))
	assert.Equal(t, fp1, uiSame.Annotations[RotationFingerprintAnnotation],
		"same-value re-auth must not change the rotation fingerprint")

	// New access token, SAME credential name → fingerprint changes.
	req.AccessToken = "at-2"
	require.NoError(t, PutOAuthToken(ctx, c, req))
	var ui2 spiceboxv1alpha1.UserIdentity
	require.NoError(t, c.Get(ctx, client.ObjectKey{Name: uiName}, &ui2))
	assert.NotEqual(t, fp1, ui2.Annotations[RotationFingerprintAnnotation],
		"re-authorizing with a new access token (same name) must change the rotation fingerprint")
}

// TestPutOAuthToken_RedemptionMaterialLifecycle covers the sibling Secret over
// a credential's life: a re-link that supplies material updates it in place, a
// re-link that supplies none removes it (a stale sibling would keep presenting
// a client the credential is no longer bound to), and a master written by a
// pre-split build has its co-located copy stripped on the next link — which is
// the migration path off the co-located shape.
func TestPutOAuthToken_RedemptionMaterialLifecycle(t *testing.T) {
	ctx := context.Background()
	c := newOAuthTestClient(t)
	base := PutOAuthTokenRequest{
		Subject:        "user:carol@example.invalid",
		CredentialName: "linear-oauth",
		AccessToken:    "at-1",
		RefreshToken:   "rt-1",
		TokenType:      "Bearer",
		TokenEndpoint:  "https://idp.example.invalid/token",
		ClientID:       "cid-1",
		ClientSecret:   "secret-1",
	}
	masterName := MasterSecretName(NameForSubject(base.Subject), base.CredentialName)
	sibKey := client.ObjectKey{
		Namespace: spiceboxv1alpha1.IdentitiesNamespace,
		Name:      refresh.MaterialSecretName(masterName),
	}
	require.NoError(t, PutOAuthToken(ctx, c, base))

	// Re-link with a rotated DCR client → sibling updated in place.
	base.AccessToken, base.ClientID, base.ClientSecret = "at-2", "cid-2", "secret-2"
	require.NoError(t, PutOAuthToken(ctx, c, base))
	var sib corev1.Secret
	require.NoError(t, c.Get(ctx, sibKey, &sib))
	assert.Equal(t, []byte("cid-2"), sib.Data["client_id"])
	assert.Equal(t, []byte("secret-2"), sib.Data["client_secret"])

	// Simulate a master written by a pre-split build: the redemption keys
	// co-located with the tokens.
	var master corev1.Secret
	masterKey := client.ObjectKey{Namespace: spiceboxv1alpha1.IdentitiesNamespace, Name: masterName}
	require.NoError(t, c.Get(ctx, masterKey, &master))
	master.Data["client_secret"] = []byte("legacy-secret")
	require.NoError(t, c.Update(ctx, &master))

	// A re-link with no client material at all: master is rewritten without the
	// legacy copy, and the now-meaningless sibling is removed.
	require.NoError(t, PutOAuthToken(ctx, c, PutOAuthTokenRequest{
		Subject:        base.Subject,
		CredentialName: base.CredentialName,
		AccessToken:    "at-3",
		TokenType:      "Bearer",
	}))
	require.NoError(t, c.Get(ctx, masterKey, &master))
	assert.NotContains(t, master.Data, "client_secret",
		"re-linking must strip a pre-split co-located copy, not leave it readable")
	assert.True(t, apierrors.IsNotFound(c.Get(ctx, sibKey, &sib)),
		"a sibling for material the credential no longer has must not linger")
}

// TestPutOAuthToken_RefusesToClobberACollidingCredential pins the naming
// hazard the refresh-material label exists for. Credential names are free-form,
// so a user holding both "linear-oauth" and "linear-oauth-refresh" has two
// master Secrets whose names are exactly refresh.MaterialSecretName's input and
// output. Writing the first credential's material over the second's master
// would destroy that credential outright — and the ownerReference would then GC
// it when the first is unlinked. It must be refused, loudly, instead.
func TestPutOAuthToken_RefusesToClobberACollidingCredential(t *testing.T) {
	ctx := context.Background()
	c := newOAuthTestClient(t)
	const subject = "user:dave@example.invalid" // untyped: converts to identity.Subject

	// The credential whose master sits at the colliding name, linked first.
	require.NoError(t, PutOAuthToken(ctx, c, PutOAuthTokenRequest{
		Subject:        subject,
		CredentialName: "linear-oauth-refresh",
		AccessToken:    "collider-at",
		TokenType:      "Bearer",
	}))
	colliding := MasterSecretName(NameForSubject(subject), "linear-oauth-refresh")
	require.Equal(t, colliding, refresh.MaterialSecretName(
		MasterSecretName(NameForSubject(subject), "linear-oauth")),
		"fixture must actually collide, or this test proves nothing")

	err := PutOAuthToken(ctx, c, PutOAuthTokenRequest{
		Subject:        subject,
		CredentialName: "linear-oauth",
		AccessToken:    "at-1",
		RefreshToken:   "rt-1",
		TokenType:      "Bearer",
		TokenEndpoint:  "https://idp.example.invalid/token",
		ClientID:       "cid",
		ClientSecret:   "secret",
	})
	require.Error(t, err, "writing redemption material over another credential's master must be refused")
	assert.Contains(t, err.Error(), colliding, "the error must name the colliding Secret")

	var sec corev1.Secret
	require.NoError(t, c.Get(ctx, client.ObjectKey{
		Namespace: spiceboxv1alpha1.IdentitiesNamespace, Name: colliding,
	}, &sec))
	assert.Equal(t, []byte("collider-at"), sec.Data["access_token"],
		"the colliding credential must be untouched")
}

// TestPutOAuthToken_RefusesAMasterThatIsAnotherCredentialsRefreshMaterial pins
// the OTHER direction of the same naming hazard. The test above links the
// colliding credential first, so the write that must be refused is the sibling
// write; here the ORDER IS REVERSED — "linear-oauth" is linked first, minting
// its sibling — and the write that must be refused is the MASTER write.
//
// Without the symmetric check, step 1 of PutOAuthToken Gets that sibling, sets
// sec.Data and Updates, preserving its Labels and OwnerReferences. The colliding
// credential's master is then labelled as linear-oauth's refresh material and
// GC-owned by linear-oauth's master, and a later re-link of linear-oauth writes
// its redemption material into a Secret the UserIdentity still lists as the
// colliding credential's oauth SecretRef — a Secret oauthMasterSecretNames puts
// in the passthrough runner's Role.
func TestPutOAuthToken_RefusesAMasterThatIsAnotherCredentialsRefreshMaterial(t *testing.T) {
	ctx := context.Background()
	c := newOAuthTestClient(t)
	const subject = "user:erin@example.invalid" // untyped: converts to identity.Subject

	require.NoError(t, PutOAuthToken(ctx, c, PutOAuthTokenRequest{
		Subject:        subject,
		CredentialName: "linear-oauth",
		AccessToken:    "at-1",
		RefreshToken:   "rt-1",
		TokenType:      "Bearer",
		TokenEndpoint:  "https://idp.example.invalid/token",
		ClientID:       "cid",
		ClientSecret:   "secret",
	}))
	sibName := refresh.MaterialSecretName(MasterSecretName(NameForSubject(subject), "linear-oauth"))
	require.Equal(t, sibName, MasterSecretName(NameForSubject(subject), "linear-oauth-refresh"),
		"fixture must actually collide, or this test proves nothing")

	err := PutOAuthToken(ctx, c, PutOAuthTokenRequest{
		Subject:        subject,
		CredentialName: "linear-oauth-refresh",
		AccessToken:    "collider-at",
		TokenType:      "Bearer",
	})
	require.Error(t, err, "writing a credential's tokens over another credential's refresh material must be refused")
	assert.Contains(t, err.Error(), sibName, "the error must name the colliding Secret")

	var sib corev1.Secret
	require.NoError(t, c.Get(ctx, client.ObjectKey{
		Namespace: spiceboxv1alpha1.IdentitiesNamespace, Name: sibName,
	}, &sib))
	assert.Equal(t, []byte("cid"), sib.Data["client_id"],
		"linear-oauth's redemption material must be untouched")
	assert.NotContains(t, sib.Data, "access_token",
		"the refresh material must not have been turned into a token Secret")
}

// TestPutOAuthToken_RepairsAStaleSiblingOwnerReference covers unlink-then-relink
// (identityd's portal Replace, `oap identity delete-token` followed by a fresh
// auth). Deleting the master deletes it by UID; the relink re-Creates it with a
// NEW UID while owner-ref GC has not yet reaped the sibling. The update branch
// must re-point the ownerReference at the live master — leaving the dead UID
// there means GC deletes the material this very call just wrote, and every
// refresh afterwards fails until another re-link.
func TestPutOAuthToken_RepairsAStaleSiblingOwnerReference(t *testing.T) {
	ctx := context.Background()
	const subject = "user:frank@example.invalid" // untyped: converts to identity.Subject
	masterName := MasterSecretName(NameForSubject(subject), "linear-oauth")
	sibName := refresh.MaterialSecretName(masterName)

	// The state a relink races: a master re-created under a new UID, and the
	// previous incarnation's sibling still awaiting GC.
	master := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: masterName, Namespace: spiceboxv1alpha1.IdentitiesNamespace, UID: "uid-live",
		},
		Data: map[string][]byte{"access_token": []byte("at-0")},
	}
	sib := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: sibName, Namespace: spiceboxv1alpha1.IdentitiesNamespace,
			Labels: map[string]string{
				refresh.MaterialSecretLabel: "true",
				adoptguard.AdoptedLabel:     "true",
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "v1", Kind: "Secret", Name: masterName, UID: "uid-reaped",
			}},
		},
		Data: map[string][]byte{"token_endpoint": []byte("https://idp.example.invalid/token")},
	}
	s := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(master, sib).Build()

	require.NoError(t, PutOAuthToken(ctx, c, PutOAuthTokenRequest{
		Subject:        subject,
		CredentialName: "linear-oauth",
		AccessToken:    "at-1",
		RefreshToken:   "rt-1",
		TokenType:      "Bearer",
		TokenEndpoint:  "https://idp.example.invalid/token",
		ClientID:       "cid",
	}))

	var got corev1.Secret
	require.NoError(t, c.Get(ctx, client.ObjectKey{
		Namespace: spiceboxv1alpha1.IdentitiesNamespace, Name: sibName,
	}, &got))
	require.Len(t, got.OwnerReferences, 1)
	assert.Equal(t, types.UID("uid-live"), got.OwnerReferences[0].UID,
		"a sibling owned by a reaped master UID is deleted by GC together with the material just written")
}

package agentidentity

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	// Registers the static/oauth/federated credkind Kinds so
	// credresolve.SourceFor's registry.Get(cred.Type) dispatch resolves in
	// this package's tests (Resolve calls it for type=static).
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/imports"
)

const (
	testNS      = "tenant-a"
	testID      = "billing-bot-id"
	testCred    = "billing-api-key"
	testSecret  = "billing-bot-creds"
	testKey     = "apiKey"
	testCurrent = "sk_dead"
)

func newClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s))
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

// identityWith builds an AgentIdentity declaring one credential of the given
// type. cred.Static is populated only for type=static.
func identityWith(credType string) *spiceboxv1alpha1.AgentIdentity {
	cred := spiceboxv1alpha1.AgentCredential{Name: testCred, Type: credType}
	switch credType {
	case "static":
		cred.Static = &spiceboxv1alpha1.StaticCredentialSource{
			SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: testSecret, Key: testKey},
		}
	case "oauth":
		cred.OAuth = &spiceboxv1alpha1.OAuthCredentialSource{
			SecretRef: spiceboxv1alpha1.SecretRef{Name: testSecret},
		}
	}
	return &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: testID, Namespace: testNS},
		Spec:       spiceboxv1alpha1.AgentIdentitySpec{Credentials: []spiceboxv1alpha1.AgentCredential{cred}},
	}
}

func backingSecret(value string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testSecret, Namespace: testNS},
		Data:       map[string][]byte{testKey: []byte(value)},
	}
}

func TestPutToken_WritesTheBackingSecret(t *testing.T) {
	c := newClient(t, identityWith("static"), backingSecret(testCurrent))

	target, err := PutToken(context.Background(), c, PutTokenRequest{
		Namespace: testNS, Name: testID, CredentialName: testCred, Token: "sk_fresh",
		ExpectSecret: &spiceboxv1alpha1.NamespacedRef{Namespace: testNS, Name: testSecret},
	})
	require.NoError(t, err)
	assert.Equal(t, testNS, target.SecretRef.Namespace)
	assert.Equal(t, testSecret, target.SecretRef.Name)
	assert.Equal(t, testKey, target.Key)

	var sec corev1.Secret
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: testSecret}, &sec))
	assert.Equal(t, "sk_fresh", string(sec.Data[testKey]),
		"the replacement value must land under the credential's own key")
}

// TestPutToken_LeavesSiblingKeysAlone — a backing Secret may hold more than one
// credential's value. Replacing one must not blank the others.
func TestPutToken_LeavesSiblingKeysAlone(t *testing.T) {
	sec := backingSecret(testCurrent)
	sec.Data["otherKey"] = []byte("untouched")
	c := newClient(t, identityWith("static"), sec)

	_, err := PutToken(context.Background(), c, PutTokenRequest{
		Namespace: testNS, Name: testID, CredentialName: testCred, Token: "sk_fresh",
	})
	require.NoError(t, err)

	var got corev1.Secret
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: testSecret}, &got))
	assert.Equal(t, "untouched", string(got.Data["otherKey"]))
}

// TestPutToken_RefusalsLeaveTheSecretUntouched enumerates every fail-closed
// branch. Each case asserts BOTH the typed error and that the stored value did
// not move: a refusal that still wrote would be the worst of both worlds.
func TestPutToken_RefusalsLeaveTheSecretUntouched(t *testing.T) {
	cases := []struct {
		name    string
		objs    []client.Object
		req     PutTokenRequest
		wantErr error
	}{
		{
			name:    "no AgentIdentity: ErrIdentityNotFound",
			objs:    []client.Object{backingSecret(testCurrent)},
			req:     PutTokenRequest{Namespace: testNS, Name: testID, CredentialName: testCred, Token: "x"},
			wantErr: ErrIdentityNotFound,
		},
		{
			name:    "credential not declared: ErrCredentialMissing",
			objs:    []client.Object{identityWith("static"), backingSecret(testCurrent)},
			req:     PutTokenRequest{Namespace: testNS, Name: testID, CredentialName: "not-a-credential", Token: "x"},
			wantErr: ErrCredentialMissing,
		},
		{
			name:    "type=oauth cannot take a pasted value: ErrNotReplaceable",
			objs:    []client.Object{identityWith("oauth"), backingSecret(testCurrent)},
			req:     PutTokenRequest{Namespace: testNS, Name: testID, CredentialName: testCred, Token: "x"},
			wantErr: ErrNotReplaceable,
		},
		{
			name: "recorded Secret is not the one it resolves to: ErrSecretRefMismatch",
			objs: []client.Object{identityWith("static"), backingSecret(testCurrent)},
			req: PutTokenRequest{
				Namespace: testNS, Name: testID, CredentialName: testCred, Token: "x",
				ExpectSecret: &spiceboxv1alpha1.NamespacedRef{Namespace: testNS, Name: "some-other-secret"},
			},
			wantErr: ErrSecretRefMismatch,
		},
		{
			name:    "backing Secret absent: ErrSecretMissing",
			objs:    []client.Object{identityWith("static")},
			req:     PutTokenRequest{Namespace: testNS, Name: testID, CredentialName: testCred, Token: "x"},
			wantErr: ErrSecretMissing,
		},
		{
			name:    "same value re-pasted: ErrValueUnchanged",
			objs:    []client.Object{identityWith("static"), backingSecret(testCurrent)},
			req:     PutTokenRequest{Namespace: testNS, Name: testID, CredentialName: testCred, Token: testCurrent},
			wantErr: ErrValueUnchanged,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newClient(t, tc.objs...)
			_, err := PutToken(context.Background(), c, tc.req)
			require.ErrorIs(t, err, tc.wantErr)

			var sec corev1.Secret
			if getErr := c.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: testSecret}, &sec); getErr == nil {
				assert.Equal(t, testCurrent, string(sec.Data[testKey]),
					"a refused write must leave the stored value exactly as it was")
			}
		})
	}
}

// TestPutToken_UnregisteredCredentialType_SurfacesRegistryError is the
// R15-shaped case for the `cred.Type != "static"` → registry-dispatch
// migration in Resolve: for oauth (see the table case above) both the old
// literal comparison and the new credkindregistry.Get dispatch land on the
// SAME sentinel, ErrNotReplaceable, with the SAME generic "is type=oauth"
// text — a test asserting only errors.Is would pass identically before and
// after this migration. An UNREGISTERED type is where they diverge: the old
// code produced the same generic "is type=%s" text for ANY non-static type,
// while the new code fails at credkindregistry.Get itself and wraps ITS
// message — proof this path actually dispatches through the registry.
func TestPutToken_UnregisteredCredentialType_SurfacesRegistryError(t *testing.T) {
	c := newClient(t, identityWith("nosuch"), backingSecret(testCurrent))

	_, err := PutToken(context.Background(), c, PutTokenRequest{
		Namespace: testNS, Name: testID, CredentialName: testCred, Token: "x",
	})
	require.ErrorIs(t, err, ErrNotReplaceable)
	assert.Contains(t, err.Error(), "unknown credential type",
		"an unregistered type must surface the registry's own diagnosis, not the generic is-type=%s text")
}

// TestResolve_MatchesTheReconcilersOwnDerivation — the whole point of routing
// through credresolve.SourceFor is that Resolve names the SAME Secret+key the
// CredentialUpdateRequest reconciler recorded. Two independent derivations is
// how a write ends up on a Secret nothing is watching.
func TestResolve_MatchesTheReconcilersOwnDerivation(t *testing.T) {
	c := newClient(t, identityWith("static"), backingSecret(testCurrent))
	target, err := Resolve(context.Background(), c, testNS, testID, testCred)
	require.NoError(t, err)
	assert.Equal(t, testNS, target.SecretRef.Namespace, "an AgentIdentity's static credential resolves in its OWN namespace")
	assert.Equal(t, testSecret, target.SecretRef.Name)
	assert.Equal(t, testKey, target.Key)
}

func TestPutToken_RequiresAToken(t *testing.T) {
	c := newClient(t, identityWith("static"), backingSecret(testCurrent))
	_, err := PutToken(context.Background(), c, PutTokenRequest{
		Namespace: testNS, Name: testID, CredentialName: testCred,
	})
	require.Error(t, err, "an empty token must never reach the Secret")
}

// --- PutOAuthToken ---

const (
	oauthNS     = "ws-abc123"
	oauthID     = "weather-ai"
	oauthCred   = "svc_oauth"
	oauthSecret = "weather-ai-svc-oauth"
)

// oauthIdentity builds an AgentIdentity in oauthNS declaring one type=oauth
// credential named oauthCred, whose SecretRef names oauthSecret — a Secret
// that does NOT yet exist. PutOAuthToken must get-or-create it: this is the
// shape a builder pre-declares before the OAuth dance has run even once.
func oauthIdentity() *spiceboxv1alpha1.AgentIdentity {
	return &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: oauthID, Namespace: oauthNS},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{Credentials: []spiceboxv1alpha1.AgentCredential{
			{
				Name: oauthCred,
				Type: "oauth",
				OAuth: &spiceboxv1alpha1.OAuthCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretRef{Name: oauthSecret},
				},
			},
		}},
	}
}

func TestPutOAuthToken_WritesBundleOntoDeclaredOAuthCredential(t *testing.T) {
	c := newClient(t, oauthIdentity())
	ctx := context.Background()
	exp := time.Now().Add(time.Hour).Unix()

	ref, err := PutOAuthToken(ctx, c, PutOAuthTokenRequest{
		Namespace: oauthNS, Name: oauthID, CredentialName: oauthCred,
		AccessToken:   "at_123",
		RefreshToken:  "rt_456",
		TokenEndpoint: "https://provider.example/token",
		ClientID:      "cid_789",
		ClientSecret:  "csec_abc",
		Scope:         "read write",
		TokenType:     "Bearer",
		ExpiresAt:     exp,
	})
	require.NoError(t, err)
	assert.Equal(t, oauthNS, ref.Namespace)
	assert.Equal(t, oauthSecret, ref.Name, "PutOAuthToken must return the credential's own SecretRef, not a name it invented")

	var sec corev1.Secret
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: oauthNS, Name: oauthSecret}, &sec))
	assert.Equal(t, "at_123", string(sec.Data["access_token"]))
	assert.Equal(t, "rt_456", string(sec.Data["refresh_token"]))
	assert.Equal(t, "https://provider.example/token", string(sec.Data["token_endpoint"]))
	assert.Equal(t, "cid_789", string(sec.Data["client_id"]))
	assert.Equal(t, "csec_abc", string(sec.Data["client_secret"]))
	assert.Equal(t, "read write", string(sec.Data["scope"]))
	assert.Equal(t, "Bearer", string(sec.Data["token_type"]))
	assert.Equal(t, time.Unix(exp, 0).UTC().Format(time.RFC3339), string(sec.Data["expires_at"]))
}

// TestPutOAuthToken_RefusesMissingOrNonOAuthCredential enumerates the
// fail-closed identity/credential lookups. Each case asserts BOTH the typed
// error and that no Secret was created anywhere in the target namespace — a
// refusal that still wrote would be the worst of both worlds.
func TestPutOAuthToken_RefusesMissingOrNonOAuthCredential(t *testing.T) {
	cases := []struct {
		name    string
		objs    []client.Object
		req     PutOAuthTokenRequest
		wantErr error
	}{
		{
			name: "AgentIdentity does not exist",
			objs: nil,
			req: PutOAuthTokenRequest{
				Namespace: oauthNS, Name: oauthID, CredentialName: oauthCred, AccessToken: "at",
			},
			wantErr: ErrIdentityNotFound,
		},
		{
			name: "credential not declared",
			objs: []client.Object{oauthIdentity()},
			req: PutOAuthTokenRequest{
				Namespace: oauthNS, Name: oauthID, CredentialName: "not-a-credential", AccessToken: "at",
			},
			wantErr: ErrCredentialMissing,
		},
		{
			name: "credential is type=static, not oauth",
			objs: []client.Object{identityWith("static")},
			req: PutOAuthTokenRequest{
				Namespace: testNS, Name: testID, CredentialName: testCred, AccessToken: "at",
			},
			wantErr: ErrNotOAuthCredential,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newClient(t, tc.objs...)
			_, err := PutOAuthToken(context.Background(), c, tc.req)
			require.ErrorIs(t, err, tc.wantErr)

			var secList corev1.SecretList
			require.NoError(t, c.List(context.Background(), &secList, client.InNamespace(tc.req.Namespace)))
			assert.Empty(t, secList.Items, "a refused write must never create a Secret")
		})
	}
}

// TestPutOAuthToken_ReconnectDropsStaleRedemptionKeys is the core divergence
// from setup.Store's writeValueIntoSecret: a re-connect that returns fewer
// keys must leave NONE of the previous ones behind, because a stale
// refresh_token or client_secret co-located here is live redemption material
// pkg/platform/identity/refresh.Run would happily redeem with.
func TestPutOAuthToken_ReconnectDropsStaleRedemptionKeys(t *testing.T) {
	c := newClient(t, oauthIdentity())
	ctx := context.Background()

	_, err := PutOAuthToken(ctx, c, PutOAuthTokenRequest{
		Namespace: oauthNS, Name: oauthID, CredentialName: oauthCred,
		AccessToken:   "at_1",
		RefreshToken:  "rt_1",
		TokenEndpoint: "https://provider.example/token",
		ClientSecret:  "csec_1",
	})
	require.NoError(t, err)

	// Reconnect without a refresh token, token endpoint, or client secret.
	_, err = PutOAuthToken(ctx, c, PutOAuthTokenRequest{
		Namespace: oauthNS, Name: oauthID, CredentialName: oauthCred,
		AccessToken: "at_2",
	})
	require.NoError(t, err)

	var sec corev1.Secret
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: oauthNS, Name: oauthSecret}, &sec))
	assert.Equal(t, "at_2", string(sec.Data["access_token"]))
	_, hasRefresh := sec.Data["refresh_token"]
	assert.False(t, hasRefresh, "a reconnect without a refresh token must drop the stale one")
	_, hasEndpoint := sec.Data["token_endpoint"]
	assert.False(t, hasEndpoint, "a reconnect without a token endpoint must drop the stale one")
	_, hasClientSecret := sec.Data["client_secret"]
	assert.False(t, hasClientSecret, "a reconnect without a client secret must drop the stale one")
}

func TestPutOAuthToken_RefusesRefreshTokenWithoutTokenEndpoint(t *testing.T) {
	c := newClient(t, oauthIdentity())
	_, err := PutOAuthToken(context.Background(), c, PutOAuthTokenRequest{
		Namespace: oauthNS, Name: oauthID, CredentialName: oauthCred,
		AccessToken:  "at",
		RefreshToken: "rt",
	})
	require.Error(t, err, "a refreshToken with no tokenEndpoint could never be refreshed")

	var sec corev1.Secret
	getErr := c.Get(context.Background(), client.ObjectKey{Namespace: oauthNS, Name: oauthSecret}, &sec)
	assert.True(t, apierrors.IsNotFound(getErr), "a refused write must never create the Secret")
}

func TestPutOAuthToken_RequiresAccessToken(t *testing.T) {
	c := newClient(t, oauthIdentity())
	_, err := PutOAuthToken(context.Background(), c, PutOAuthTokenRequest{
		Namespace: oauthNS, Name: oauthID, CredentialName: oauthCred,
	})
	require.Error(t, err, "an empty access token must never reach the Secret")
}

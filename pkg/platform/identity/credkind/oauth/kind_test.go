package oauth

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credresolve"
)

func TestKind_Facts(t *testing.T) {
	k := Kind{}
	assert.Equal(t, "oauth", k.Type())
	assert.False(t, k.Minted(), "the token is stored in a Secret, not minted per use")
	assert.True(t, k.NeedsRefresh(),
		"an oauth access token expires and is renewed out of band by the refresh controller")
	assert.ElementsMatch(t,
		[]credkind.Scope{
			credkind.ScopeAgentIdentity,
			credkind.ScopeUserIdentity,
			credkind.ScopeSessionUserIdentity,
		},
		k.ValidOn())
}

func TestValidateSpec(t *testing.T) {
	cases := []struct {
		name    string
		cred    spiceboxv1alpha1.AgentCredential
		wantErr string
	}{
		{
			name:    "missing oauth block: rejected",
			cred:    spiceboxv1alpha1.AgentCredential{Name: "c", Type: "oauth"},
			wantErr: "secretRef.name required",
		},
		{
			name: "empty secret name: rejected",
			cred: spiceboxv1alpha1.AgentCredential{Name: "c", Type: "oauth",
				OAuth: &spiceboxv1alpha1.OAuthCredentialSource{}},
			wantErr: "secretRef.name required",
		},
		{
			name: "well-formed: accepted",
			cred: spiceboxv1alpha1.AgentCredential{Name: "c", Type: "oauth",
				OAuth: &spiceboxv1alpha1.OAuthCredentialSource{SecretRef: spiceboxv1alpha1.SecretRef{Name: "s"}}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Kind{}.ValidateSpec(tc.cred)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestSecretRef_ReportsNoKeyBecauseTheShapeIsFixed(t *testing.T) {
	built, err := Kind{}.BuildCredential("hubspot", "demo-agent-hubspot", "ignored")
	require.NoError(t, err)
	assert.Equal(t, "oauth", built.Type)

	ref := Kind{}.SecretRef(built)
	require.NotNil(t, ref)
	assert.Equal(t, "demo-agent-hubspot", ref.Name)
	assert.Empty(t, ref.Key,
		"the oauth Secret has a fixed multi-key shape (access_token, refresh_token, expires_at, ...), "+
			"not one named key — an empty Key is how a caller learns there is no single key to check")
}

func TestBuildCredential_IgnoresTheKeyArgument(t *testing.T) {
	withKey, err := Kind{}.BuildCredential("c", "s", "some-key")
	require.NoError(t, err)
	withoutKey, err := Kind{}.BuildCredential("c", "s", "")
	require.NoError(t, err)
	assert.Equal(t, withoutKey, withKey,
		"an oauth credential has no single key, so passing one must not change the result")
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s), "AddToScheme")
	require.NoError(t, corev1.AddToScheme(s), "corev1 AddToScheme")
	return s
}

func oauthCred(name, secret string) spiceboxv1alpha1.AgentCredential {
	return spiceboxv1alpha1.AgentCredential{Name: name, Type: "oauth",
		OAuth: &spiceboxv1alpha1.OAuthCredentialSource{SecretRef: spiceboxv1alpha1.SecretRef{Name: secret}}}
}

// TestReadStoredValue covers ReadStoredValue's former life as
// credresolve.resolveOAuth: the expiry gate (future/absent/past expires_at),
// the credresolve sentinel errors for a missing/empty access_token, and a nil
// oauth block.
func TestReadStoredValue(t *testing.T) {
	future := time.Now().Add(time.Hour).Format(time.RFC3339)
	past := time.Now().Add(-time.Hour).Format(time.RFC3339)

	cases := []struct {
		name       string
		secretName string
		secretData map[string][]byte
		wantErr    error
		wantVal    string
	}{
		{name: "future expires_at: access_token returned", secretName: "s", secretData: map[string][]byte{"access_token": []byte("at-1"), "expires_at": []byte(future)}, wantVal: "at-1"},
		{name: "no expires_at: treated as long-lived", secretName: "s", secretData: map[string][]byte{"access_token": []byte("at-1")}, wantVal: "at-1"},
		{name: "past expires_at: ErrExpired", secretName: "s", secretData: map[string][]byte{"access_token": []byte("at-1"), "expires_at": []byte(past)}, wantErr: credresolve.ErrExpired},
		{name: "empty access_token: ErrSecretValueEmpty", secretName: "s", secretData: map[string][]byte{"access_token": []byte("")}, wantErr: credresolve.ErrSecretValueEmpty},
		{name: "absent access_token: ErrSecretKeyMissing", secretName: "s", secretData: map[string][]byte{"other": []byte("x")}, wantErr: credresolve.ErrSecretKeyMissing},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: tc.secretName, Namespace: "default"}, Data: tc.secretData}
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(sec).Build()
			got, err := Kind{}.ReadStoredValue(context.Background(), c, "default", oauthCred("c", tc.secretName))
			if tc.wantErr != nil {
				assert.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantVal, string(got.UnderlyingValue()))
		})
	}
	t.Run("nil oauth block: local error, not a panic", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
		_, err := Kind{}.ReadStoredValue(context.Background(), c, "default", spiceboxv1alpha1.AgentCredential{Name: "c", Type: "oauth"})
		require.Error(t, err, "a nil oauth block must error, not panic on a nil dereference")
		assert.Contains(t, err.Error(), "oauth block is nil")
	})
}

// TestResolve_UnexpiredToken_ReadsThroughReadStoredValue proves Resolve reads
// through its own ReadStoredValue (not credresolve.ResolveSecretValue) for
// the non-expired case, without needing the JIT-refresh HTTP round trip
// (already covered end-to-end by TestBroker_ExpiredOAuth_JITRefresh in
// pkg/platform/identity/broker/inproc).
func TestResolve_UnexpiredToken_ReadsThroughReadStoredValue(t *testing.T) {
	future := time.Now().Add(time.Hour).Format(time.RFC3339)
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"},
		Data: map[string][]byte{"access_token": []byte("at-1"), "expires_at": []byte(future)}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(sec).Build()

	got, exp, err := Kind{}.Resolve(context.Background(), credkind.Deps{Client: c},
		spiceboxv1alpha1.CredentialSource{Type: "oauth", Namespace: "default", Name: "s"})
	require.NoError(t, err)
	assert.Equal(t, "at-1", string(got.AccessToken.UnderlyingValue()))
	assert.True(t, exp.IsZero(), "the value's own expires_at is enforced by ReadStoredValue; the broker bounds the cache entry")
}

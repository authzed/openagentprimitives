package credresolve_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	// Registers the static/oauth/federated credkind Kinds so
	// ResolveSecretValue's registry.Get(cred.Type) dispatch resolves in this
	// package's tests. This is the external (credresolve_test) test package, so
	// importing back into credkind (which imports credresolve) is NOT a cycle —
	// it links into the same `go test` binary as descriptors_test.go (package
	// credresolve), so that file's tests share this one registration too.
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/imports"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credresolve"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s), "AddToScheme")
	require.NoError(t, corev1.AddToScheme(s), "corev1 AddToScheme")
	return s
}

func secretWith(name, key, val string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Data:       map[string][]byte{key: []byte(val)},
	}
}

func TestResolveSecretValue(t *testing.T) {
	staticCred := func(secret, key string) spiceboxv1alpha1.AgentCredential {
		return spiceboxv1alpha1.AgentCredential{Name: "c", Type: "static",
			Static: &spiceboxv1alpha1.StaticCredentialSource{SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: secret, Key: key}}}
	}
	cases := []struct {
		name    string
		secret  *corev1.Secret
		cred    spiceboxv1alpha1.AgentCredential
		wantErr error
		wantVal string
	}{
		{name: "static present: value returned", secret: secretWith("s", "token", "abc"), cred: staticCred("s", "token"), wantVal: "abc"},
		{name: "static empty: ErrSecretValueEmpty", secret: secretWith("s", "token", ""), cred: staticCred("s", "token"), wantErr: credresolve.ErrSecretValueEmpty},
		{name: "static key missing: ErrSecretKeyMissing", secret: secretWith("s", "other", "x"), cred: staticCred("s", "token"), wantErr: credresolve.ErrSecretKeyMissing},
		{name: "static secret missing: ErrSecretMissing", secret: nil, cred: staticCred("s", "token"), wantErr: credresolve.ErrSecretMissing},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objs := []client.Object{}
			if tc.secret != nil {
				objs = append(objs, tc.secret)
			}
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
			got, err := credresolve.ResolveSecretValue(context.Background(), c, "default", tc.cred)
			if tc.wantErr != nil {
				assert.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantVal, string(got.UnderlyingValue()))
		})
	}
}

// TestResolveSecretValue_OAuth covers the OAuth resolution paths: secret with
// an expiry in the future, secret with no expires_at (treated as long-lived),
// secret past expiry (returns ErrExpired), present-but-empty access_token
// (ErrSecretValueEmpty), and absent access_token (ErrSecretKeyMissing).
func TestResolveSecretValue_OAuth(t *testing.T) {
	future := time.Now().Add(time.Hour).Format(time.RFC3339)
	past := time.Now().Add(-time.Hour).Format(time.RFC3339)

	oauthCred := func(secret string) spiceboxv1alpha1.AgentCredential {
		return spiceboxv1alpha1.AgentCredential{Name: "c", Type: "oauth",
			OAuth: &spiceboxv1alpha1.OAuthCredentialSource{SecretRef: spiceboxv1alpha1.SecretRef{Name: secret}}}
	}

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
			sec := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: tc.secretName, Namespace: "default"},
				Data:       tc.secretData,
			}
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(sec).Build()
			got, err := credresolve.ResolveSecretValue(context.Background(), c, "default", oauthCred(tc.secretName))
			if tc.wantErr != nil {
				assert.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantVal, string(got.UnderlyingValue()))
		})
	}
}

// TestResolveSecretValue_UnregisteredType pins the fail-closed contract
// registry.Get gives ResolveSecretValue: seven production callers reach this
// function, and every one of them relies on an unregistered type erroring
// here rather than silently resolving to a zero SensitiveValue or falling
// back to some default type's shape.
func TestResolveSecretValue_UnregisteredType(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	_, err := credresolve.ResolveSecretValue(context.Background(), c, "default",
		spiceboxv1alpha1.AgentCredential{Name: "c", Type: "bogus-unregistered-type"})
	require.Error(t, err, "an unregistered credential type must fail closed, never resolve to a zero value")
	assert.Contains(t, err.Error(), "bogus-unregistered-type")
}

// TestAgentCredentialFromSource covers the registry-dispatched
// (name, secretName, secretKey) → AgentCredential inverse for each
// value-resolvable type, plus the two cases that must error rather than
// guess a shape: type=federated (minted, never read this way — its own
// BuildCredential always errors) and an unregistered type (registry.Get
// itself errors). Silently substituting a static-shaped credential for
// either case previously turned a missing-registration bug into a confusing
// "secret key missing" error pointing at the wrong thing; erroring here
// points straight at the real cause.
func TestAgentCredentialFromSource(t *testing.T) {
	cases := []struct {
		name       string
		src        spiceboxv1alpha1.CredentialSource
		want       spiceboxv1alpha1.AgentCredential
		wantErrStr string
	}{
		{
			name: "type=static: static shape with name+key",
			src:  spiceboxv1alpha1.CredentialSource{Type: "static", Name: "s", Key: "k"},
			want: spiceboxv1alpha1.AgentCredential{Name: "s", Type: "static",
				Static: &spiceboxv1alpha1.StaticCredentialSource{SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "s", Key: "k"}}},
		},
		{
			name: "type=oauth: oauth shape, key ignored",
			src:  spiceboxv1alpha1.CredentialSource{Type: "oauth", Name: "s", Key: "ignored"},
			want: spiceboxv1alpha1.AgentCredential{Name: "s", Type: "oauth",
				OAuth: &spiceboxv1alpha1.OAuthCredentialSource{SecretRef: spiceboxv1alpha1.SecretRef{Name: "s"}}},
		},
		{
			name:       "type=federated: errors (minted, not value-resolvable this way)",
			src:        spiceboxv1alpha1.CredentialSource{Type: "federated", Name: "idp-secret", Key: "k"},
			wantErrStr: "minted on demand",
		},
		{
			name:       "unregistered type: errors, never silently guesses static",
			src:        spiceboxv1alpha1.CredentialSource{Type: "nosuch", Name: "s", Key: "k"},
			wantErrStr: "unknown credential type",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := credresolve.AgentCredentialFromSource(tc.src)
			if tc.wantErrStr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErrStr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

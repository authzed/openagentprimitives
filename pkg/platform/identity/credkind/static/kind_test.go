package static

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credresolve"
)

func TestKind_Facts(t *testing.T) {
	k := Kind{}
	assert.Equal(t, "static", k.Type())
	assert.False(t, k.Minted(), "a static value is stored, not minted")
	assert.False(t, k.NeedsRefresh(), "a static value never expires")
	assert.ElementsMatch(t,
		[]credkind.Scope{
			credkind.ScopeAgentIdentity,
			credkind.ScopeUserIdentity,
			credkind.ScopeSessionUserIdentity,
		},
		k.ValidOn(), "static is valid on every identity CR")
}

func TestValidateSpec(t *testing.T) {
	cases := []struct {
		name    string
		cred    spiceboxv1alpha1.AgentCredential
		wantErr string
	}{
		{
			name:    "missing static block: rejected",
			cred:    spiceboxv1alpha1.AgentCredential{Name: "c", Type: "static"},
			wantErr: "secretRef must set both name and key",
		},
		{
			name: "missing key: rejected",
			cred: spiceboxv1alpha1.AgentCredential{Name: "c", Type: "static",
				Static: &spiceboxv1alpha1.StaticCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "s"}}},
			wantErr: "secretRef must set both name and key",
		},
		{
			name: "well-formed: accepted",
			cred: spiceboxv1alpha1.AgentCredential{Name: "c", Type: "static",
				Static: &spiceboxv1alpha1.StaticCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "s", Key: "k"}}},
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
			assert.Contains(t, err.Error(), "c", "the message must name the offending credential")
		})
	}
}

func TestSecretRefAndBuildCredential_RoundTrip(t *testing.T) {
	built, err := Kind{}.BuildCredential("gh", "demo-agent-gh", "token")
	require.NoError(t, err, "static is writable by the setup flow")
	assert.Equal(t, "static", built.Type)
	assert.Equal(t, "gh", built.Name)

	ref := Kind{}.SecretRef(built)
	require.NotNil(t, ref, "static has a backing Secret")
	assert.Equal(t, "demo-agent-gh", ref.Name)
	assert.Equal(t, "token", ref.Key,
		"static stores its value under one named key, so callers can key-check it")
}

func TestSecretRef_NilWhenBlockAbsent(t *testing.T) {
	assert.Nil(t, Kind{}.SecretRef(spiceboxv1alpha1.AgentCredential{Name: "c", Type: "static"}),
		"a malformed credential has no Secret to point at; validation reports it, SecretRef does not guess")
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s), "AddToScheme")
	require.NoError(t, corev1.AddToScheme(s), "corev1 AddToScheme")
	return s
}

func staticCred(name, secret, key string) spiceboxv1alpha1.AgentCredential {
	return spiceboxv1alpha1.AgentCredential{Name: name, Type: "static",
		Static: &spiceboxv1alpha1.StaticCredentialSource{SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: secret, Key: key}}}
}

// TestReadStoredValue covers ReadStoredValue's former life as
// credresolve.resolveStatic: present → value, empty/missing key/missing
// secret → the credresolve sentinel errors, nil static block → a local error.
func TestReadStoredValue(t *testing.T) {
	cases := []struct {
		name    string
		cred    spiceboxv1alpha1.AgentCredential
		secret  *corev1.Secret
		wantErr error
		wantVal string
	}{
		{
			name:    "present non-empty: value returned",
			cred:    staticCred("c", "s", "token"),
			secret:  &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"}, Data: map[string][]byte{"token": []byte("abc")}},
			wantVal: "abc",
		},
		{
			name:    "present empty: ErrSecretValueEmpty",
			cred:    staticCred("c", "s", "token"),
			secret:  &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"}, Data: map[string][]byte{"token": []byte("")}},
			wantErr: credresolve.ErrSecretValueEmpty,
		},
		{
			name:    "key missing: ErrSecretKeyMissing",
			cred:    staticCred("c", "s", "token"),
			secret:  &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"}, Data: map[string][]byte{"other": []byte("x")}},
			wantErr: credresolve.ErrSecretKeyMissing,
		},
		{
			name:    "secret missing: ErrSecretMissing",
			cred:    staticCred("c", "s", "token"),
			secret:  nil,
			wantErr: credresolve.ErrSecretMissing,
		},
		{
			name:    "nil static block: local error, not a panic",
			cred:    spiceboxv1alpha1.AgentCredential{Name: "c", Type: "static"},
			wantErr: nil, // checked via ErrorContains below, not errors.Is
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objs := []client.Object{}
			if tc.secret != nil {
				objs = append(objs, tc.secret)
			}
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
			got, err := Kind{}.ReadStoredValue(context.Background(), c, "default", tc.cred)
			if tc.cred.Static == nil {
				require.Error(t, err, "a nil static block must error, not panic on a nil dereference")
				assert.Contains(t, err.Error(), "static block is nil")
				return
			}
			if tc.wantErr != nil {
				assert.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantVal, string(got.UnderlyingValue()))
		})
	}
}

// TestResolve_DelegatesToReadStoredValue proves Resolve reads through its own
// ReadStoredValue (not credresolve.ResolveSecretValue) and returns the zero
// expiry — static never expires, so the broker bounds the cache entry itself.
func TestResolve_DelegatesToReadStoredValue(t *testing.T) {
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"}, Data: map[string][]byte{"token": []byte("abc")}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(sec).Build()

	got, exp, err := Kind{}.Resolve(context.Background(), credkind.Deps{Client: c},
		spiceboxv1alpha1.CredentialSource{Type: "static", Namespace: "default", Name: "s", Key: "token"})
	require.NoError(t, err)
	assert.Equal(t, "abc", string(got.AccessToken.UnderlyingValue()))
	assert.True(t, exp.IsZero(), "static carries no expiry; the broker bounds the cache entry itself")
}

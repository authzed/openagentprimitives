package githubapp

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credresolve"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/sensitive"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s), "AddToScheme")
	require.NoError(t, corev1.AddToScheme(s), "corev1 AddToScheme")
	return s
}

// fakeDepsMinter satisfies credkind.GitHubAppMinter (the flattened shape
// Deps.GitHubApp carries), recording exactly what Kind.Resolve extracted
// from the Secret and handing back a scripted mint result. It does NOT
// exercise Adapt or the real Minter interface — that translation is covered
// by deps_test.go.
type fakeDepsMinter struct {
	calls int
	// gotAppID/gotPrivateKeyPEM/gotInstallationID are the arguments from the
	// most recent call, so a test can assert Resolve read the right Secret
	// keys into the right positional argument.
	gotAppID          string
	gotPrivateKeyPEM  []byte
	gotInstallationID string
	token             sensitive.SensitiveValue
	exp               time.Time
	err               error
}

func (f *fakeDepsMinter) Mint(_ context.Context, appID string, privateKeyPEM []byte, installationID string) (sensitive.SensitiveValue, time.Time, error) {
	f.calls++
	f.gotAppID, f.gotPrivateKeyPEM, f.gotInstallationID = appID, privateKeyPEM, installationID
	return f.token, f.exp, f.err
}

var _ credkind.GitHubAppMinter = (*fakeDepsMinter)(nil)

func githubAppCred(name, secretName string) spiceboxv1alpha1.AgentCredential {
	return spiceboxv1alpha1.AgentCredential{
		Name: name, Type: "githubApp",
		GitHubApp: &spiceboxv1alpha1.GitHubAppCredentialSource{
			SecretRef: spiceboxv1alpha1.SecretRef{Name: secretName},
		},
	}
}

func TestKind_Facts(t *testing.T) {
	k := Kind{}
	assert.Equal(t, "githubApp", k.Type())
	assert.True(t, k.Minted(), "a fresh installation token is minted per resolve")
	assert.False(t, k.NeedsRefresh(), "nothing stored means nothing to refresh")
	assert.False(t, k.Projectable(), "the Secret is multi-key App material, not a single projectable value")
	assert.NotEmpty(t, k.DisplayName())
}

// TestValidOn_AgentIdentityOnly is the exact inverse of federated's
// SessionUserIdentity-only ValidOn: a GitHub App has no human subject at
// all, so a user-scoped identity could never own one.
func TestValidOn_AgentIdentityOnly(t *testing.T) {
	assert.True(t, credkind.ValidOnScope(Kind{}, credkind.ScopeAgentIdentity))
	assert.False(t, credkind.ValidOnScope(Kind{}, credkind.ScopeUserIdentity))
	assert.False(t, credkind.ValidOnScope(Kind{}, credkind.ScopeSessionUserIdentity))
}

func TestRequiredSecretKeys(t *testing.T) {
	assert.Nil(t, Kind{}.RequiredSecretKeys(spiceboxv1alpha1.AgentCredential{Type: "githubApp"}),
		"nil block must fail closed to no requirement, not a nil dereference")

	got := Kind{}.RequiredSecretKeys(githubAppCred("demo-gh-cred", "demo-gh-secret"))
	assert.ElementsMatch(t, []string{"app-id", "private-key", "installation-id"}, got,
		"exactly the three minting keys -- webhook-secret is consumed by the channel kind's "+
			"webhook verification, not by credential resolution, so it must NOT appear here")
}

func TestValidateSpec(t *testing.T) {
	cases := []struct {
		name    string
		cred    spiceboxv1alpha1.AgentCredential
		wantErr string
	}{
		{
			name:    "missing githubApp block: rejected",
			cred:    spiceboxv1alpha1.AgentCredential{Name: "demo-gh-cred", Type: "githubApp"},
			wantErr: "secretRef.name required",
		},
		{
			name: "empty secretRef.name: rejected",
			cred: spiceboxv1alpha1.AgentCredential{Name: "demo-gh-cred", Type: "githubApp",
				GitHubApp: &spiceboxv1alpha1.GitHubAppCredentialSource{}},
			wantErr: "secretRef.name required",
		},
		{
			name: "well-formed: accepted",
			cred: spiceboxv1alpha1.AgentCredential{Name: "demo-gh-cred", Type: "githubApp",
				GitHubApp: &spiceboxv1alpha1.GitHubAppCredentialSource{SecretRef: spiceboxv1alpha1.SecretRef{Name: "demo-gh-secret"}}},
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
			assert.Contains(t, err.Error(), "demo-gh-cred", "the message must name the offending credential")
		})
	}
}

func TestSecretRef(t *testing.T) {
	assert.Nil(t, Kind{}.SecretRef(spiceboxv1alpha1.AgentCredential{Name: "demo-gh-cred", Type: "githubApp"}),
		"a malformed credential has no Secret to point at; validation reports it, SecretRef does not guess")

	ref := Kind{}.SecretRef(githubAppCred("demo-gh-cred", "demo-gh-app"))
	require.NotNil(t, ref)
	assert.Equal(t, "demo-gh-app", ref.Name)
	assert.Empty(t, ref.Key, "the App material is a fixed multi-key shape, not one named value")
}

func TestBuildCredential_NotWritableBySetup(t *testing.T) {
	_, err := Kind{}.BuildCredential("demo-gh-cred", "demo-gh-secret", "demo-gh-key")
	require.Error(t, err, "the Secret is written by the channel wizard, so setup cannot write one")
	assert.Contains(t, err.Error(), "channel wizard",
		"the message must name WHO writes the Secret, not just that setup can't")
	assert.Contains(t, err.Error(), "demo-gh-cred", "the message must name the offending credential")
}

// TestReadStoredValue_AlwaysErrors pins the read half of Minted: a githubApp
// credential is minted per resolve, never read as a value.
func TestReadStoredValue_AlwaysErrors(t *testing.T) {
	c := fakeclient.NewClientBuilder().WithScheme(testScheme(t)).Build()
	val, err := Kind{}.ReadStoredValue(context.Background(), c, "default", githubAppCred("demo-gh-cred", "demo-gh-secret"))
	require.Error(t, err, "a minted type has nothing stored to read")
	assert.Contains(t, err.Error(), "minted on demand and has no stored value to read",
		"the message must say WHY there is nothing to read, not just that it errored")
	assert.Contains(t, err.Error(), "demo-gh-cred", "the message must name the offending credential")
	assert.True(t, val.IsEmpty())
}

func TestResolve_NilMinterFailsClosed(t *testing.T) {
	c := fakeclient.NewClientBuilder().WithScheme(testScheme(t)).Build()
	_, _, err := Kind{}.Resolve(context.Background(), credkind.Deps{Client: c}, spiceboxv1alpha1.CredentialSource{
		Type: "githubApp", Namespace: "default", Name: "gh",
	})
	require.Error(t, err, "a githubApp credential with no minter must fail closed, never resolve empty")
	assert.Contains(t, err.Error(), "no GitHub App minter configured",
		"the error must name the missing collaborator")
}

func TestResolve_MissingSecret_ErrorsBeforeMinting(t *testing.T) {
	c := fakeclient.NewClientBuilder().WithScheme(testScheme(t)).Build()
	minter := &fakeDepsMinter{}

	_, _, err := Kind{}.Resolve(context.Background(), credkind.Deps{Client: c, GitHubApp: minter},
		spiceboxv1alpha1.CredentialSource{Type: "githubApp", Namespace: "demo-ns", Name: "gh"})
	require.Error(t, err)
	assert.ErrorIs(t, err, credresolve.ErrSecretMissing)
	assert.Equal(t, 0, minter.calls, "the mint must never be attempted when the Secret cannot be read")
}

// TestResolve_MissingSecretKey_NamesTheKey covers each of the three required
// keys individually: Secret present but missing exactly one key must name
// THAT key in the error, not a generic failure.
func TestResolve_MissingSecretKey_NamesTheKey(t *testing.T) {
	full := map[string][]byte{"app-id": []byte("123"), "private-key": []byte("pem"), "installation-id": []byte("456")}

	for _, missing := range []string{"app-id", "private-key", "installation-id"} {
		t.Run("missing "+missing+": error names "+missing, func(t *testing.T) {
			data := map[string][]byte{}
			for k, v := range full {
				if k != missing {
					data[k] = v
				}
			}
			sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "gh", Namespace: "demo-ns"}, Data: data}
			c := fakeclient.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(sec).Build()
			minter := &fakeDepsMinter{}

			_, _, err := Kind{}.Resolve(context.Background(), credkind.Deps{Client: c, GitHubApp: minter},
				spiceboxv1alpha1.CredentialSource{Type: "githubApp", Namespace: "demo-ns", Name: "gh"})
			require.Error(t, err)
			assert.ErrorIs(t, err, credresolve.ErrSecretKeyMissing)
			assert.Contains(t, err.Error(), missing, "the error must name the specific missing key")
			assert.Equal(t, 0, minter.calls, "the mint must never be attempted with incomplete App material")
		})
	}
}

func TestResolve_Success_ReadsSecretAndMintsUnchanged(t *testing.T) {
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "gh", Namespace: "demo-ns"},
		Data: map[string][]byte{
			"app-id":          []byte("app-123"),
			"private-key":     []byte("-----BEGIN RSA PRIVATE KEY-----\nfake\n-----END RSA PRIVATE KEY-----"),
			"installation-id": []byte("install-456"),
			"webhook-secret":  []byte("unused-by-resolve"),
		},
	}
	c := fakeclient.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(sec).Build()
	wantExp := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	minter := &fakeDepsMinter{token: sensitive.NewSensitiveValue([]byte("ghs_minted")), exp: wantExp}

	got, exp, err := Kind{}.Resolve(context.Background(), credkind.Deps{Client: c, GitHubApp: minter},
		spiceboxv1alpha1.CredentialSource{Type: "githubApp", Namespace: "demo-ns", Name: "gh"})
	require.NoError(t, err)
	assert.Equal(t, "ghs_minted", string(got.AccessToken.UnderlyingValue()))
	assert.True(t, exp.Equal(wantExp), "the minter's expiry must pass through unchanged")

	require.Equal(t, 1, minter.calls)
	assert.Equal(t, "app-123", minter.gotAppID)
	assert.Equal(t, "install-456", minter.gotInstallationID)
	assert.Equal(t, "-----BEGIN RSA PRIVATE KEY-----\nfake\n-----END RSA PRIVATE KEY-----", string(minter.gotPrivateKeyPEM))
}

// TestResolve_ZeroExpiryPassesThroughUnenforced pins that the zero-expiry
// fail-closed check deliberately does NOT live here -- it is enforced once
// at the broker's choke point for every minted kind, not per-kind.
func TestResolve_ZeroExpiryPassesThroughUnenforced(t *testing.T) {
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "gh", Namespace: "demo-ns"},
		Data: map[string][]byte{
			"app-id": []byte("app-123"), "private-key": []byte("pem"), "installation-id": []byte("install-456"),
		},
	}
	c := fakeclient.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(sec).Build()
	minter := &fakeDepsMinter{token: sensitive.NewSensitiveValue([]byte("ghs_minted"))} // exp left zero

	_, exp, err := Kind{}.Resolve(context.Background(), credkind.Deps{Client: c, GitHubApp: minter},
		spiceboxv1alpha1.CredentialSource{Type: "githubApp", Namespace: "demo-ns", Name: "gh"})
	require.NoError(t, err)
	assert.True(t, exp.IsZero(), "Resolve returns the minted expiry as-is, zero included")
}

func TestResolve_MintFailure_WrapsMinterError(t *testing.T) {
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "gh", Namespace: "demo-ns"},
		Data: map[string][]byte{
			"app-id": []byte("app-123"), "private-key": []byte("pem"), "installation-id": []byte("install-456"),
		},
	}
	c := fakeclient.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(sec).Build()
	minter := &fakeDepsMinter{err: assert.AnError}

	_, _, err := Kind{}.Resolve(context.Background(), credkind.Deps{Client: c, GitHubApp: minter},
		spiceboxv1alpha1.CredentialSource{Type: "githubApp", Namespace: "demo-ns", Name: "gh"})
	require.Error(t, err)
	assert.ErrorIs(t, err, assert.AnError)
}

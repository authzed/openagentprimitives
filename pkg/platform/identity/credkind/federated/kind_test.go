package federated

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
	// Registers the oauth Kind: Resolve's SubjectMaterial call reads the
	// IdP-identity Secret via a transient type=oauth AgentCredential, and
	// credresolve.ResolveSecretValue now dispatches that read through the
	// registry rather than switching on cred.Type — the "federated" type this
	// package's own init() registers is not enough. This package cannot
	// blank-import credkind/imports itself (it imports this "federated"
	// package, which would be a real cycle for an internal test file); oauth
	// alone has no path back to federated.
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/oauth"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/federation"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/federation/fake"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/sensitive"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s), "AddToScheme")
	require.NoError(t, corev1.AddToScheme(s), "corev1 AddToScheme")
	return s
}

func TestResolve_NilMinterFailsClosed(t *testing.T) {
	_, _, err := Kind{}.Resolve(context.Background(), credkind.Deps{}, spiceboxv1alpha1.CredentialSource{
		Type: "federated", Namespace: "default", Name: "idp",
	})
	require.Error(t, err, "a federated credential with no minter must fail closed, never resolve empty")
	assert.Contains(t, err.Error(), "no federation minter configured")
}

func TestResolve_Success_MintsFromIdPSubjectSecret(t *testing.T) {
	idpSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "idp", Namespace: "demo-ns"},
		Data: map[string][]byte{
			"access_token":   []byte("idp-subject-token"),
			"token_endpoint": []byte("https://idp.example/token"),
			"client_id":      []byte("demo-client"),
		},
	}
	c := fakeclient.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(idpSecret).Build()
	minter := &fake.Minter{TTL: time.Hour}

	got, exp, err := Kind{}.Resolve(context.Background(), credkind.Deps{Client: c, Federation: minter},
		spiceboxv1alpha1.CredentialSource{
			Type: "federated", Namespace: "demo-ns", Name: "idp",
			Resource: "demo-resource", ResourceServerURL: "https://mcp.example",
		})
	require.NoError(t, err)
	assert.Equal(t, "minted-for-demo-resource", string(got.AccessToken.UnderlyingValue()))
	assert.False(t, exp.IsZero(), "the minter's expiry must pass through unmodified")

	require.Len(t, minter.Calls, 1)
	assert.Equal(t, "demo-resource", minter.Calls[0].Resource)
	assert.Equal(t, "https://mcp.example", minter.Calls[0].ResourceServerURL)
	assert.Equal(t, "idp-subject-token", string(minter.Calls[0].Subject.Token.UnderlyingValue()),
		"the subject token must come from the IdP-identity Secret, not the upstream credential")
}

// zeroExpiryMinter mints without stamping an expiry, standing in for a
// misbehaving federation.Minter implementation.
type zeroExpiryMinter struct{}

func (zeroExpiryMinter) Mint(context.Context, federation.MintRequest) (federation.MintedToken, error) {
	return federation.MintedToken{AccessToken: sensitive.NewSensitiveValue([]byte("tok"))}, nil
}

func TestResolve_ZeroExpiryPassesThroughUnenforced(t *testing.T) {
	// The zero-expiry fail-closed check deliberately does NOT live here — it
	// is enforced once at the broker's choke point (Task 4) for every minted
	// kind, rather than trusted to each implementation.
	idpSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "idp", Namespace: "demo-ns"},
		Data:       map[string][]byte{"access_token": []byte("idp-subject-token")},
	}
	c := fakeclient.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(idpSecret).Build()

	_, exp, err := Kind{}.Resolve(context.Background(), credkind.Deps{Client: c, Federation: zeroExpiryMinter{}},
		spiceboxv1alpha1.CredentialSource{Type: "federated", Namespace: "demo-ns", Name: "idp", Resource: "r"})
	require.NoError(t, err)
	assert.True(t, exp.IsZero(), "Resolve returns the minted expiry as-is, zero included")
}

func TestResolve_MintFailure_WrapsMinterError(t *testing.T) {
	idpSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "idp", Namespace: "demo-ns"},
		Data:       map[string][]byte{"access_token": []byte("idp-subject-token")},
	}
	c := fakeclient.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(idpSecret).Build()
	minter := &fake.Minter{Err: assert.AnError}

	_, _, err := Kind{}.Resolve(context.Background(), credkind.Deps{Client: c, Federation: minter},
		spiceboxv1alpha1.CredentialSource{Type: "federated", Namespace: "demo-ns", Name: "idp", Resource: "r"})
	require.Error(t, err)
	assert.ErrorIs(t, err, assert.AnError)
}

func TestResolve_MissingIdPSecret_ErrorsBeforeMinting(t *testing.T) {
	c := fakeclient.NewClientBuilder().WithScheme(testScheme(t)).Build()
	minter := &fake.Minter{}

	_, _, err := Kind{}.Resolve(context.Background(), credkind.Deps{Client: c, Federation: minter},
		spiceboxv1alpha1.CredentialSource{Type: "federated", Namespace: "demo-ns", Name: "idp", Resource: "r"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "idp subject")
	assert.Empty(t, minter.Calls, "the mint must never be attempted when the subject material cannot be resolved")
}

func TestValidOn_SessionUserIdentityOnly(t *testing.T) {
	assert.False(t, credkind.ValidOnScope(Kind{}, credkind.ScopeAgentIdentity),
		"federated is passthrough-only: a bot identity has no human subject to assert")
	assert.False(t, credkind.ValidOnScope(Kind{}, credkind.ScopeUserIdentity),
		"nothing in the repo writes type=federated onto a UserIdentity, and nothing validates a "+
			"UserIdentity's credentials against ValidOn at all -- the only producer, "+
			"BuildSessionUserIdentity, writes it directly onto a SessionUserIdentity")
	assert.True(t, credkind.ValidOnScope(Kind{}, credkind.ScopeSessionUserIdentity))
}

func TestValidateSpec(t *testing.T) {
	cases := []struct {
		name    string
		cred    spiceboxv1alpha1.AgentCredential
		wantErr string
	}{
		{
			name:    "missing federated block: rejected",
			cred:    spiceboxv1alpha1.AgentCredential{Name: "c", Type: "federated"},
			wantErr: "block required for type=federated",
		},
		{
			name: "well-formed: accepted",
			cred: spiceboxv1alpha1.AgentCredential{Name: "c", Type: "federated",
				Federated: &spiceboxv1alpha1.FederatedCredentialSource{Resource: "r"}},
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

func TestBuildCredential_NotWritableBySetup(t *testing.T) {
	_, err := Kind{}.BuildCredential("c", "s", "k")
	require.Error(t, err, "nothing is stored for a federated credential, so setup cannot write one")
}

// TestReadStoredValue_AlwaysErrors pins the read half of Minted: a federated
// credential is minted per resolve via an ID-JAG exchange, never read from a
// Secret, so credresolve.ResolveSecretValue's registry dispatch must fail
// closed here rather than return a zero SensitiveValue.
func TestReadStoredValue_AlwaysErrors(t *testing.T) {
	c := fakeclient.NewClientBuilder().WithScheme(testScheme(t)).Build()
	val, err := Kind{}.ReadStoredValue(context.Background(), c, "default", spiceboxv1alpha1.AgentCredential{Name: "c", Type: "federated"})
	require.Error(t, err, "a minted type has nothing stored to read")
	assert.Contains(t, err.Error(), "c", "the message must name the offending credential")
	assert.True(t, val.IsEmpty())
}

func TestFacts(t *testing.T) {
	assert.Equal(t, "federated", Kind{}.Type())
	assert.True(t, Kind{}.Minted(), "federated is minted per use")
	assert.False(t, Kind{}.NeedsRefresh(), "nothing stored means nothing to refresh")
	assert.Nil(t, Kind{}.SecretRef(spiceboxv1alpha1.AgentCredential{Type: "federated"}),
		"the IdP-identity Secret is not this credential's backing Secret")
	assert.NotEmpty(t, Kind{}.DisplayName())
}

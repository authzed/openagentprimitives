package inproc

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/authkind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/broker"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind"
	credkindregistry "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry/registrytest"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/federation"
	federationfake "github.com/authzed/openagentprimitives/pkg/platform/identity/federation/fake"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/refresh"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/sensitive"
)

// installRefreshClient routes pkg/platform/identity/refresh's package-global
// HTTP client through srv for the test's lifetime.
func installRefreshClient(t *testing.T, srv *httptest.Server) {
	t.Helper()
	refresh.SetHTTPClient(srv.Client())
	t.Cleanup(func() { refresh.SetHTTPClient(http.DefaultClient) })
}

func staticDesc(ns, name, key, envVar string) spiceboxv1alpha1.CredentialDescriptor {
	return spiceboxv1alpha1.CredentialDescriptor{
		Source: spiceboxv1alpha1.CredentialSource{Type: "static", Namespace: ns, Name: name, Key: key},
		Inject: spiceboxv1alpha1.CredentialInjection{EnvVar: envVar},
	}
}

func TestBroker_StaticEnvVar(t *testing.T) {
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"},
		Data:       map[string][]byte{"token": []byte("ghp_abc")},
	}
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(sec).Build()
	b := New(c)

	res, err := b.Resolve(context.Background(), broker.Request{
		Credentials: []spiceboxv1alpha1.CredentialDescriptor{staticDesc("default", "s", "token", "GH_TOKEN")},
	})
	require.NoError(t, err)
	assert.Equal(t, "ghp_abc", res.EnvVars["GH_TOKEN"])
	assert.Empty(t, res.HTTPHeaders)
}

func TestBroker_OAuthHeader(t *testing.T) {
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "o", Namespace: "default"},
		Data: map[string][]byte{
			"access_token": []byte("at-1"),
			"expires_at":   []byte(time.Now().Add(time.Hour).UTC().Format(time.RFC3339)),
		},
	}
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(sec).Build()
	b := New(c)

	res, err := b.Resolve(context.Background(), broker.Request{
		Credentials: []spiceboxv1alpha1.CredentialDescriptor{{
			Source: spiceboxv1alpha1.CredentialSource{Type: "oauth", Namespace: "default", Name: "o"},
			Inject: spiceboxv1alpha1.CredentialInjection{Header: &spiceboxv1alpha1.HeaderInjection{Name: "Authorization", ValuePrefix: "Bearer "}},
		}},
	})
	require.NoError(t, err)
	assert.Equal(t, "Bearer at-1", res.HTTPHeaders["Authorization"])
}

func TestBroker_MissingSecret(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).Build()
	b := New(c)
	_, err := b.Resolve(context.Background(), broker.Request{
		Credentials: []spiceboxv1alpha1.CredentialDescriptor{staticDesc("default", "nope", "token", "X")},
	})
	require.Error(t, err)
}

func TestInvalidateSecretDropsCacheEntry(t *testing.T) {
	const ns = "agent-ns"
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "my-cred-secret"},
		Data:       map[string][]byte{"token": []byte("token-v1")},
	}
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(sec).Build()
	b := New(c)
	desc := staticDesc(ns, "my-cred-secret", "token", "MY_TOKEN")

	// Prime the cache.
	res1, err := b.Resolve(context.Background(), broker.Request{
		Credentials: []spiceboxv1alpha1.CredentialDescriptor{desc},
	})
	require.NoError(t, err)
	assert.Equal(t, "token-v1", res1.EnvVars["MY_TOKEN"])

	// Mutate the Secret so a re-read would return a different value.
	sec.Data["token"] = []byte("token-v2")
	require.NoError(t, c.Update(context.Background(), sec))

	// Drop the cache entry by Secret coordinates.
	require.NoError(t, b.InvalidateSecret(ns, "my-cred-secret"))

	// Next Resolve must re-read the Secret and return the updated value.
	res2, err := b.Resolve(context.Background(), broker.Request{
		Credentials: []spiceboxv1alpha1.CredentialDescriptor{desc},
	})
	require.NoError(t, err)
	assert.Equal(t, "token-v2", res2.EnvVars["MY_TOKEN"],
		"after InvalidateSecret, Resolve must return the freshly resolved value")
}

func TestBroker_ExpiredOAuth_JITRefresh(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at-2", "refresh_token": "rt-2", "expires_in": 3600, "token_type": "Bearer",
		})
	}))
	t.Cleanup(srv.Close)
	installRefreshClient(t, srv)

	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "o", Namespace: "default"},
		Data: map[string][]byte{
			"access_token":   []byte("at-1"),
			"refresh_token":  []byte("rt-1"),
			"expires_at":     []byte(time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)),
			"token_endpoint": []byte(srv.URL),
			"client_id":      []byte("cid"),
		},
	}
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(sec).Build()
	b := New(c)

	res, err := b.Resolve(context.Background(), broker.Request{
		Credentials: []spiceboxv1alpha1.CredentialDescriptor{{
			Source: spiceboxv1alpha1.CredentialSource{Type: "oauth", Namespace: "default", Name: "o"},
			Inject: spiceboxv1alpha1.CredentialInjection{Header: &spiceboxv1alpha1.HeaderInjection{Name: "Authorization", ValuePrefix: "Bearer "}},
		}},
	})
	require.NoError(t, err, "JIT refresh must recover an expired token")
	assert.Equal(t, "Bearer at-2", res.HTTPHeaders["Authorization"])
}

// idpIdentitySecret returns a fake IdP-identity Secret with oauth fields but
// no expires_at — tokens are treated as perpetually valid, so no JIT refresh
// is triggered during the federated resolution tests.
func idpIdentitySecret(name string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "agentprimitives-identities"},
		Data: map[string][]byte{
			"access_token":   []byte("user-id-token"),
			"refresh_token":  []byte("rt"),
			"token_endpoint": []byte("https://idp.example.com/token"),
			"client_id":      []byte("ap-client"),
			"client_secret":  []byte("ap-secret"),
			// no expires_at → never expires → no JIT refresh in this test
		},
	}
}

func TestResolve_Federated_MintsAndInjectsBearer(t *testing.T) {
	idpSecret := idpIdentitySecret("u-abc-idp-identity")
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(idpSecret).Build()
	m := &federationfake.Minter{TTL: time.Hour}
	b := NewWithMinter(c, m)

	res, err := b.Resolve(t.Context(), broker.Request{Credentials: []spiceboxv1alpha1.CredentialDescriptor{{
		Source: spiceboxv1alpha1.CredentialSource{
			Type: "federated", Namespace: "agentprimitives-identities",
			Name: "u-abc-idp-identity", Resource: "linear-res", ResourceServerURL: "https://mcp.example.com",
		},
		Inject: spiceboxv1alpha1.CredentialInjection{Header: &spiceboxv1alpha1.HeaderInjection{Name: "Authorization", ValuePrefix: "Bearer "}},
	}}})
	require.NoError(t, err)
	assert.Equal(t, "Bearer minted-for-linear-res", res.HTTPHeaders["Authorization"])
	require.Len(t, m.Calls, 1)
	assert.Equal(t, "linear-res", m.Calls[0].Resource)
	assert.Equal(t, "https://mcp.example.com", m.Calls[0].ResourceServerURL)
}

// zeroExpiryMinter is a federation.Minter that mints a token with a zero
// ExpiresAt — the shape any non-conformant Minter implementation can produce.
type zeroExpiryMinter struct{}

func (zeroExpiryMinter) Mint(_ context.Context, _ federation.MintRequest) (federation.MintedToken, error) {
	return federation.MintedToken{
		AccessToken: sensitive.NewSensitiveValue([]byte("minted-no-expiry")),
		// ExpiresAt deliberately left as the zero value.
	}, nil
}

// A federated credential is short-lived by definition; the broker's cache reads
// a zero expiresAt as "never expires" (resolveOneCached). Caching a minted token
// forever would make mid-session revocation unenforceable for every federated
// credential, so the broker must reject a zero expiry at the choke point rather
// than trust each Minter to stamp one. idjag.DefaultTokenTTL fixes the shipped
// Minter; this guard covers every other implementation of the interface.
func TestResolve_Federated_ZeroExpiry_FailsClosed(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(idpIdentitySecret("u-abc-idp-identity")).Build()
	b := NewWithMinter(c, zeroExpiryMinter{})

	_, err := b.Resolve(t.Context(), broker.Request{Credentials: []spiceboxv1alpha1.CredentialDescriptor{{
		Source: spiceboxv1alpha1.CredentialSource{
			Type: "federated", Namespace: "agentprimitives-identities",
			Name: "u-abc-idp-identity", Resource: "linear-res", ResourceServerURL: "https://mcp.example.com",
		},
		Inject: spiceboxv1alpha1.CredentialInjection{Header: &spiceboxv1alpha1.HeaderInjection{Name: "Authorization", ValuePrefix: "Bearer "}},
	}}})

	require.Error(t, err, "a federated mint with no expiry must fail closed, not be cached forever")
	assert.Contains(t, err.Error(), "expiry", "error must name the missing expiry")
	assert.NotContains(t, err.Error(), "minted-no-expiry", "error must not leak token bytes")
}

func TestResolve_Federated_NilMinter_FailsClosed(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(idpIdentitySecret("u-abc-idp-identity")).Build()
	b := New(c) // no minter
	_, err := b.Resolve(t.Context(), broker.Request{Credentials: []spiceboxv1alpha1.CredentialDescriptor{{
		Source: spiceboxv1alpha1.CredentialSource{Type: "federated", Namespace: "agentprimitives-identities", Name: "u-abc-idp-identity", Resource: "r"},
		Inject: spiceboxv1alpha1.CredentialInjection{Header: &spiceboxv1alpha1.HeaderInjection{Name: "Authorization"}},
	}}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no federation minter")
}

func TestInvalidateSecret_Federated_DropsAllResourceEntries(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(idpIdentitySecret("u-abc-idp-identity")).Build()
	m := &federationfake.Minter{TTL: time.Hour}
	b := NewWithMinter(c, m)
	mintTwice := func() {
		for _, r := range []string{"linear-res", "asana-res"} {
			_, err := b.Resolve(t.Context(), broker.Request{Credentials: []spiceboxv1alpha1.CredentialDescriptor{{
				Source: spiceboxv1alpha1.CredentialSource{Type: "federated", Namespace: "agentprimitives-identities", Name: "u-abc-idp-identity", Resource: r},
				Inject: spiceboxv1alpha1.CredentialInjection{Header: &spiceboxv1alpha1.HeaderInjection{Name: "Authorization"}},
			}}})
			require.NoError(t, err)
		}
	}
	mintTwice()
	require.Len(t, m.Calls, 2) // both minted, cached

	mintTwice()
	require.Len(t, m.Calls, 2) // served from cache, no new mints

	require.NoError(t, b.InvalidateSecret("agentprimitives-identities", "u-abc-idp-identity"))
	mintTwice()
	assert.Len(t, m.Calls, 4) // both resource entries dropped → re-minted
}

// zeroExpiryMintedKind is a credkind.Kind that reports itself as Minted but
// whose Resolve returns a zero expiry — the shape any non-conformant Kind
// implementation could produce, and exactly what resolveOneSource's
// zero-expiry guard exists to catch at the broker's single choke point.
type zeroExpiryMintedKind struct{}

var _ credkind.Kind = zeroExpiryMintedKind{}

func (zeroExpiryMintedKind) Type() string        { return "zeroexp" }
func (zeroExpiryMintedKind) Minted() bool        { return true }
func (zeroExpiryMintedKind) NeedsRefresh() bool  { return false }
func (zeroExpiryMintedKind) DisplayName() string { return "Zero-expiry test kind" }

func (zeroExpiryMintedKind) ValidOn() []credkind.Scope {
	return []credkind.Scope{credkind.ScopeAgentIdentity}
}

func (zeroExpiryMintedKind) ValidateSpec(spiceboxv1alpha1.AgentCredential) error { return nil }

func (zeroExpiryMintedKind) SecretRefPath() []string { return nil }

// HasBlock: a test kind owns no AgentCredential union block.
func (zeroExpiryMintedKind) HasBlock(spiceboxv1alpha1.AgentCredential) bool { return false }

func (zeroExpiryMintedKind) SecretRef(spiceboxv1alpha1.AgentCredential) *credkind.SecretRef {
	return nil
}

func (zeroExpiryMintedKind) BuildCredential(string, string, string) (spiceboxv1alpha1.AgentCredential, error) {
	return spiceboxv1alpha1.AgentCredential{}, nil
}

func (zeroExpiryMintedKind) Resolve(context.Context, credkind.Deps, spiceboxv1alpha1.CredentialSource) (authkind.ResolvedCredential, time.Time, error) {
	return authkind.ResolvedCredential{}, time.Time{}, nil
}

func (zeroExpiryMintedKind) ReadStoredValue(context.Context, client.Reader, string, spiceboxv1alpha1.AgentCredential) (sensitive.SensitiveValue, error) {
	return sensitive.SensitiveValue{}, errors.New("zeroExpiryMintedKind: minted, nothing stored")
}

func (zeroExpiryMintedKind) Projectable() bool { return false }

func (zeroExpiryMintedKind) PublicSecretKeys(spiceboxv1alpha1.AgentCredential) []string   { return nil }
func (zeroExpiryMintedKind) RequiredSecretKeys(spiceboxv1alpha1.AgentCredential) []string { return nil }

func TestResolveOneSource_MintedKindWithZeroExpiryFailsClosed(t *testing.T) {
	registrytest.Snapshot(t)
	credkindregistry.Reset()
	credkindregistry.Register(zeroExpiryMintedKind{})

	b := &Broker{Client: fake.NewClientBuilder().Build(), clk: clock.RealClock{}}
	_, _, err := b.resolveOneSource(context.Background(), spiceboxv1alpha1.CredentialSource{
		Type: "zeroexp", Namespace: "default", Name: "x",
	})
	require.Error(t, err, "a minted kind returning no expiry must fail closed at the choke point")
	assert.Contains(t, err.Error(), "must never be cached indefinitely")
}

func TestResolveOneSource_UnknownTypeFailsClosed(t *testing.T) {
	registrytest.Snapshot(t)
	credkindregistry.Reset()

	b := &Broker{Client: fake.NewClientBuilder().Build(), clk: clock.RealClock{}}
	_, _, err := b.resolveOneSource(context.Background(), spiceboxv1alpha1.CredentialSource{
		Type: "nosuch", Namespace: "default", Name: "x",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown credential type")
}

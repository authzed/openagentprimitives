package credresolve

import (
	"context"
	"fmt"
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
	"github.com/authzed/openagentprimitives/pkg/platform/identity/authkind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind"
	credkindregistry "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry/registrytest"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/sensitive"
)

// The static/oauth/federated credkind Kinds this file's tests need
// registered (SourceFor / ResolveSecretValue now dispatch through
// registry.Get(cred.Type)) are registered by resolve_test.go's blank import
// of credkind/imports — this file (package credresolve, not credresolve_test)
// cannot import that blank-import package itself without an import cycle
// (credkind/imports -> credkind/static -> credresolve), but both files link
// into the same `go test` binary and share that one process-wide
// registration. credkind/registry itself has no such cycle (it depends only
// on the credkind interface package), so it is imported directly below for
// TestSourceFor_UnknownTypeIsAnError's Reset/Register.

func staticCred(name, secret, key string) spiceboxv1alpha1.AgentCredential {
	return spiceboxv1alpha1.AgentCredential{Name: name, Type: "static",
		Static: &spiceboxv1alpha1.StaticCredentialSource{SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: secret, Key: key}}}
}

func TestDescriptors(t *testing.T) {
	id := RuntimeIdentity{Namespace: "default", Label: "AgentIdentity x",
		Credentials: []spiceboxv1alpha1.AgentCredential{staticCred("github-token", "s", "token")}}
	reqEnv := authkind.CredentialRequirement{SuggestedName: "github-token", Inject: authkind.Injection{EnvVar: "GITHUB_TOKEN"}}

	t.Run("env requirement → descriptor", func(t *testing.T) {
		got, err := Descriptors([]authkind.CredentialRequirement{reqEnv}, id, nil)
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, "GITHUB_TOKEN", got[0].Inject.EnvVar)
		assert.Equal(t, "s", got[0].Source.Name)
		assert.Equal(t, "token", got[0].Source.Key)
	})
	t.Run("header requirement → descriptor", func(t *testing.T) {
		req := authkind.CredentialRequirement{SuggestedName: "github-token", Inject: authkind.Injection{Header: &authkind.HeaderInjection{Name: "Authorization", ValuePrefix: "Bearer "}}}
		got, err := Descriptors([]authkind.CredentialRequirement{req}, id, nil)
		require.NoError(t, err)
		require.NotNil(t, got[0].Inject.Header)
		assert.Equal(t, "Authorization", got[0].Inject.Header.Name)
		assert.Equal(t, "Bearer ", got[0].Inject.Header.ValuePrefix)
	})
	t.Run("missing credential → typed error, never nil,nil", func(t *testing.T) {
		_, err := Descriptors([]authkind.CredentialRequirement{{SuggestedName: "absent", Inject: authkind.Injection{EnvVar: "X"}}}, id, nil)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrCredentialMissing)
	})
	t.Run("remap applied before lookup", func(t *testing.T) {
		got, err := Descriptors([]authkind.CredentialRequirement{{SuggestedName: "tool-name", Inject: authkind.Injection{EnvVar: "GITHUB_TOKEN"}}}, id, map[string]string{"tool-name": "github-token"})
		require.NoError(t, err)
		assert.Equal(t, "s", got[0].Source.Name)
	})
}

func TestDescriptors_FederatedSourceCarriesResource(t *testing.T) {
	id := RuntimeIdentity{
		Namespace: "agentprimitives-identities",
		Label:     "SessionUserIdentity s1",
		Credentials: []spiceboxv1alpha1.AgentCredential{{
			Name: "linear", Type: "federated",
			Federated: &spiceboxv1alpha1.FederatedCredentialSource{
				Resource:          "linear-res",
				ResourceServerURL: "https://mcp.example.com",
				IdPSecretRef:      spiceboxv1alpha1.SecretRef{Name: "u-abc-idp-identity"},
				Scopes:            []string{"read"},
			},
		}},
	}
	reqs := []authkind.CredentialRequirement{{
		SuggestedName: "linear",
		Inject:        authkind.Injection{Header: &authkind.HeaderInjection{Name: "Authorization", ValuePrefix: "Bearer "}},
	}}

	got, err := Descriptors(reqs, id, nil)
	require.NoError(t, err)
	require.Len(t, got, 1)
	src := got[0].Source
	assert.Equal(t, "federated", src.Type)
	assert.Equal(t, "u-abc-idp-identity", src.Name) // IdP Secret, not upstream
	assert.Equal(t, "linear-res", src.Resource)
	assert.Equal(t, "https://mcp.example.com", src.ResourceServerURL)
	assert.Equal(t, []string{"read"}, src.Scopes)
}

// TestSessionUserIdentityStaticProjection_ToolCallNamespace is the regression
// pin for the cross-namespace ToolCall denial. A userPassthrough type=static
// credential, after projection, must yield a descriptor whose Source namespace
// equals the session (ToolCall) namespace — so ValidateCredentialSourceNamespaces
// PASSES where it previously failed (the master was stamped in the identities
// namespace). A type=oauth passthrough credential still resolves from the
// identities namespace, so the same boundary check fail-closes it (the sandbox
// oauth-refresh path is deferred).
func TestSessionUserIdentityStaticProjection_ToolCallNamespace(t *testing.T) {
	suid := &spiceboxv1alpha1.SessionUserIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "sess-bob", Namespace: "default"},
		Spec: spiceboxv1alpha1.SessionUserIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{
				staticCred("github-token", "u-bob-github-token", "token"),
				{Name: "linear-oauth", Type: "oauth",
					OAuth: &spiceboxv1alpha1.OAuthCredentialSource{SecretRef: spiceboxv1alpha1.SecretRef{Name: "u-bob-linear"}}},
			},
		},
	}
	id := RuntimeIdentityFromSessionUserIdentity(suid)

	toolCall := func(descs []spiceboxv1alpha1.CredentialDescriptor) *spiceboxv1alpha1.ToolCall {
		return &spiceboxv1alpha1.ToolCall{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
			Spec:       spiceboxv1alpha1.ToolCallSpec{Credentials: descs},
		}
	}

	t.Run("static credential: source in session namespace → ToolCall validation passes", func(t *testing.T) {
		descs, err := Descriptors([]authkind.CredentialRequirement{
			{SuggestedName: "github-token", Inject: authkind.Injection{EnvVar: "GITHUB_TOKEN"}},
		}, id, nil)
		require.NoError(t, err)
		require.Len(t, descs, 1)
		assert.Equal(t, "default", descs[0].Source.Namespace, "static source must resolve in the session namespace")
		assert.Equal(t, spiceboxv1alpha1.PassthroughCredentialSecretName("sess-bob"), descs[0].Source.Name)
		assert.Equal(t, "github-token", descs[0].Source.Key, "projected Secret is keyed by credential name")
		assert.NoError(t, toolCall(descs).ValidateCredentialSourceNamespaces(),
			"a projected static passthrough credential must pass the ToolCall namespace boundary")
	})

	t.Run("oauth credential: source stays in identities namespace → ToolCall validation fails (fail-closed)", func(t *testing.T) {
		descs, err := Descriptors([]authkind.CredentialRequirement{
			{SuggestedName: "linear-oauth", Inject: authkind.Injection{EnvVar: "LINEAR_TOKEN"}},
		}, id, nil)
		require.NoError(t, err)
		require.Len(t, descs, 1)
		assert.Equal(t, spiceboxv1alpha1.IdentitiesNamespace, descs[0].Source.Namespace,
			"oauth source stays anchored on the master Secret namespace")
		assert.Error(t, toolCall(descs).ValidateCredentialSourceNamespaces(),
			"oauth passthrough sandbox creds are fail-closed (refresh-token double-spend not yet solved)")
	})
}

// TestSourceFor_UnknownTypeIsAnError pins the fail-closed contract the
// registry dispatch replaces the switch with: an unregistered credential type
// must return an error, never a zero-value Source and never a silent
// fallback to treating the credential as though it were type=static (the old
// switch's `default:` arm did exactly that).
func TestSourceFor_UnknownTypeIsAnError(t *testing.T) {
	registrytest.Snapshot(t)
	credkindregistry.Reset()

	_, err := SourceFor(&spiceboxv1alpha1.AgentCredential{Name: "c", Type: "nosuch"}, RuntimeIdentity{})
	require.Error(t, err, "an unregistered type must not silently produce a zero Source")
}

// TestSourceFor_MintedTypeWithNilFederatedBlock_IsAnError pins SourceFor's
// defensive guard for a minted type whose credential carries no Federated
// block to read Resource/ResourceServerURL/Scopes/IdPSecretRef from — a state
// ValidateSpec rejects before it reaches here, but SourceFor must still fail
// closed rather than panic on a nil dereference if that guard is ever bypassed.
func TestSourceFor_MintedTypeWithNilFederatedBlock_IsAnError(t *testing.T) {
	_, err := SourceFor(&spiceboxv1alpha1.AgentCredential{Name: "c", Type: "federated"}, RuntimeIdentity{Namespace: "default"})
	require.Error(t, err, "a minted credential with no federated block must error, not panic or resolve a zero Source")
}

// mintedNonFederatedKind stands in for a kind like githubApp: Minted() is
// true, but nothing like federated's no-Secret shape — SecretRef() reports no
// backing Secret either, so a credential of this type with no Federated block
// has genuinely nothing for SourceFor to resolve. Today federated is the
// only Minted kind, which is exactly why SourceFor's Minted() branch could
// read cred.Federated directly and look correct while actually assuming
// minted == federated.
type mintedNonFederatedKind struct{}

var _ credkind.Kind = mintedNonFederatedKind{}

func (mintedNonFederatedKind) Type() string        { return "demo-minted" }
func (mintedNonFederatedKind) Minted() bool        { return true }
func (mintedNonFederatedKind) NeedsRefresh() bool  { return false }
func (mintedNonFederatedKind) DisplayName() string { return "Demo minted test kind" }
func (mintedNonFederatedKind) Projectable() bool   { return false }

func (mintedNonFederatedKind) ValidOn() []credkind.Scope {
	return []credkind.Scope{credkind.ScopeAgentIdentity}
}

func (mintedNonFederatedKind) ValidateSpec(spiceboxv1alpha1.AgentCredential) error { return nil }

func (mintedNonFederatedKind) SecretRefPath() []string { return nil }

// HasBlock: a test kind owns no AgentCredential union block.
func (mintedNonFederatedKind) HasBlock(spiceboxv1alpha1.AgentCredential) bool { return false }

func (mintedNonFederatedKind) SecretRef(spiceboxv1alpha1.AgentCredential) *credkind.SecretRef {
	return nil
}

func (mintedNonFederatedKind) BuildCredential(name, _, _ string) (spiceboxv1alpha1.AgentCredential, error) {
	return spiceboxv1alpha1.AgentCredential{}, fmt.Errorf("credential %q: mintedNonFederatedKind cannot be built by the setup flow", name)
}

func (mintedNonFederatedKind) Resolve(context.Context, credkind.Deps, spiceboxv1alpha1.CredentialSource) (authkind.ResolvedCredential, time.Time, error) {
	return authkind.ResolvedCredential{}, time.Time{}, fmt.Errorf("mintedNonFederatedKind: Resolve not exercised by this test")
}

func (mintedNonFederatedKind) ReadStoredValue(context.Context, client.Reader, string, spiceboxv1alpha1.AgentCredential) (sensitive.SensitiveValue, error) {
	return sensitive.SensitiveValue{}, fmt.Errorf("mintedNonFederatedKind: minted, nothing stored to read")
}

func (mintedNonFederatedKind) PublicSecretKeys(spiceboxv1alpha1.AgentCredential) []string { return nil }
func (mintedNonFederatedKind) RequiredSecretKeys(spiceboxv1alpha1.AgentCredential) []string {
	return nil
}

// TestSourceFor_MintedNonFederatedKindDoesNotErrorOnANilFederatedBlock is the
// test that makes today's Minted()-gated body wrong the moment a second
// minted kind (like githubApp) registers: federated is the only Minted kind
// today, so SourceFor's Minted() branch could read cred.Federated directly
// and both today's federated tests AND this fake would need a real
// distinguishing case to catch the assumption. Before the fix, SourceFor's
// Minted() branch reads cred.Federated unconditionally and errors "its
// federated block is nil" — wrong for a kind that was never federated in the
// first place. After the fix, SourceFor consults k.SecretRef() first; this
// kind has neither a Secret nor a Federated block, so it must still error,
// but with a kind-neutral message naming the type, not the word "federated".
func TestSourceFor_MintedNonFederatedKindDoesNotErrorOnANilFederatedBlock(t *testing.T) {
	registrytest.Snapshot(t)
	credkindregistry.Register(mintedNonFederatedKind{})

	_, err := SourceFor(&spiceboxv1alpha1.AgentCredential{
		Name: "gh-app", Type: "demo-minted",
	}, RuntimeIdentity{Namespace: "default", Label: "demo-agent"})

	require.Error(t, err, "a minted kind with no locatable Secret still cannot produce a Source")
	// The constraint is "no message here may say federated" — not "may not
	// repeat the OLD message's exact phrase". Asserting the word itself is
	// what actually catches a fix that swaps in different federated-flavored
	// wording (e.g. "...or a federated block to resolve") while still naming
	// a mechanism this kind never had.
	assert.NotContains(t, err.Error(), "federated",
		"the message must not name federated — this kind is minted but is not federated")
	assert.Contains(t, err.Error(), "demo-minted",
		"the error must name cred.Type so the failure is diagnosable without source-diving")
}

// TestSourceFor_ProjectionGatesOnProjectableNotOnSecretRefKey pins the fix
// for the projection gate keying off credkind.Kind.Projectable() rather than
// off the credential's own Static.SecretRef.Key. Two regressions this
// closes: (1) a static credential whose secretRef.key is the empty string
// (admissible at the apiserver — key is `required` but has no `minLength`)
// used to fall through to the master Secret instead of the session-projected
// one, because the old gate read ref.Key != "" off SecretRef; (2) a static
// credential with a nil Static block used to error before reaching the
// projection branch at all, even though the projection branch never needs
// Static populated — it resolves purely from cred.Name + id.StaticProjection.
func TestSourceFor_ProjectionGatesOnProjectableNotOnSecretRefKey(t *testing.T) {
	proj := &StaticCredentialProjection{Namespace: "sess-ns", SecretName: "sess-secret"}

	t.Run("secretRef.key empty: still resolves to the projected Source, not the master", func(t *testing.T) {
		cred := spiceboxv1alpha1.AgentCredential{Name: "github-token", Type: "static",
			Static: &spiceboxv1alpha1.StaticCredentialSource{SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "master-secret", Key: ""}}}
		src, err := SourceFor(&cred, RuntimeIdentity{Namespace: "identities-ns", StaticProjection: proj})
		require.NoError(t, err)
		assert.Equal(t, "sess-ns", src.Namespace, "must resolve in the session namespace, not the master's")
		assert.Equal(t, "sess-secret", src.Name, "must resolve to the projected Secret, not master-secret")
		assert.Equal(t, "github-token", src.Key, "projected Secret is keyed by credential name")
	})

	t.Run("nil Static block: projection branch never needs it, so this still resolves rather than erroring", func(t *testing.T) {
		cred := spiceboxv1alpha1.AgentCredential{Name: "github-token", Type: "static"}
		src, err := SourceFor(&cred, RuntimeIdentity{Namespace: "identities-ns", StaticProjection: proj})
		require.NoError(t, err)
		assert.Equal(t, "sess-ns", src.Namespace)
		assert.Equal(t, "sess-secret", src.Name)
		assert.Equal(t, "github-token", src.Key)
	})

	t.Run("no projection configured: nil Static block still errors (no backing Secret to resolve)", func(t *testing.T) {
		cred := spiceboxv1alpha1.AgentCredential{Name: "github-token", Type: "static"}
		_, err := SourceFor(&cred, RuntimeIdentity{Namespace: "identities-ns"})
		require.Error(t, err, "outside projection, a nil Static block has no backing Secret to resolve")
	})
}

func TestValidateCoverage(t *testing.T) {
	id := RuntimeIdentity{Credentials: []spiceboxv1alpha1.AgentCredential{{Name: "github-token"}}}
	gaps := ValidateCoverage([]authkind.CredentialRequirement{{SuggestedName: "github-token"}, {SuggestedName: "missing"}}, id, nil)
	require.Len(t, gaps, 1)
	assert.Equal(t, "missing", gaps[0].CredentialName)
}

func descTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s), "AddToScheme")
	require.NoError(t, corev1.AddToScheme(s), "corev1 AddToScheme")
	return s
}

// TestCheckSecretPresent verifies CheckSecretPresent validates the backing
// Secret without returning the value: present+non-empty → nil, present-but-empty
// → ErrSecretValueEmpty, absent secret → ErrSecretMissing.
func TestCheckSecretPresent(t *testing.T) {
	cred := staticCred("github-token", "s", "token")
	cases := []struct {
		name    string
		secret  *corev1.Secret
		wantErr error
	}{
		{
			name:    "present non-empty: nil",
			secret:  &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"}, Data: map[string][]byte{"token": []byte("abc")}},
			wantErr: nil,
		},
		{
			name:    "present empty: ErrSecretValueEmpty",
			secret:  &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"}, Data: map[string][]byte{"token": []byte("")}},
			wantErr: ErrSecretValueEmpty,
		},
		{
			name:    "absent secret: ErrSecretMissing",
			secret:  nil,
			wantErr: ErrSecretMissing,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objs := []client.Object{}
			if tc.secret != nil {
				objs = append(objs, tc.secret)
			}
			c := fake.NewClientBuilder().WithScheme(descTestScheme(t)).WithObjects(objs...).Build()
			err := CheckSecretPresent(context.Background(), c, "default", cred)
			if tc.wantErr != nil {
				assert.ErrorIs(t, err, tc.wantErr)
				return
			}
			assert.NoError(t, err)
		})
	}
}

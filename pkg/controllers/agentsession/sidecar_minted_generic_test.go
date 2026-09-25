// pkg/controllers/agentsession/sidecar_minted_generic_test.go
//
// Unit test proving materializeSidecarSecret's Minted() branch (controller.go)
// does not assume the minted kind IS federated. Today federated is the only
// registered Minted kind, which is exactly why a body that reads cred.Federated
// or calls r.Minter directly can look correct while actually encoding "minted
// == federated". This file registers a SECOND minted kind — one backed by its
// own multi-key Secret, with no Federated block at all and no dependency on
// r.Minter — to prove the site dispatches through k.Resolve() alone.
package agentsession

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/authkind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind"
	credkindregistry "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry/registrytest"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/sensitive"
)

// demoMintedKind stands in for a kind like githubApp: Minted() is true, but
// the credential has no Federated block and its own real, multi-key backing
// Secret. Resolve reads that Secret directly and never touches
// credkind.Deps.Federation — proving a minted kind need not be a federation
// mint at all.
type demoMintedKind struct{ secretName string }

var _ credkind.Kind = demoMintedKind{}

func (demoMintedKind) Type() string        { return "demo-minted" }
func (demoMintedKind) Minted() bool        { return true }
func (demoMintedKind) NeedsRefresh() bool  { return false }
func (demoMintedKind) DisplayName() string { return "Demo minted test kind" }
func (demoMintedKind) Projectable() bool   { return false }

func (demoMintedKind) ValidOn() []credkind.Scope {
	return []credkind.Scope{credkind.ScopeAgentIdentity}
}

func (demoMintedKind) ValidateSpec(spiceboxv1alpha1.AgentCredential) error { return nil }

func (demoMintedKind) SecretRefPath() []string { return []string{"demoMinted", "secretRef", "name"} }

// HasBlock: a test kind owns no AgentCredential union block.
func (demoMintedKind) HasBlock(spiceboxv1alpha1.AgentCredential) bool { return false }

func (k demoMintedKind) SecretRef(spiceboxv1alpha1.AgentCredential) *credkind.SecretRef {
	return &credkind.SecretRef{Name: k.secretName}
}

func (demoMintedKind) BuildCredential(name, _, _ string) (spiceboxv1alpha1.AgentCredential, error) {
	return spiceboxv1alpha1.AgentCredential{}, fmt.Errorf("credential %q: demoMintedKind cannot be built by the setup flow", name)
}

func (demoMintedKind) Resolve(ctx context.Context, deps credkind.Deps, src spiceboxv1alpha1.CredentialSource) (authkind.ResolvedCredential, time.Time, error) {
	var sec corev1.Secret
	if err := deps.Client.Get(ctx, types.NamespacedName{Namespace: src.Namespace, Name: src.Name}, &sec); err != nil {
		return authkind.ResolvedCredential{}, time.Time{}, fmt.Errorf("demoMintedKind: read backing secret %s/%s: %w", src.Namespace, src.Name, err)
	}
	return authkind.ResolvedCredential{AccessToken: sensitive.NewSensitiveValue(sec.Data["installation-id"])},
		time.Now().Add(time.Hour), nil
}

func (demoMintedKind) ReadStoredValue(context.Context, client.Reader, string, spiceboxv1alpha1.AgentCredential) (sensitive.SensitiveValue, error) {
	return sensitive.SensitiveValue{}, fmt.Errorf("demoMintedKind: minted, nothing stored to read")
}

func (demoMintedKind) PublicSecretKeys(spiceboxv1alpha1.AgentCredential) []string   { return nil }
func (demoMintedKind) RequiredSecretKeys(spiceboxv1alpha1.AgentCredential) []string { return nil }

// TestMaterializeSidecarSecret_MintedNonFederatedKind_ResolvesThroughItsOwnSecretNotFederation
// pins the fix for the Minted() branch of materializeSidecarSecret assuming
// the minted kind was federated: before the fix it called r.Minter.Mint
// directly and required cred.Federated != nil, so a minted kind resolving
// from its own Secret (like demoMintedKind here) would fail with either
// "no federation minter is configured" (Minter nil, as it deliberately is
// below) or "federated block is nil" (no Federated block, as here). The fixed
// body dispatches through k.Resolve() alone and must succeed.
func TestMaterializeSidecarSecret_MintedNonFederatedKind_ResolvesThroughItsOwnSecretNotFederation(t *testing.T) {
	registrytest.Snapshot(t)
	credkindregistry.Register(demoMintedKind{secretName: "gh-app-secret"})

	const (
		ns         = "default"
		envVar     = "UPSTREAM_TOKEN"
		sidecarRef = "ghapp"
		credName   = sidecarRef + "-creds"
		scSecret   = "sc-secret-demo-minted"
	)

	backing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "gh-app-secret", Namespace: ns},
		Data:       map[string][]byte{"installation-id": []byte("inst-123")},
	}
	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "ai-demo-minted", Namespace: ns},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{Name: credName, Type: "demo-minted"}},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(testfixtures.NewScheme(t)).
		WithObjects(backing, ai).
		Build()

	// Minter is deliberately nil: a non-federated minted kind must resolve
	// without ever touching it.
	r := &Reconciler{Client: c, Minter: nil}

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s-demo-minted", Namespace: ns},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{AgentIdentity: "ai-demo-minted"},
	}
	ac := &spiceboxv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "ac-demo-minted", Namespace: ns}}
	rt := spiceboxv1alpha1.ResolvedSidecarToolbox{
		Name: sidecarRef,
		Ref:  sidecarRef,
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			UpstreamAuth: spiceboxv1alpha1.SidecarToolboxUpstream{Provider: "demo-minted-provider", EnvVar: envVar},
		},
	}

	err := r.materializeSidecarSecret(t.Context(), sess, ac, rt, scSecret, nil)
	require.NoError(t, err, "a minted kind with its own Secret and a nil Minter must still resolve")

	var sec corev1.Secret
	require.NoError(t, c.Get(t.Context(), types.NamespacedName{Namespace: ns, Name: scSecret}, &sec))
	got := sec.StringData[envVar]
	if got == "" {
		got = string(sec.Data[envVar])
	}
	assert.Equal(t, "inst-123", got, "value must come from demoMintedKind's own Secret, not a federation mint")
}

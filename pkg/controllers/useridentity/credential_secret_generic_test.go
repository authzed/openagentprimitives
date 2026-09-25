// pkg/controllers/useridentity/credential_secret_generic_test.go
//
// Unit test proving reconcileStatus (controller.go) does not validate every
// multi-key-shaped Secret (an empty ref.Key) as though it were oauth. The
// oauth-shaped access_token message is gated on k.NeedsRefresh() — the fact
// that actually means "this Secret holds a value that expires and is
// refreshed out of band" — rather than on an empty key, which a minted
// multi-key kind reports too. A minted kind with its own fixed multi-key
// Secret (a GitHub App's app-id/private-key/installation-id, say) that
// declines to declare a RequiredSecretKeys requirement (returns nil) gets no
// per-field check here at all — this file registers exactly that second
// shape to prove it.
package useridentity

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/authkind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind"
	credkindregistry "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry/registrytest"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/sensitive"
)

// demoMultiKeyMintedKind stands in for a kind like githubApp: Minted() is
// true, NeedsRefresh() is false, and SecretRef reports a real backing Secret
// with NO single named key (Key == ""), because its Secret shape is fixed
// and multi-field, same as oauth's. It is exactly the shape a Key == "" gate
// cannot distinguish from oauth.
type demoMultiKeyMintedKind struct{ secretName string }

var _ credkind.Kind = demoMultiKeyMintedKind{}

func (demoMultiKeyMintedKind) Type() string        { return "demo-minted" }
func (demoMultiKeyMintedKind) Minted() bool        { return true }
func (demoMultiKeyMintedKind) NeedsRefresh() bool  { return false }
func (demoMultiKeyMintedKind) DisplayName() string { return "Demo minted test kind" }
func (demoMultiKeyMintedKind) Projectable() bool   { return false }

func (demoMultiKeyMintedKind) ValidOn() []credkind.Scope {
	return []credkind.Scope{credkind.ScopeUserIdentity}
}

func (demoMultiKeyMintedKind) ValidateSpec(spiceboxv1alpha1.AgentCredential) error { return nil }

// HasBlock: a test kind owns no AgentCredential union block.
func (demoMultiKeyMintedKind) HasBlock(spiceboxv1alpha1.AgentCredential) bool { return false }

func (demoMultiKeyMintedKind) SecretRefPath() []string {
	return []string{"demoMultiKeyMinted", "secretRef", "name"}
}

func (k demoMultiKeyMintedKind) SecretRef(spiceboxv1alpha1.AgentCredential) *credkind.SecretRef {
	return &credkind.SecretRef{Name: k.secretName} // Key == "": fixed multi-key shape
}

func (demoMultiKeyMintedKind) BuildCredential(name, _, _ string) (spiceboxv1alpha1.AgentCredential, error) {
	return spiceboxv1alpha1.AgentCredential{}, fmt.Errorf("credential %q: demoMultiKeyMintedKind cannot be built by the setup flow", name)
}

func (demoMultiKeyMintedKind) Resolve(context.Context, credkind.Deps, spiceboxv1alpha1.CredentialSource) (authkind.ResolvedCredential, time.Time, error) {
	return authkind.ResolvedCredential{}, time.Time{}, fmt.Errorf("demoMultiKeyMintedKind: Resolve not exercised by this test")
}

func (demoMultiKeyMintedKind) ReadStoredValue(context.Context, client.Reader, string, spiceboxv1alpha1.AgentCredential) (sensitive.SensitiveValue, error) {
	return sensitive.SensitiveValue{}, fmt.Errorf("demoMultiKeyMintedKind: minted, nothing stored to read")
}

// RequiredSecretKeys is nil: this test kind deliberately declares no per-key
// requirement, standing in for a fixed multi-key shape this generic checker
// has no field-level knowledge of.
func (demoMultiKeyMintedKind) PublicSecretKeys(spiceboxv1alpha1.AgentCredential) []string { return nil }
func (demoMultiKeyMintedKind) RequiredSecretKeys(spiceboxv1alpha1.AgentCredential) []string {
	return nil
}

// TestReconcileStatus_MintedMultiKeyKindIsNotValidatedAsOAuth pins two facts
// about reconcileStatus: the OAuthSecretIncomplete message stays gated on
// k.NeedsRefresh() (false here) rather than on an empty key, so a minted
// kind's own multi-key Secret — which has neither access_token nor
// expires_at — is never checked against oauth's schema; and a kind that
// declares no RequiredSecretKeys gets no per-field check at all — existence
// was already confirmed by the adopt + Get above, and per-field validation
// of a shape this generic checker was never told about is the kind's own
// concern.
func TestReconcileStatus_MintedMultiKeyKindIsNotValidatedAsOAuth(t *testing.T) {
	registrytest.Snapshot(t)
	credkindregistry.Register(demoMultiKeyMintedKind{secretName: "u-x-ghapp"})

	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "u-x-ghapp", Namespace: spiceboxv1alpha1.IdentitiesNamespace},
		// Deliberately NOT oauth-shaped: no access_token, no expires_at.
		Data: map[string][]byte{"installation-id": []byte("inst-123")},
	}
	adoptguard.WithAdoptedLabel(sec)

	ui := &spiceboxv1alpha1.UserIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "u-x"},
		Spec: spiceboxv1alpha1.UserIdentitySpec{
			Subject: "user:abc",
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: "ghapp", Type: "demo-minted",
			}},
		},
	}

	c := buildClient(t, sec, ui)
	r := newReconciler(c)
	reconcileOnce(t, r, "u-x")

	var got spiceboxv1alpha1.UserIdentity
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "u-x"}, &got))
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.UserIdentityConditionValid)
	require.NotNil(t, cond, "Valid condition must be present")
	assert.Equal(t, metav1.ConditionTrue, cond.Status,
		"a minted multi-key credential must resolve — it must not be checked against oauth's access_token schema")
	assert.Equal(t, spiceboxv1alpha1.ReasonAllReferencesResolve, cond.Reason)
	assert.Equal(t, []string{"ghapp"}, got.Status.AvailableCredentials)
}

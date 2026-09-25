// pkg/controllers/useridentity/required_secret_keys_test.go
//
// Unit test proving reconcileStatus (controller.go) actually drives its
// per-key validation from k.RequiredSecretKeys — not from a fact the registry
// cannot distinguish using only today's three kinds (static/oauth/federated).
// A kind that declares a multi-key requirement (a GitHub App's
// app-id/private-key/installation-id, say) must have EACH of those keys
// checked, and the resulting failure must name the SPECIFIC missing key —
// not just report "an error occurred", and not describe every required key
// as if all were absent.
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

// demoTripleKeyKind is a minted kind whose Secret has a fixed THREE-key
// shape — unlike demoMultiKeyMintedKind in credential_secret_generic_test.go
// (same package), which deliberately declares no requirement at all. This is
// the shape that discriminates a correct RequiredSecretKeys-driven checker
// from one that still does nothing for a type it does not special-case:
// today's three registered kinds (static, oauth, federated) can never tell
// the two apart, because none of them needs more than one required key.
type demoTripleKeyKind struct{ secretName string }

var _ credkind.Kind = demoTripleKeyKind{}

func (demoTripleKeyKind) Type() string        { return "demo-triple" }
func (demoTripleKeyKind) Minted() bool        { return true }
func (demoTripleKeyKind) NeedsRefresh() bool  { return false }
func (demoTripleKeyKind) DisplayName() string { return "Demo triple-key test kind" }
func (demoTripleKeyKind) Projectable() bool   { return false }

func (demoTripleKeyKind) ValidOn() []credkind.Scope {
	return []credkind.Scope{credkind.ScopeUserIdentity}
}

func (demoTripleKeyKind) ValidateSpec(spiceboxv1alpha1.AgentCredential) error { return nil }

// HasBlock: a test kind owns no AgentCredential union block.
func (demoTripleKeyKind) HasBlock(spiceboxv1alpha1.AgentCredential) bool { return false }

func (demoTripleKeyKind) SecretRefPath() []string {
	return []string{"demoTripleKey", "secretRef", "name"}
}

func (k demoTripleKeyKind) SecretRef(spiceboxv1alpha1.AgentCredential) *credkind.SecretRef {
	return &credkind.SecretRef{Name: k.secretName} // Key == "": fixed multi-key shape
}

func (demoTripleKeyKind) BuildCredential(name, _, _ string) (spiceboxv1alpha1.AgentCredential, error) {
	return spiceboxv1alpha1.AgentCredential{}, fmt.Errorf("credential %q: demoTripleKeyKind cannot be built by the setup flow", name)
}

func (demoTripleKeyKind) Resolve(context.Context, credkind.Deps, spiceboxv1alpha1.CredentialSource) (authkind.ResolvedCredential, time.Time, error) {
	return authkind.ResolvedCredential{}, time.Time{}, fmt.Errorf("demoTripleKeyKind: Resolve not exercised by this test")
}

func (demoTripleKeyKind) ReadStoredValue(context.Context, client.Reader, string, spiceboxv1alpha1.AgentCredential) (sensitive.SensitiveValue, error) {
	return sensitive.SensitiveValue{}, fmt.Errorf("demoTripleKeyKind: minted, nothing stored to read")
}

// RequiredSecretKeys is the shape under test: three fixed keys, none of them
// "access_token" or a NeedsRefresh()-shaped name, so a checker that still
// special-cases oauth's own key rather than reading this method would miss
// all three.
func (demoTripleKeyKind) PublicSecretKeys(spiceboxv1alpha1.AgentCredential) []string { return nil }
func (demoTripleKeyKind) RequiredSecretKeys(spiceboxv1alpha1.AgentCredential) []string {
	return []string{"alpha-key", "beta-key", "gamma-key"}
}

// TestReconcileStatus_MultiKeyRequirementNamesTheMissingKey is the test that
// discriminates a RequiredSecretKeys-driven reconcileStatus from the pre-fix
// default-does-nothing behavior: it registers a kind with a THREE-key
// requirement, a Secret with the middle key ("beta-key") absent, and asserts
// the Valid condition reports the credential as invalid with a message
// naming exactly "beta-key" — not the first key present ("alpha-key") and
// not the last key never reached ("gamma-key"). Reverting reconcileStatus's
// RequiredSecretKeys loop back to the old key/NeedsRefresh switch makes this
// fail: that switch's default arm did nothing for a kind reporting an empty
// key that is not NeedsRefresh(), so this credential would resolve
// Valid=True instead.
func TestReconcileStatus_MultiKeyRequirementNamesTheMissingKey(t *testing.T) {
	registrytest.Snapshot(t)
	credkindregistry.Register(demoTripleKeyKind{secretName: "demo-triple-secret"})

	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-triple-secret", Namespace: spiceboxv1alpha1.IdentitiesNamespace},
		Data: map[string][]byte{
			"alpha-key": []byte("a"),
			// beta-key is deliberately absent.
			"gamma-key": []byte("g"),
		},
	}
	adoptguard.WithAdoptedLabel(sec)

	ui := &spiceboxv1alpha1.UserIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "u-triple"},
		Spec: spiceboxv1alpha1.UserIdentitySpec{
			Subject: "user:triple",
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: "triple", Type: "demo-triple",
			}},
		},
	}

	c := buildClient(t, sec, ui)
	r := newReconciler(c)
	reconcileOnce(t, r, "u-triple")

	var got spiceboxv1alpha1.UserIdentity
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "u-triple"}, &got))
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.UserIdentityConditionValid)
	require.NotNil(t, cond, "Valid condition must be present")

	assert.Equal(t, metav1.ConditionFalse, cond.Status,
		"a Secret missing one of a multi-key requirement's fields must be rejected")
	assert.Equal(t, spiceboxv1alpha1.ReasonSecretKeyMissing, cond.Reason)
	assert.Contains(t, cond.Message, `key "beta-key"`,
		"the message must name the SPECIFIC missing key")
	assert.NotContains(t, cond.Message, "alpha-key",
		"the message must not describe the key that IS present as missing")
	assert.NotContains(t, cond.Message, "gamma-key",
		"the loop must stop at the first missing key, not describe every required key")
}

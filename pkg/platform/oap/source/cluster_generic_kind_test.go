// pkg/platform/oap/source/cluster_generic_kind_test.go
//
// Unit test proving collectDepSecretRefs (cluster.go) does not special-case
// the three credkind kinds it happens to know about by name. An UNREGISTERED
// type and a federated type (both covered in cluster_test.go) behave
// IDENTICALLY under the pre-migration hardcoded switch and the post-
// migration registry dispatch — neither discriminates the fix. The scenario
// that DOES discriminate, and the one Task 0b's own motivation names (a
// fourth kind like githubApp): a type that IS registered in credkind but is
// not one of Static/OAuth/Federated. The old switch matched no case and
// silently dropped its Secret from the .oap bundle; the new dispatch
// captures it — this file registers exactly that fourth shape to prove it.
package source_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/authkind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind"
	credkindregistry "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry/registrytest"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/sensitive"
)

// demoGenericKind stands in for a hypothetical fourth registered credkind —
// githubApp is the concrete example Task 0b's motivation names. It is
// registered, but is not one of the three cases the pre-migration switch in
// collectDepSecretRefs hardcoded (cred.Static/OAuth/Federated). Its
// SecretRef reports BOTH a Name and a Key, so this exercises the exact fact
// collectDepSecretRefs must carry through — the Key is why that site calls
// registry.Get + Kind.SecretRef directly rather than the name-only
// SecretNameFor.
type demoGenericKind struct{ secretName, secretKey string }

var _ credkind.Kind = demoGenericKind{}

func (demoGenericKind) Type() string        { return "demo-generic" }
func (demoGenericKind) Minted() bool        { return false }
func (demoGenericKind) NeedsRefresh() bool  { return false }
func (demoGenericKind) DisplayName() string { return "Demo generic test kind" }
func (demoGenericKind) Projectable() bool   { return true }

func (demoGenericKind) ValidOn() []credkind.Scope {
	return []credkind.Scope{credkind.ScopeAgentIdentity}
}

func (demoGenericKind) ValidateSpec(v1alpha1.AgentCredential) error { return nil }

func (k demoGenericKind) SecretRef(v1alpha1.AgentCredential) *credkind.SecretRef {
	return &credkind.SecretRef{Name: k.secretName, Key: k.secretKey}
}

func (demoGenericKind) SecretRefPath() []string { return []string{"demoGeneric", "secretRef", "name"} }

// HasBlock: a test kind owns no AgentCredential union block.
func (demoGenericKind) HasBlock(v1alpha1.AgentCredential) bool { return false }

func (demoGenericKind) BuildCredential(name, _, _ string) (v1alpha1.AgentCredential, error) {
	return v1alpha1.AgentCredential{}, fmt.Errorf("credential %q: demoGenericKind cannot be built by the setup flow", name)
}

func (demoGenericKind) Resolve(context.Context, credkind.Deps, v1alpha1.CredentialSource) (authkind.ResolvedCredential, time.Time, error) {
	return authkind.ResolvedCredential{}, time.Time{}, fmt.Errorf("demoGenericKind: Resolve not exercised by this test")
}

func (demoGenericKind) ReadStoredValue(context.Context, client.Reader, string, v1alpha1.AgentCredential) (sensitive.SensitiveValue, error) {
	return sensitive.SensitiveValue{}, fmt.Errorf("demoGenericKind: ReadStoredValue not exercised by this test")
}

func (demoGenericKind) PublicSecretKeys(v1alpha1.AgentCredential) []string   { return nil }
func (demoGenericKind) RequiredSecretKeys(v1alpha1.AgentCredential) []string { return nil }

// TestClusterSource_Bundle_RegisteredNonHardcodedKind_SecretIsCaptured is the
// test that actually discriminates the collectDepSecretRefs migration: a
// credential of a type the registry knows but the old switch's three cases
// did not name. Revert collectDepSecretRefs to the pointer-switch and this
// test fails — the unregistered-type and federated subtests in
// cluster_test.go do not.
func TestClusterSource_Bundle_RegisteredNonHardcodedKind_SecretIsCaptured(t *testing.T) {
	registrytest.Snapshot(t)
	credkindregistry.Register(demoGenericKind{secretName: "gh-app-secret", secretKey: "installation-id"})

	f := fixtures()
	f.agentIdentity.Spec.Credentials = []v1alpha1.AgentCredential{{
		Name: "cred",
		Type: "demo-generic",
	}}
	b, err := buildBundle(t, f)
	require.NoError(t, err)

	var found bool
	var gotKeys []string
	for _, rs := range b.Manifest.Requires.Secrets {
		if rs.Name == "gh-app-secret" {
			found = true
			gotKeys = rs.Keys
		}
	}
	require.True(t, found, "a registered-but-not-hardcoded kind's Secret must still be recorded as required")
	assert.Equal(t, []string{"installation-id"}, gotKeys,
		"the Key must ride along too — this is why collectDepSecretRefs calls registry.Get + Kind.SecretRef "+
			"directly, not the name-only SecretNameFor")

	var gotKey string
	for _, q := range b.Manifest.Questions {
		if q.Secret != nil && q.Secret.CreateSecret != nil && q.Secret.CreateSecret.Name == "gh-app-secret" {
			gotKey = q.Secret.CreateSecret.Key
		}
	}
	assert.Equal(t, "installation-id", gotKey, "the install question must target the kind's own key")
}

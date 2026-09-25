package registry_test

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	// Registers every shipped credkind.Kind against THIS test binary's
	// registry, for the reason secretname_test.go's copy of this import gives.
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/imports"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"
)

// everyBlockPopulated builds a credential of the given type carrying EVERY
// union block. registry.ValidateExclusive would reject it and no cluster would
// ever hold it, which is the point: it makes each kind answer about its own
// block rather than take the nil-block early return, so a test sweeping the
// registry probes the real answer for every type at once.
func everyBlockPopulated(typ string) spiceboxv1alpha1.AgentCredential {
	return spiceboxv1alpha1.AgentCredential{
		Name: "c", Type: typ,
		Static: &spiceboxv1alpha1.StaticCredentialSource{
			SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "demo-secret", Key: "the-value"}},
		OAuth: &spiceboxv1alpha1.OAuthCredentialSource{
			SecretRef: spiceboxv1alpha1.SecretRef{Name: "demo-secret"}},
		Federated: &spiceboxv1alpha1.FederatedCredentialSource{},
		GitHubApp: &spiceboxv1alpha1.GitHubAppCredentialSource{
			SecretRef: spiceboxv1alpha1.SecretRef{Name: "demo-secret"}},
	}
}

// TestPublicSecretKeysFor_FailsClosedOnAnUnregisteredType is the rule the whole
// method exists to keep: every outcome that is not an explicit declaration must
// be "all of it is secret".
//
// An unregistered type is the shape a consumer is most likely to get wrong,
// because the successful and the failed call both hand back an empty slice. The
// error is the only thing that distinguishes them, so it must be there.
func TestPublicSecretKeysFor_FailsClosedOnAnUnregisteredType(t *testing.T) {
	got, err := registry.PublicSecretKeysFor(spiceboxv1alpha1.AgentCredential{Name: "c", Type: "nosuch"})
	require.Error(t, err, "an unregistered type is a wiring bug, not a credential with no public keys")
	assert.Contains(t, err.Error(), "nosuch", "the error must name the type a reader has to go and register")
	assert.Empty(t, got, "no keys may be reported alongside an error; a caller reading past it must be safe")
}

// TestPublicSecretKeysFor_DeclaringNothingIsTheDefault walks the registry
// rather than naming types, so a credential type added later is covered here
// without this test being edited — and covered with the SAFE answer, since a
// new type that has not thought about this method returns nil.
//
// githubApp is the one exception and is asserted positively below; every other
// registered type must declare nothing.
//
// EVERY union block is populated, which no real credential would be. Each kind
// reads only its own and most of them fail closed to nil when theirs is absent,
// so a probe with an empty credential would report "declares nothing" for every
// type on earth and assert precisely nothing.
func TestPublicSecretKeysFor_DeclaringNothingIsTheDefault(t *testing.T) {
	for _, k := range registry.All() {
		if k.Type() == "githubApp" {
			continue
		}
		cred := everyBlockPopulated(k.Type())
		got, err := registry.PublicSecretKeysFor(cred)
		require.NoError(t, err, "type %q is registered, so it must answer", k.Type())
		assert.Empty(t, got,
			"type %q declares a public Secret key; that is a deliberate act and needs its own test "+
				"naming the key and saying why it is published", k.Type())
	}
}

// TestPublicSecretKeysFor_GitHubAppDeclaresItsTwoPublishedIdentifiers is the
// positive half, asserted THROUGH the registry so the dispatch is covered and
// not just the kind's own method.
func TestPublicSecretKeysFor_GitHubAppDeclaresItsTwoPublishedIdentifiers(t *testing.T) {
	cred := spiceboxv1alpha1.AgentCredential{Name: "gh", Type: "githubApp",
		GitHubApp: &spiceboxv1alpha1.GitHubAppCredentialSource{
			SecretRef: spiceboxv1alpha1.SecretRef{Name: "demo-gh-secret"}}}

	got, err := registry.PublicSecretKeysFor(cred)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"app-id", "installation-id"}, got)
	assert.NotContains(t, got, "private-key", "the App's signing key is not a public identifier")
	assert.NotContains(t, got, "webhook-secret", "the delivery-signing secret is not a public identifier")
}

// TestPublicSecretKeysNeverCoverACredentialsOwnValueKey is a registry-wide
// invariant rather than a list, and it is the strongest generic statement
// available: whatever else a type publishes, it must never publish the key its
// OWN value is stored under.
//
// SecretRef.Key is non-empty exactly for the types that store their credential
// under one named key (static today), which is precisely the case where a
// public declaration would hand a consumer the credential itself. Multi-key
// types report an empty Key and are skipped — for those, the backstop is the
// structural scan, which reads the emitted bytes with no reference to any
// declaration at all.
func TestPublicSecretKeysNeverCoverACredentialsOwnValueKey(t *testing.T) {
	for _, k := range registry.All() {
		cred := everyBlockPopulated(k.Type())

		ref := k.SecretRef(cred)
		if ref == nil || ref.Key == "" {
			continue
		}
		assert.False(t, slices.Contains(k.PublicSecretKeys(cred), ref.Key),
			"type %q declares %q public, and that is the key its own credential VALUE lives under",
			k.Type(), ref.Key)
	}
}

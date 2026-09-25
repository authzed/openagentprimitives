package githubapp

import (
	"testing"

	"github.com/stretchr/testify/assert"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// TestPublicSecretKeys pins WHICH of this type's Secret data keys are public
// identifiers, and — the half that matters — which two are not.
//
// The distinction exists because a consumer holding the Secret's bytes cannot
// make it: "this came out of a Secret" is provenance, not sensitivity. A
// steel-thread capture refused every triggered GitHub session over exactly
// this, because installation-id is in the body of every webhook GitHub sends,
// and no amount of redaction takes it back out of a verbatim record of what was
// delivered.
//
// Both directions are asserted. A test that only checked the two public keys
// were listed would still pass with private-key listed beside them, which is
// the one mistake this method must never make.
func TestPublicSecretKeys(t *testing.T) {
	assert.Nil(t, Kind{}.PublicSecretKeys(spiceboxv1alpha1.AgentCredential{Type: "githubApp"}),
		"a nil block must fail closed to NOTHING public, not to a nil dereference")

	got := Kind{}.PublicSecretKeys(githubAppCred("demo-gh-cred", "demo-gh-secret"))
	assert.ElementsMatch(t, []string{"app-id", "installation-id"}, got,
		"app-id is in the App's own settings URL and installation-id is in every webhook body; "+
			"neither authenticates anything on its own")
	assert.NotContains(t, got, "private-key",
		"the private key signs the App JWT: declaring it public tells a consumer it is safe to write down")
	assert.NotContains(t, got, "webhook-secret",
		"the webhook secret authenticates every inbound delivery; same rule")
}

// TestPublicSecretKeysAreKeysThisTypesSecretActuallyHolds keeps the declaration
// from drifting into a key that belongs to nobody. A public key this type never
// reads is either a typo or a claim about some other type's Secret shape, and
// both are worth failing on rather than quietly vouching for a key that is not
// ours to vouch for.
//
// The reverse containment is deliberately NOT asserted: private-key is required
// and secret, which is the entire point of having two lists.
func TestPublicSecretKeysAreKeysThisTypesSecretActuallyHolds(t *testing.T) {
	cred := githubAppCred("demo-gh-cred", "demo-gh-secret")
	// webhook-secret is read by the github CHANNEL kind against this same
	// Secret and is deliberately absent from RequiredSecretKeys, so the
	// superset is the union of what either half reads.
	held := append(Kind{}.RequiredSecretKeys(cred), "webhook-secret")
	for _, k := range (Kind{}).PublicSecretKeys(cred) {
		assert.Contains(t, held, k, "public key %q is not a key this credential type's Secret holds", k)
	}
}

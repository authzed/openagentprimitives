package github

import (
	"testing"

	"github.com/stretchr/testify/assert"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// TestPublicSecretKeys pins which of this kind's four credentials-Secret data
// keys are published identifiers and which two are the credential.
//
// It is the channel-side twin of the githubApp credkind's identical test, and
// they must agree because they describe ONE Secret: the wizard here writes all
// four keys, and that credential type reads three of them. Either reference may
// exist without the other — a Channel that mints its own tokens needs no
// AgentIdentity credential — so a consumer asks both and unions the answers.
//
// Both directions are asserted. Listing the two public keys correctly while ALSO
// listing private-key would satisfy a one-sided test and would tell a consumer
// that the App's signing key is safe to write into a repo.
func TestPublicSecretKeys(t *testing.T) {
	got := Kind{}.PublicSecretKeys(&spiceboxv1alpha1.Channel{})
	assert.ElementsMatch(t, []string{"app-id", "installation-id"}, got,
		"app-id is in the App's own settings URL; installation-id is in the body of every "+
			"webhook GitHub delivers, so a verbatim record of a delivery cannot omit it")
	assert.NotContains(t, got, "private-key", "the App's signing key is not a published identifier")
	assert.NotContains(t, got, "webhook-secret",
		"the webhook secret authenticates every inbound delivery; publishing it would be the leak")
}

// TestPublicSecretKeysAreASubsetOfRequiredSecretKeys keeps the two lists talking
// about the same Secret. A public key this kind does not require is a claim
// about a key that is not in the Secret this kind writes.
func TestPublicSecretKeysAreASubsetOfRequiredSecretKeys(t *testing.T) {
	ch := &spiceboxv1alpha1.Channel{}
	required := Kind{}.RequiredSecretKeys(ch)
	for _, k := range (Kind{}).PublicSecretKeys(ch) {
		assert.Contains(t, required, k, "public key %q is not one this kind's Secret holds", k)
	}
}

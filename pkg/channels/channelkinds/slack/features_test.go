package slack

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/channels/channelfeatures"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// TestFeatureSupportScopeUnionMatchesKnownScopes pins FeatureSupport's total
// scope union (via channelkinds.ScopesFor) to a known-good golden list.
//
// It guards INTERNAL consistency only, which is its limit: every entry below
// was copied from what FeatureSupport already said, so a scope that is wrong at
// the source is wrong here too and this test stays green.
// TestFeatureSupportDeclaresOnlyScopesSlackDefines (scope_vocabulary_test.go)
// is the check against Slack's own vocabulary.
//
// Before this test existed as written, it compared against the package-level
// requiredBotScopes var (scopes.go) — a second hand-maintained copy of the
// same scope list. That collapse (scopes.go, manifest.go now both generate
// from FeatureSupport via channelkinds.ScopesFor) removed the second copy,
// which would otherwise leave this test nothing to compare against. Rather
// than delete the guard — the only remaining structural check that
// FeatureSupport's union still equals what the runtime has always required —
// the known scope list is pinned here as test-only golden data instead of a
// second production list: an edit to FeatureSupport that silently drops or
// adds a scope (e.g. a channelfeatures entry losing its Scopes) must update
// this list deliberately, not by accident.
//
// This also covers map completeness: dropping a channelfeatures entry (e.g.
// AttachmentsInbound, and with it files:read) shrinks the union below the
// golden list and fails here, even though every other FeatureSupport test
// stays green.
func TestFeatureSupportScopeUnionMatchesKnownScopes(t *testing.T) {
	knownRequiredScopes := []string{
		"app_mentions:read",
		"chat:write",
		"chat:write.customize",
		"commands",
		"channels:history",
		"groups:history",
		"im:history",
		"im:write",
		"users:read",
		"users:read.email",
		"assistant:write",
		"files:write",
		"files:read",
		"channels:read",
		"groups:read",
	}
	got := channelkinds.ScopesFor(&Kind{}, channelfeatures.All())
	assert.ElementsMatch(t, knownRequiredScopes, got,
		"FeatureSupport's scope union changed; if intentional, update knownRequiredScopes here")
}

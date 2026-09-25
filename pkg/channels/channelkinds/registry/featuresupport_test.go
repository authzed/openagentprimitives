package registry_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelfeatures"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"

	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/bento"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/local"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
)

func TestEveryKindDeclaresFeatureSupport(t *testing.T) {
	kinds := registry.All()
	require.NotEmpty(t, kinds, "the kind registry must be populated by init")

	valid := map[channelfeatures.Feature]bool{}
	for _, f := range channelfeatures.All() {
		valid[f] = true
	}

	for _, k := range kinds {
		t.Run(k.Name()+": FeatureSupport declares only known features", func(t *testing.T) {
			for f, req := range k.FeatureSupport() {
				assert.True(t, valid[f], "kind %q declares unknown feature %q", k.Name(), f)
				assert.NotEmpty(t, req.Degrades,
					"kind %q feature %q must say what breaks without it — that string is the "+
						"ScopesValid=False message an operator reads", k.Name(), f)
			}
		})
	}
}

func TestSlackDeclaresScopesForItsFeatures(t *testing.T) {
	k, ok := registry.Get("slack")
	require.True(t, ok, "slack kind must be registered")

	sup := k.FeatureSupport()
	req, ok := sup[channelfeatures.AttachmentsOutbound]
	require.True(t, ok, "slack must support outbound attachments")
	assert.Contains(t, req.Scopes, "files:write")
	assert.NotEmpty(t, req.Setup, "the wizard renders Setup as where-to-enable guidance")
}

func TestScopesForIsDeduplicatedAndSorted(t *testing.T) {
	k, ok := registry.Get("slack")
	require.True(t, ok)

	// ThreadHistory contributes channels:history, groups:history, im:history,
	// users:read, users:read.email. ChannelHistory contributes channels:history,
	// groups:history, users:read, users:read.email — all four overlap
	// ThreadHistory's, so the union is exactly these five, not nine. Asserting
	// the exact slice (not
	// just "sorted" and "no scope repeated") is what actually discriminates a
	// correct ScopesFor from a broken one: sort.StringsAreSorted(nil) is
	// true and a duplicate-count loop over an empty slice never runs, so a
	// ScopesFor that always returned nil would have passed the weaker form
	// of this assertion.
	got := channelkinds.ScopesFor(k, []channelfeatures.Feature{
		channelfeatures.ThreadHistory,
		channelfeatures.ChannelHistory,
	})

	assert.Equal(t, []string{
		"channels:history", "groups:history", "im:history", "users:read", "users:read.email",
	}, got)
}

func TestScopesForIgnoresUnsupportedFeatures(t *testing.T) {
	k, ok := registry.Get("fake")
	require.True(t, ok)
	assert.Empty(t, channelkinds.ScopesFor(k, channelfeatures.All()),
		"a kind with no transport permissions requires no scopes for any feature")
}

package registry_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"

	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/bento"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/local"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
)

// TestNeedsWebhook_AgreesWithTheKindItself is derived from the registry rather
// than from a list of names, so a kind added later is covered without editing
// this test — which is the whole property NeedsWebhook exists to give its
// callers.
func TestNeedsWebhook_AgreesWithTheKindItself(t *testing.T) {
	kinds := registry.All()
	require.NotEmpty(t, kinds,
		"the kind registry must be populated by init — an empty registry would make every row below vacuous")

	var anyWebhookKind bool
	for _, k := range kinds {
		want := k.WebhookReceiver(channelkinds.Deps{}) != nil
		anyWebhookKind = anyWebhookKind || want
		t.Run(k.Name(), func(t *testing.T) {
			assert.Equal(t, want, registry.NeedsWebhook(k.Name()),
				"NeedsWebhook must answer exactly what the kind's own WebhookReceiver does")
		})
	}
	assert.True(t, anyWebhookKind,
		"at least one registered kind must receive webhooks, or the equality above holds only in the false direction")
}

// TestNeedsWebhook_UnregisteredKindReportsFalse: the callers that ask this hold
// a string from a CR or a bundle, and each already refuses an unknown kind in
// words that name it. Answering false here keeps this from becoming a second,
// differently-shaped failure for the same wiring bug.
func TestNeedsWebhook_UnregisteredKindReportsFalse(t *testing.T) {
	assert.False(t, registry.NeedsWebhook("nosuchkind"))
	assert.False(t, registry.NeedsWebhook(""))
}

package fake_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/outputbind"

	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake" // register the kind
)

func fakeOutputChannel(name string) *spiceboxv1alpha1.Channel {
	return &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind: "fake",
			Role: spiceboxv1alpha1.ChannelRoleOutput,
			Fake: &spiceboxv1alpha1.FakeChannelConfig{},
		},
	}
}

// TestOutboundAnchor_ReachedThroughTheRegistry is the assertion that matters:
// the callers (channelsd's inbound pipeline, the Channel controller's two
// destination rules) never type-assert the kind themselves — they go through
// outputbind.Anchor, which probes the registry for the optional interface. A
// method declared but not reachable that way would leave every split-channel
// fake session undeliverable exactly as before.
func TestOutboundAnchor_ReachedThroughTheRegistry(t *testing.T) {
	key, external, err := outputbind.Anchor(fakeOutputChannel("demo-out"))
	require.NoError(t, err,
		"a fake output Channel that cannot answer makes channelsd refuse to create the session")
	assert.Equal(t, "channel:demo-out", key,
		"mirrors slack's bare-channel anchor shape rather than one unique to this kind")
	assert.Equal(t, map[string]string{"channel_id": "demo-out"}, external,
		"channel_id is this kind's own convention — ChannelViewSubjectRef reads the same key")
}

// TestOutboundAnchor_IsAPureFunctionOfTheChannel pins the interface's stated
// requirement. The Channel controller derives the binding at apply time and
// channelsd derives it again at session creation; a value that moved between
// those two would bind a session to a destination its own Valid condition was
// computed against a different version of.
func TestOutboundAnchor_IsAPureFunctionOfTheChannel(t *testing.T) {
	first, firstExt, err := outputbind.Anchor(fakeOutputChannel("demo-out"))
	require.NoError(t, err)
	second, secondExt, err := outputbind.Anchor(fakeOutputChannel("demo-out"))
	require.NoError(t, err)

	assert.Equal(t, first, second)
	assert.Equal(t, firstExt, secondExt)
}

// TestOutboundAnchor_DistinctPerChannel: the anchor is the destination, so two
// Channels must not collide on one. The name is the only identifier a fake
// Channel has, which is exactly why it is the one used.
func TestOutboundAnchor_DistinctPerChannel(t *testing.T) {
	a, _, err := outputbind.Anchor(fakeOutputChannel("demo-out"))
	require.NoError(t, err)
	b, _, err := outputbind.Anchor(fakeOutputChannel("other-out"))
	require.NoError(t, err)
	assert.NotEqual(t, a, b)
}

// TestOutboundAnchor_RefusesAnUnnamedChannel covers the one error path. It is
// unreachable through the API server (metadata.name is required) and is here so
// an in-memory caller gets a refusal rather than the key "channel:", which
// every unnamed Channel would then share.
func TestOutboundAnchor_RefusesAnUnnamedChannel(t *testing.T) {
	ch := fakeOutputChannel("")
	_, _, err := outputbind.Anchor(ch)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "metadata.name")
}

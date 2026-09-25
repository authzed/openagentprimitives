package hooks

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// The channel destination must be a faithful wrapper: same subjects, same
// capability, same error. Anything else changes the gate that protects every
// channel reply today.
func TestChannelDestinationCarriesResolveAudienceVerbatim(t *testing.T) {
	want := []string{"user:ann", "user:bo"}
	d := &channelDestination{resolve: func(context.Context) ([]string, channelkinds.Capability, error) {
		return want, channelkinds.CapabilityFull, nil
	}}

	got, err := d.Audience(context.Background())
	require.NoError(t, err)
	assert.Equal(t, want, got)
	assert.Equal(t, channelkinds.CapabilityFull, d.Capability())
	assert.Equal(t, "channel", d.Describe())
}

// A resolver error must surface as an error, never as an empty audience: an
// empty audience passes the gate vacuously, so degrading an error into one
// would open it.
func TestChannelDestinationReportsAnErrorRatherThanAnEmptyAudience(t *testing.T) {
	d := &channelDestination{resolve: func(context.Context) ([]string, channelkinds.Capability, error) {
		return nil, channelkinds.CapabilityUnsupported, assert.AnError
	}}
	got, err := d.Audience(context.Background())
	require.Error(t, err, "an audience we could not compute is unknown, not empty")
	assert.Nil(t, got)
}

// The underlying closure returns audience and capability together, so the
// destination resolves once and memoizes. Without that, the gate would call
// it twice per evaluation — once for the bypass branch, once for the
// comparison — doubling a SpiceDB round trip on every channel reply.
func TestChannelDestinationResolvesOnceAndMemoizes(t *testing.T) {
	calls := 0
	d := &channelDestination{resolve: func(context.Context) ([]string, channelkinds.Capability, error) {
		calls++
		return nil, channelkinds.CapabilitySingleUser, nil
	}}
	_ = d.Capability()
	_, _ = d.Audience(context.Background())
	_ = d.Capability()
	assert.Equal(t, 1, calls, "resolve once, then answer from the memo")
}

// A nil resolver must REFUSE, not panic.
//
// It reaches this type because the `ResolveAudience == nil` bypass lives on
// the channel leg alone — it cannot sit in the shared gate without silently
// skipping a pool destination — so nothing short-circuits before a destination
// is built, and calling a nil func here would take down a security gate.
func TestChannelDestinationWithNoResolverRefusesRatherThanPanics(t *testing.T) {
	d := &channelDestination{}

	assert.NotPanics(t, func() {
		got, err := d.Audience(context.Background())
		require.Error(t, err, "no resolver ⇒ the audience is unknown, not empty")
		assert.Nil(t, got)
	})
}

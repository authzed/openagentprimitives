package capability

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	corev1 "k8s.io/api/core/v1"
)

func assertErr() error { return errors.New("boom") }

func TestThreadHistoryOnlyForMentionOnly(t *testing.T) {
	c, ok := Lookup("thread_history")
	require.True(t, ok)
	assert.True(t, c.DefaultOn())

	// non-mention_only → nothing
	tools, skip := c.Offer(OfferContext{Ctx: context.Background(), Binding: &spiceboxv1alpha1.ChannelBinding{RoutingMode: "all"}})
	assert.Nil(t, skip)
	assert.Empty(t, tools)

	// no binding at all (kubectl-driven) → nothing
	tools, skip = c.Offer(OfferContext{Ctx: context.Background(), Binding: nil})
	assert.Nil(t, skip)
	assert.Empty(t, tools)

	// mention_only → read_thread_history
	tools, skip = c.Offer(OfferContext{
		Ctx:     context.Background(),
		Binding: &spiceboxv1alpha1.ChannelBinding{RoutingMode: "mention_only"},
		Env:     RunnerEnv{SubjectPrefix: "p", NATSRequest: nil},
	})
	assert.Nil(t, skip)
	assert.Equal(t, []string{"read_thread_history"}, toolNames(tools))
}

func TestMentionLookupSkipsOnResolveError(t *testing.T) {
	c, ok := Lookup("mention_lookup")
	require.True(t, ok)
	tools, skip := c.Offer(OfferContext{
		Ctx:     context.Background(),
		Binding: &spiceboxv1alpha1.ChannelBinding{},
		Env:     RunnerEnv{ResolveErr: assertErr()},
	})
	require.NotNil(t, skip)
	assert.Equal(t, "mention_lookup", skip.Capability)
	assert.Empty(t, tools)
}

func TestMentionLookupInactiveWithoutBinding(t *testing.T) {
	c, _ := Lookup("mention_lookup")
	tools, skip := c.Offer(OfferContext{Ctx: context.Background(), Binding: nil})
	assert.Nil(t, skip)
	assert.Empty(t, tools)
}

func TestMentionLookupNilWhenKindUnsupported(t *testing.T) {
	// fake.Kind advertises no SupportedMentionLookups, so
	// NewLookupUserForMention returns nil and the capability offers nothing
	// (not a skip — a granted-but-declining-kind is a normal inactive state).
	c, _ := Lookup("mention_lookup")
	tools, skip := c.Offer(OfferContext{
		Ctx:     context.Background(),
		Binding: &spiceboxv1alpha1.ChannelBinding{},
		Env: RunnerEnv{
			ResolvedChannel: &spiceboxv1alpha1.Channel{Spec: spiceboxv1alpha1.ChannelSpec{Kind: "fake"}},
			ResolvedSecret:  &corev1.Secret{},
			ResolvedKind:    fake.Kind{},
		},
	})
	assert.Nil(t, skip)
	assert.Empty(t, tools)
}

func TestChannelHistoryInactiveWithoutBinding(t *testing.T) {
	c, ok := Lookup("channel_history")
	require.True(t, ok)
	tools, skip := c.Offer(OfferContext{Ctx: context.Background(), Binding: nil})
	assert.Nil(t, skip)
	assert.Empty(t, tools)
}

func TestChannelHistorySkipsOnResolveError(t *testing.T) {
	c, _ := Lookup("channel_history")
	tools, skip := c.Offer(OfferContext{
		Ctx:     context.Background(),
		Binding: &spiceboxv1alpha1.ChannelBinding{},
		Env:     RunnerEnv{ResolveErr: assertErr()},
	})
	// channel_history treats resolve errors as a quiet inactive state, not a
	// SkipReason — mirrors main.go:584's `if err == nil` gate (the warning is
	// already logged once, by mention_lookup's Offer, for the same err).
	assert.Nil(t, skip)
	assert.Empty(t, tools)
}

func TestChannelHistoryEmptyWhenChannelOptsOut(t *testing.T) {
	// Channel.Spec.ChannelHistory is nil → channelhistorygate.Offer returns
	// ok=false → capability contributes nothing.
	c, _ := Lookup("channel_history")
	tools, skip := c.Offer(OfferContext{
		Ctx:     context.Background(),
		Binding: &spiceboxv1alpha1.ChannelBinding{},
		Session: &spiceboxv1alpha1.AgentSession{},
		Env: RunnerEnv{
			ResolvedChannel: &spiceboxv1alpha1.Channel{Spec: spiceboxv1alpha1.ChannelSpec{Kind: "fake"}},
			ResolvedKind:    fake.Kind{},
		},
	})
	assert.Nil(t, skip)
	assert.Empty(t, tools)
}

func TestChannelHistoryOffersToolWhenOptedIn(t *testing.T) {
	// Positive path: fake.Kind implements channelkinds.ChannelHistoryReader
	// (pkg/channels/channelkinds/fake/channel_history.go), so this exercises
	// channelhistorygate.Offer's ok=true branch end-to-end without a
	// hand-rolled fixture. Output==input (OutputChannel nil, InputChannel
	// set) satisfies the no-leakage-gate same-channel condition.
	c, _ := Lookup("channel_history")
	tools, skip := c.Offer(OfferContext{
		Ctx:     context.Background(),
		Binding: &spiceboxv1alpha1.ChannelBinding{},
		Session: &spiceboxv1alpha1.AgentSession{
			Spec: spiceboxv1alpha1.AgentSessionSpec{
				InputChannel: &spiceboxv1alpha1.ChannelBinding{Name: "c1"},
			},
		},
		Class: &spiceboxv1alpha1.AgentClass{},
		Env: RunnerEnv{
			SubjectPrefix: "p",
			ResolvedChannel: &spiceboxv1alpha1.Channel{
				Spec: spiceboxv1alpha1.ChannelSpec{
					Kind:           "fake",
					ChannelHistory: &spiceboxv1alpha1.ChannelHistorySpec{Enabled: true},
				},
			},
			ResolvedKind: fake.Kind{},
		},
	})
	assert.Nil(t, skip)
	assert.Equal(t, []string{"read_channel_history"}, toolNames(tools))
}

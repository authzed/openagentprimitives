package channelhistorygate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/local"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
)

// enabledHistoryChannel builds a Channel with spec.channelHistory opted in.
func enabledHistoryChannel() *spiceboxv1alpha1.Channel {
	return &spiceboxv1alpha1.Channel{
		Spec: spiceboxv1alpha1.ChannelSpec{
			ChannelHistory: &spiceboxv1alpha1.ChannelHistorySpec{Enabled: true},
		},
	}
}

// leakageClass builds an AgentClass with authz.informationLeakage.mode set to
// mode ("" leaves the block absent entirely, resolving to "disabled").
func leakageClass(mode string) *spiceboxv1alpha1.AgentClass {
	class := &spiceboxv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "cls"}}
	if mode != "" {
		class.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{
			InformationLeakage: &spiceboxv1alpha1.InformationLeakagePolicy{Mode: mode},
		}
	}
	return class
}

// sessionWithChannels builds an AgentSession whose InputChannel is always
// "ch-in" (channel_id C1); OutputChannel is nil (same as input, per
// ChannelBinding.SameChannelAs) unless sameOutput is false, in which case it
// points at a distinct channel_id.
func sessionWithChannels(sameOutput bool) *spiceboxv1alpha1.AgentSession {
	input := &spiceboxv1alpha1.ChannelBinding{Name: "ch-in", Kind: "slack", External: map[string]string{"channel_id": "C1"}}
	var output *spiceboxv1alpha1.ChannelBinding
	if !sameOutput {
		output = &spiceboxv1alpha1.ChannelBinding{Name: "ch-out", Kind: "slack", External: map[string]string{"channel_id": "C2"}}
	}
	return &spiceboxv1alpha1.AgentSession{
		Spec: spiceboxv1alpha1.AgentSessionSpec{InputChannel: input, OutputChannel: output},
	}
}

func TestOffer(t *testing.T) {
	cases := []struct {
		name      string
		ch        *spiceboxv1alpha1.Channel
		kind      channelkinds.Kind
		sess      *spiceboxv1alpha1.AgentSession
		class     *spiceboxv1alpha1.AgentClass
		wantOffer bool
	}{
		{
			name:      "enabled + slack + leakage off + output==input -> offered",
			ch:        enabledHistoryChannel(),
			kind:      &slack.Kind{},
			sess:      sessionWithChannels(true),
			class:     leakageClass(""),
			wantOffer: true,
		},
		{
			name:      "enabled + slack + leakage off + output!=input -> NOT offered",
			ch:        enabledHistoryChannel(),
			kind:      &slack.Kind{},
			sess:      sessionWithChannels(false),
			class:     leakageClass(""),
			wantOffer: false,
		},
		{
			name:      "enabled + slack + leakage on -> offered (regardless of output/input)",
			ch:        enabledHistoryChannel(),
			kind:      &slack.Kind{},
			sess:      sessionWithChannels(false),
			class:     leakageClass("enforcing"),
			wantOffer: true,
		},
		{
			name:      "enabled + local (not a ChannelHistoryReader) -> NOT offered",
			ch:        enabledHistoryChannel(),
			kind:      &local.Kind{},
			sess:      sessionWithChannels(true),
			class:     leakageClass("enforcing"),
			wantOffer: false,
		},
		{
			name:      "ChannelHistory nil (disabled) -> NOT offered",
			ch:        &spiceboxv1alpha1.Channel{},
			kind:      &slack.Kind{},
			sess:      sessionWithChannels(true),
			class:     leakageClass("enforcing"),
			wantOffer: false,
		},
		{
			name: "ChannelHistory.Enabled=false -> NOT offered",
			ch: &spiceboxv1alpha1.Channel{
				Spec: spiceboxv1alpha1.ChannelSpec{ChannelHistory: &spiceboxv1alpha1.ChannelHistorySpec{Enabled: false}},
			},
			kind:      &slack.Kind{},
			sess:      sessionWithChannels(true),
			class:     leakageClass("enforcing"),
			wantOffer: false,
		},
		{
			name:      "nil class + leakage-off default + output==input -> offered",
			ch:        enabledHistoryChannel(),
			kind:      &slack.Kind{},
			sess:      sessionWithChannels(true),
			class:     nil,
			wantOffer: true,
		},
		{
			name:      "nil class + leakage-off default + output!=input -> NOT offered",
			ch:        enabledHistoryChannel(),
			kind:      &slack.Kind{},
			sess:      sessionWithChannels(false),
			class:     nil,
			wantOffer: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reader, ok := Offer(c.ch, c.kind, c.sess, c.class)
			assert.Equal(t, c.wantOffer, ok, "offer decision mismatch")
			if c.wantOffer {
				assert.NotNil(t, reader, "expected a non-nil reader when offered")
			} else {
				assert.Nil(t, reader, "expected a nil reader when not offered")
			}
		})
	}
}

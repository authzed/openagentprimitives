package slack

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

func si(external, anno string) channelkinds.SessionInfo {
	s := channelkinds.SessionInfo{Channel: &v1alpha1.ChannelBinding{External: map[string]string{"channel_id": "D01"}}}
	if external != "" {
		s.Channel.External["thread_ts"] = external
	}
	if anno != "" {
		s.Annotations = map[string]string{LastInboundTSAnnotationKey: anno}
	}
	return s
}

func TestEffectiveOutboundThreadTS(t *testing.T) {
	cases := []struct{ name, external, anno, want string }{
		{"DM with per-turn annotation, no External: uses annotation", "", "111.1", "111.1"},
		{"DM without annotation or External: empty (first-send capture preserved)", "", "", ""},
		{"channel: annotation==External: yields the anchor", "222.2", "222.2", "222.2"},
		{"legacy binding, no annotation: falls back to External", "333.3", "", "333.3"},
		{"annotation wins over stale External (per-turn freshness)", "444.4", "555.5", "555.5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, effectiveOutboundThreadTS(si(tc.external, tc.anno)))
		})
	}
}

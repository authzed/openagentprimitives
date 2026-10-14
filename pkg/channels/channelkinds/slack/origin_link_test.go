package slack

import (
	"testing"

	"github.com/stretchr/testify/assert"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestKind_OriginLink(t *testing.T) {
	k := &Kind{}
	t.Run("persisted permalink present", func(t *testing.T) {
		u, ok := k.OriginLink(&spiceboxv1alpha1.ChannelBinding{Kind: "slack",
			External: map[string]string{"permalink": "https://demo.slack.com/archives/C1/p1700000000000100"}})
		assert.True(t, ok)
		assert.Equal(t, "https://demo.slack.com/archives/C1/p1700000000000100", u)
	})
	t.Run("no permalink yet: ok=false", func(t *testing.T) {
		_, ok := k.OriginLink(&spiceboxv1alpha1.ChannelBinding{Kind: "slack",
			External: map[string]string{"channel_id": "C1", "thread_ts": "1700000000.000100"}})
		assert.False(t, ok)
	})
	t.Run("nil binding: ok=false", func(t *testing.T) {
		_, ok := k.OriginLink(nil)
		assert.False(t, ok)
	})
}

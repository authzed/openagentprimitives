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
	t.Run("interactive session: reconstruct from the routing tuple when no permalink", func(t *testing.T) {
		// A human-initiated Slack session's binding carries team_id+channel_id+
		// thread_ts (stamped by the listener at creation) but no sender-resolved
		// permalink — the thread predates the agent. OriginLink reconstructs a
		// thread deep-link so the artifact view still links back to the thread.
		u, ok := k.OriginLink(&spiceboxv1alpha1.ChannelBinding{Kind: "slack",
			External: map[string]string{"team_id": "T1", "channel_id": "C1", "thread_ts": "1700000000.000100"}})
		assert.True(t, ok)
		assert.Equal(t, "https://app.slack.com/client/T1/C1/thread/C1-1700000000.000100", u)
	})
	t.Run("permalink wins over reconstruction", func(t *testing.T) {
		u, ok := k.OriginLink(&spiceboxv1alpha1.ChannelBinding{Kind: "slack",
			External: map[string]string{"team_id": "T1", "channel_id": "C1", "thread_ts": "1700.1",
				"permalink": "https://demo.slack.com/archives/C1/p17001"}})
		assert.True(t, ok)
		assert.Equal(t, "https://demo.slack.com/archives/C1/p17001", u)
	})
	t.Run("no permalink and incomplete tuple (no team_id): ok=false", func(t *testing.T) {
		_, ok := k.OriginLink(&spiceboxv1alpha1.ChannelBinding{Kind: "slack",
			External: map[string]string{"channel_id": "C1", "thread_ts": "1700000000.000100"}})
		assert.False(t, ok)
	})
	t.Run("nil binding: ok=false", func(t *testing.T) {
		_, ok := k.OriginLink(nil)
		assert.False(t, ok)
	})
}

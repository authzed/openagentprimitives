package slack

import (
	"testing"

	"github.com/stretchr/testify/assert"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

func TestKind_OriginLink(t *testing.T) {
	k := &Kind{}
	t.Run("persisted permalink present", func(t *testing.T) {
		link, ok := k.OriginLink(&spiceboxv1alpha1.ChannelBinding{Kind: "slack",
			External: map[string]string{"permalink": "https://demo.slack.com/archives/C1/p1700000000000100"}})
		assert.True(t, ok)
		assert.Equal(t, channelkinds.OriginLink{URL: "https://demo.slack.com/archives/C1/p1700000000000100",
			Label: "Open the Slack thread", Icon: "slack"}, link)
	})
	t.Run("interactive session: reconstruct from the routing tuple when no permalink", func(t *testing.T) {
		// A human-initiated Slack session's binding carries team_id+channel_id+
		// thread_ts (stamped by the listener at creation) but no sender-resolved
		// permalink — the thread predates the agent. OriginLink reconstructs a
		// thread deep-link so the artifact view still links back to the thread.
		link, ok := k.OriginLink(&spiceboxv1alpha1.ChannelBinding{Kind: "slack",
			External: map[string]string{"team_id": "T1", "channel_id": "C1", "thread_ts": "1700000000.000100"}})
		assert.True(t, ok)
		assert.Equal(t, channelkinds.OriginLink{URL: "https://app.slack.com/client/T1/C1/thread/C1-1700000000.000100",
			Label: "Open the Slack thread", Icon: "slack"}, link)
	})
	t.Run("permalink wins over reconstruction", func(t *testing.T) {
		link, ok := k.OriginLink(&spiceboxv1alpha1.ChannelBinding{Kind: "slack",
			External: map[string]string{"team_id": "T1", "channel_id": "C1", "thread_ts": "1700.1",
				"permalink": "https://demo.slack.com/archives/C1/p17001"}})
		assert.True(t, ok)
		assert.Equal(t, "https://demo.slack.com/archives/C1/p17001", link.URL)
	})
	t.Run("untrusted permalink host: ignored, falls back to reconstruction", func(t *testing.T) {
		// External is user-writable. A permalink that is not a *.slack.com https
		// URL is not trusted — the reconstructed deep link is used instead.
		link, ok := k.OriginLink(&spiceboxv1alpha1.ChannelBinding{Kind: "slack",
			External: map[string]string{"team_id": "T1", "channel_id": "C1", "thread_ts": "1700.1",
				"permalink": "https://evil.example/login"}})
		assert.True(t, ok)
		assert.Equal(t, "https://app.slack.com/client/T1/C1/thread/C1-1700.1", link.URL)
	})
	t.Run("javascript: permalink and no tuple: ok=false", func(t *testing.T) {
		_, ok := k.OriginLink(&spiceboxv1alpha1.ChannelBinding{Kind: "slack",
			External: map[string]string{"permalink": "javascript:alert(1)"}})
		assert.False(t, ok)
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

func TestIsSlackURL(t *testing.T) {
	cases := []struct {
		name string
		u    string
		want bool
	}{
		{"team permalink host", "https://demo.slack.com/archives/C1/p1", true},
		{"app deep link host", "https://app.slack.com/client/T1/C1/thread/C1-1", true},
		{"bare slack.com", "https://slack.com/x", true},
		{"http not https", "http://demo.slack.com/x", false},
		{"foreign host", "https://evil.example/login", false},
		{"slack.com substring attack", "https://slack.com.evil.example/x", false},
		{"javascript scheme", "javascript:alert(1)", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isSlackURL(tc.u))
		})
	}
}

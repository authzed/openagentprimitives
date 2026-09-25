package channelevents

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWebhookInboundPayload_RoundTrips(t *testing.T) {
	in := WebhookInboundPayload{
		ChannelNamespace: "default", ChannelName: "demo-reviewbot-gh", ChannelKind: "github",
		ChannelKey: "pr:demo-org/platform#42", MessageText: "PR 42 opened", AuthzSubject: "service:reviewbot",
	}
	raw, err := json.Marshal(in)
	require.NoError(t, err)

	var out WebhookInboundPayload
	require.NoError(t, json.Unmarshal(raw, &out))
	assert.Equal(t, in, out)
}

func TestWebhookInboundSubject_IsClusterWideNotSessionScoped(t *testing.T) {
	// A delivery arrives before any session exists, so the subject cannot be
	// session-scoped the way ap.session.*.*.in.* subjects are.
	assert.Equal(t, "ap.channel.webhook_inbound", WebhookInboundSubject)
	assert.NotContains(t, WebhookInboundSubject, "*")
}

func TestWebhookInboundQueueGroup_IsSet(t *testing.T) {
	// The queue group is what actually prevents duplicate sessions across
	// replicated channelsd pods — the subject constant alone doesn't.
	assert.Equal(t, "channelsd-webhook-inbound", WebhookInboundQueueGroup)
	assert.NotEmpty(t, WebhookInboundQueueGroup)
}

package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// TestWebhookInboundHandler_DerivesOriginURL pins the handoff that lets the
// artifact-view page link back to the PR: the github receiver's
// TriggerOriginURLProvider derives the PR html_url from the verified delivery,
// and the handler places it on InboundEvent.External["origin_url"], which the
// pipeline copies onto the session's input binding.
func TestWebhookInboundHandler_DerivesOriginURL(t *testing.T) {
	cli := fake.NewClientBuilder().WithScheme(attachScheme(t)).WithObjects(webhookTestChannel()).Build()
	pipe := &fakeWebhookPipeline{dec: channelkinds.InboundDecision{Outcome: channelkinds.OutcomeRouted}}
	logger, _ := captureLogger()
	payload := webhookTestPayload()
	payload.DeliveryEvent = "pull_request"
	payload.RawDelivery = []byte(`{"action":"opened","number":42,` +
		`"pull_request":{"html_url":"https://github.com/demo-org/platform/pull/42",` +
		`"user":{"login":"demo-user","id":4172237}},` +
		`"repository":{"full_name":"demo-org/platform"}}`)
	raw, err := json.Marshal(payload)
	require.NoError(t, err)

	webhookInboundHandler(context.Background(), cli, pipe, logger, &nats.Msg{Data: raw})

	require.True(t, pipe.Called())
	assert.Equal(t, "https://github.com/demo-org/platform/pull/42",
		pipe.observed().External["origin_url"],
		"the PR html_url, derived from the verified delivery, must reach the session input binding")
}

// A delivery the provider cannot derive an origin URL from still dispatches —
// the origin link is additive, never a gate — and an undecodable delivery is
// logged, not silent.
func TestWebhookInboundHandler_OriginURLDerivationFailure_StillDispatches(t *testing.T) {
	cases := []struct {
		name        string
		event       string
		rawDelivery []byte
		wantLog     string
	}{
		{name: "non-pull_request event: no origin_url, no log", event: "ping",
			rawDelivery: []byte(`{"zen":"x"}`)},
		{name: "undecodable delivery: no origin_url, logged", event: "pull_request",
			rawDelivery: []byte("not json"), wantLog: "origin url derivation failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cli := fake.NewClientBuilder().WithScheme(attachScheme(t)).WithObjects(webhookTestChannel()).Build()
			pipe := &fakeWebhookPipeline{dec: channelkinds.InboundDecision{Outcome: channelkinds.OutcomeRouted}}
			logger, read := captureLogger()
			payload := webhookTestPayload()
			payload.DeliveryEvent = tc.event
			payload.RawDelivery = tc.rawDelivery
			raw, err := json.Marshal(payload)
			require.NoError(t, err)

			webhookInboundHandler(context.Background(), cli, pipe, logger, &nats.Msg{Data: raw})

			require.True(t, pipe.Called(), "the session must still start; the origin link is additive, not a gate")
			assert.NotContains(t, pipe.observed().External, "origin_url")
			if tc.wantLog != "" {
				assert.Contains(t, read(), tc.wantLog)
			}
		})
	}
}

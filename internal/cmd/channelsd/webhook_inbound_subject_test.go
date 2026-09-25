package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// The webhook-inbound payload carried the acting subject, and channelsd used it
// verbatim as the new session's started_by. But the payload arrives over NATS,
// and holding a bus grant is not the same as being webd: any publisher on the
// inbound subject could spawn a session AS ANY SUBJECT — a session whose owner,
// approver set, and transcript readership are all the attacker's choice.
//
// The subject was never per-delivery data in the first place. github's receiver
// reads it straight off ch.Spec.AuthzSubject, and channelsd already fetches that
// Channel CR to dispatch at all. So the CR is the authority and the payload
// field is corroboration: a disagreement is either a forged publish or a version
// skew, and neither should start a session.
func TestWebhookInboundHandler_TakesTheSubjectFromTheChannelCRNotThePayload(t *testing.T) {
	ch := webhookTestChannel()
	ch.Spec.AuthzSubject = "service:reviewbot"
	cli := fake.NewClientBuilder().WithScheme(attachScheme(t)).WithObjects(ch).Build()
	pipe := &fakeWebhookPipeline{dec: channelkinds.InboundDecision{Outcome: channelkinds.OutcomeRouted}}
	logger, logs := captureLogger()

	// A forged publish naming somebody else.
	payload := webhookTestPayload()
	payload.AuthzSubject = "user:victim"
	raw, err := json.Marshal(payload)
	require.NoError(t, err)

	webhookInboundHandler(context.Background(), cli, pipe, logger, &nats.Msg{Data: raw})

	assert.False(t, pipe.Called(),
		"a delivery naming a subject the Channel does not declare must not start a session")
	assert.True(t, strings.Contains(logs(), "authzSubject"),
		"the refusal is logged with the claimed subject: on this path there is no caller to answer")
}

// The honest case: webd publishes exactly what the Channel declares, and the
// delivery dispatches with that subject.
func TestWebhookInboundHandler_MatchingSubjectDispatches(t *testing.T) {
	ch := webhookTestChannel()
	ch.Spec.AuthzSubject = "service:reviewbot"
	cli := fake.NewClientBuilder().WithScheme(attachScheme(t)).WithObjects(ch).Build()
	pipe := &fakeWebhookPipeline{dec: channelkinds.InboundDecision{Outcome: channelkinds.OutcomeRouted}}
	logger, _ := captureLogger()

	raw, err := json.Marshal(webhookTestPayload())
	require.NoError(t, err)

	webhookInboundHandler(context.Background(), cli, pipe, logger, &nats.Msg{Data: raw})

	require.True(t, pipe.Called())
	assert.Equal(t, "service:reviewbot", pipe.observed().AuthzSubject,
		"the subject the pipeline sees comes from the Channel CR")
}

// A Channel that declares NO subject cannot attribute a webhook delivery to
// anyone. Letting the payload fill that gap is exactly the hole — an
// unattributed Channel would be the easiest one to spawn sessions through — so
// a payload subject against a subject-less Channel is refused too.
func TestWebhookInboundHandler_SubjectlessChannelRefusesAPayloadSubject(t *testing.T) {
	ch := webhookTestChannel()
	ch.Spec.AuthzSubject = "" // this Channel attributes its deliveries to nobody
	cli := fake.NewClientBuilder().WithScheme(attachScheme(t)).WithObjects(ch).Build()
	pipe := &fakeWebhookPipeline{dec: channelkinds.InboundDecision{Outcome: channelkinds.OutcomeRouted}}
	logger, _ := captureLogger()

	raw, err := json.Marshal(webhookTestPayload()) // carries "service:reviewbot"
	require.NoError(t, err)

	webhookInboundHandler(context.Background(), cli, pipe, logger, &nats.Msg{Data: raw})

	assert.False(t, pipe.Called(),
		"a Channel declaring no subject must not have one supplied for it over the bus")
}

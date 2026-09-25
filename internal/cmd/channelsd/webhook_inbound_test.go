package main

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	natstest "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// fakeWebhookPipeline is a stub channelkinds.InboundPipeline that records the
// InboundEvent it was handed (so a test can assert exact field values, not
// merely that Deliver was called) and returns a fixed decision/error.
// The mutex is load-bearing, not decoration: the queue-subscribe test below
// drives Deliver from nats.go's own delivery goroutine while the test
// goroutine polls for completion, so every field here is written and read
// across a goroutine boundary. Without it `go test -race` — which is what
// `mage test:unit` runs — reports a genuine data race on `called`.
type fakeWebhookPipeline struct {
	dec channelkinds.InboundDecision
	err error

	mu     sync.Mutex
	called bool
	got    channelkinds.InboundEvent
}

func (f *fakeWebhookPipeline) Deliver(_ context.Context, ev channelkinds.InboundEvent) (channelkinds.InboundDecision, error) {
	f.mu.Lock()
	f.called = true
	f.got = ev
	f.mu.Unlock()
	return f.dec, f.err
}

// Called reports whether Deliver ran, and observed returns what it was handed.
// Accessors rather than direct field reads so no caller can forget the lock.
func (f *fakeWebhookPipeline) Called() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.called
}

func (f *fakeWebhookPipeline) observed() channelkinds.InboundEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.got
}

func webhookTestChannel() *spiceboxv1alpha1.Channel {
	return &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "demo-reviewbot-gh"},
		// AuthzSubject matches webhookTestPayload: webd only ever publishes the
		// Channel CR's own subject, and channelsd refuses a delivery that
		// claims a different one (see webhook_inbound_subject_test.go).
		Spec: spiceboxv1alpha1.ChannelSpec{Kind: "github", AuthzSubject: "service:reviewbot"},
	}
}

func webhookTestPayload() channelevents.WebhookInboundPayload {
	return channelevents.WebhookInboundPayload{
		ChannelNamespace: "default",
		ChannelName:      "demo-reviewbot-gh",
		ChannelKind:      "github",
		ChannelKey:       "pr:demo-org/platform#42",
		MessageText:      "PR 42 opened",
		AuthzSubject:     "service:reviewbot",
	}
}

func TestWebhookInboundHandler_ResolvesChannelAndDispatchesToPipeline(t *testing.T) {
	ch := webhookTestChannel()
	cli := fake.NewClientBuilder().WithScheme(attachScheme(t)).WithObjects(ch).Build()
	pipe := &fakeWebhookPipeline{dec: channelkinds.InboundDecision{Outcome: channelkinds.OutcomeRouted}}
	logger, _ := captureLogger()
	payload := webhookTestPayload()
	raw, err := json.Marshal(payload)
	require.NoError(t, err)

	webhookInboundHandler(context.Background(), cli, pipe, logger, &nats.Msg{Data: raw})

	require.True(t, pipe.Called(), "Deliver must be called for a resolvable Channel CR")
	assert.Equal(t, "demo-reviewbot-gh", pipe.observed().Channel.Name, "must dispatch with the Channel CR named by the payload")
	assert.Equal(t, "default", pipe.observed().Channel.Namespace)
	assert.Equal(t, "pr:demo-org/platform#42", pipe.observed().ChannelKey, "ChannelKey must pass through verbatim")
	assert.Equal(t, "PR 42 opened", pipe.observed().MessageText, "MessageText must pass through verbatim")
	assert.Equal(t, "service:reviewbot", pipe.observed().AuthzSubject,
		"AuthzSubject comes from the Channel CR, which is the authority for it — the bus payload is not")
}

// TestWebhookInboundHandler_DerivesTriggerOwnerAnnotation pins the handoff
// that makes a PR author a session owner: the github receiver's
// TriggerOwnerProvider derives "github_user:<id>#user" from the verified
// delivery, and the handler places it in PreTurnAnnotations so session
// creation stamps it atomically — the operator's owner resolver reads it from
// there. Derivation happens HERE, at the writer, from the same RawDelivery
// bytes TriggerFacts uses, so no security-relevant derived value travels
// between components as data.
func TestWebhookInboundHandler_DerivesTriggerOwnerAnnotation(t *testing.T) {
	cli := fake.NewClientBuilder().WithScheme(attachScheme(t)).WithObjects(webhookTestChannel()).Build()
	pipe := &fakeWebhookPipeline{dec: channelkinds.InboundDecision{Outcome: channelkinds.OutcomeRouted}}
	logger, _ := captureLogger()
	payload := webhookTestPayload()
	payload.DeliveryEvent = "pull_request"
	payload.RawDelivery = []byte(`{"action":"opened","number":42,` +
		`"pull_request":{"user":{"login":"demo-user","id":4172237},` +
		`"head":{"sha":"a1b2c3d4e5f60718293a4b5c6d7e8f9012345678","repo":{"full_name":"demo-org/platform","fork":false}}},` +
		`"repository":{"full_name":"demo-org/platform"}}`)
	raw, err := json.Marshal(payload)
	require.NoError(t, err)

	webhookInboundHandler(context.Background(), cli, pipe, logger, &nats.Msg{Data: raw})

	require.True(t, pipe.Called())
	assert.Equal(t, "github_user:4172237#user",
		pipe.observed().PreTurnAnnotations[spiceboxv1alpha1.AnnotationTriggerOwnerSubject],
		"the PR author's account, derived from the verified delivery, must reach the session as a pre-turn annotation")
}

// A delivery the provider cannot derive an owner from — or one whose
// derivation errors — still dispatches: the owner grant is an addition to the
// session, never a gate on it. The error case is logged, not silent.
func TestWebhookInboundHandler_OwnerDerivationFailure_StillDispatches(t *testing.T) {
	cases := []struct {
		name        string
		event       string
		rawDelivery []byte
		wantLog     string
	}{
		{name: "no account in the payload: no annotation, no log", event: "pull_request",
			rawDelivery: []byte(`{"action":"opened","number":42,"pull_request":{"user":{"login":"demo-user"}},"repository":{"full_name":"demo-org/platform"}}`)},
		{name: "undecodable delivery: no annotation, logged", event: "pull_request",
			rawDelivery: []byte("not json"), wantLog: "trigger owner derivation failed"},
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

			require.True(t, pipe.Called(), "the session must still start; the owner grant is additive, not a gate")
			assert.NotContains(t, pipe.observed().PreTurnAnnotations, spiceboxv1alpha1.AnnotationTriggerOwnerSubject)
			if tc.wantLog != "" {
				assert.Contains(t, read(), tc.wantLog)
			}
		})
	}
}

func TestWebhookInboundHandler_UndecodablePayload_LogsAndDoesNotDispatch(t *testing.T) {
	cli := fake.NewClientBuilder().WithScheme(attachScheme(t)).WithObjects(webhookTestChannel()).Build()
	pipe := &fakeWebhookPipeline{}
	logger, read := captureLogger()

	assert.NotPanics(t, func() {
		webhookInboundHandler(context.Background(), cli, pipe, logger, &nats.Msg{Data: []byte("{not json")})
	}, "an undecodable payload must be logged and dropped, never panic")

	assert.False(t, pipe.Called(), "Deliver must not run for a payload that failed to decode")
	assert.Contains(t, read(), "undecodable payload",
		"the failure must be logged — there is no caller to return the error to")
}

func TestWebhookInboundHandler_MissingChannelCR_LogsAndDoesNotDispatch(t *testing.T) {
	// No objects registered: the fake client's Get for demo-reviewbot-gh fails.
	cli := fake.NewClientBuilder().WithScheme(attachScheme(t)).Build()
	pipe := &fakeWebhookPipeline{}
	logger, read := captureLogger()
	raw, err := json.Marshal(webhookTestPayload())
	require.NoError(t, err)

	assert.NotPanics(t, func() {
		webhookInboundHandler(context.Background(), cli, pipe, logger, &nats.Msg{Data: raw})
	}, "a missing Channel CR must be logged and dropped, never panic")

	assert.False(t, pipe.Called(), "Deliver must not run when the Channel CR cannot be resolved")
	got := read()
	assert.Contains(t, got, "Channel CR lookup failed")
	assert.Contains(t, got, "demo-reviewbot-gh", "the log must name which channel failed to resolve")
}

func TestWebhookInboundHandler_DeliverError_LogsAndDoesNotDispatchTwice(t *testing.T) {
	cli := fake.NewClientBuilder().WithScheme(attachScheme(t)).WithObjects(webhookTestChannel()).Build()
	pipe := &fakeWebhookPipeline{err: errors.New("dispatch failed")}
	logger, read := captureLogger()
	raw, err := json.Marshal(webhookTestPayload())
	require.NoError(t, err)

	assert.NotPanics(t, func() {
		webhookInboundHandler(context.Background(), cli, pipe, logger, &nats.Msg{Data: raw})
	}, "a Deliver error must be logged and dropped, never panic")

	assert.True(t, pipe.Called(), "Deliver must have been attempted")
	got := read()
	assert.Contains(t, got, "pipeline.Deliver errored")
	assert.Contains(t, got, "pr:demo-org/platform#42", "the log must name which delivery failed")
}

func TestWebhookInboundHandler_InternalErrorOutcome_Logs(t *testing.T) {
	cli := fake.NewClientBuilder().WithScheme(attachScheme(t)).WithObjects(webhookTestChannel()).Build()
	pipe := &fakeWebhookPipeline{dec: channelkinds.InboundDecision{Outcome: channelkinds.OutcomeInternalError}}
	logger, read := captureLogger()
	raw, err := json.Marshal(webhookTestPayload())
	require.NoError(t, err)

	webhookInboundHandler(context.Background(), cli, pipe, logger, &nats.Msg{Data: raw})

	assert.Contains(t, read(), "pipeline returned InternalError",
		"a successful-but-internal-error decision must still be logged, not silently absorbed")
}

// TestWebhookInboundSubscription_IsQueueSubscribe_NotPlainSubscribe wires the
// subscription exactly as main.go does and asserts the structural fact that
// actually prevents duplicate sessions across replicated channelsd pods: a
// nats.Subscription created via QueueSubscribe carries its queue group name
// on Sub.Queue, which a plain Subscribe leaves empty. It then publishes a
// delivery end-to-end and asserts it reaches the pipeline, proving the wiring
// (not just the constants) works.
func TestWebhookInboundSubscription_IsQueueSubscribe_NotPlainSubscribe(t *testing.T) {
	srv := natstest.RunServer(&server.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true})
	t.Cleanup(srv.Shutdown)
	nc, err := nats.Connect(srv.ClientURL())
	require.NoError(t, err, "nats.Connect")
	t.Cleanup(nc.Close)

	cli := fake.NewClientBuilder().WithScheme(attachScheme(t)).WithObjects(webhookTestChannel()).Build()
	pipe := &fakeWebhookPipeline{dec: channelkinds.InboundDecision{Outcome: channelkinds.OutcomeRouted}}
	logger, _ := captureLogger()

	// Verbatim from internal/cmd/channelsd/main.go's subscription wiring.
	sub, err := nc.QueueSubscribe(channelevents.WebhookInboundSubject, channelevents.WebhookInboundQueueGroup,
		func(m *nats.Msg) { webhookInboundHandler(context.Background(), cli, pipe, logger, m) })
	require.NoError(t, err, "QueueSubscribe")
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, nc.Flush(), "flush so the subscription is registered server-side")

	require.Equal(t, channelevents.WebhookInboundQueueGroup, sub.Queue,
		"a plain Subscribe leaves Queue empty; this must be a queue subscription or N channelsd "+
			"replicas would each create a session for the same webhook delivery")

	raw, err := json.Marshal(webhookTestPayload())
	require.NoError(t, err)
	require.NoError(t, nc.Publish(channelevents.WebhookInboundSubject, raw))
	require.NoError(t, nc.Flush())

	deadline := time.After(5 * time.Second)
	for {
		if pipe.Called() {
			assert.Equal(t, "pr:demo-org/platform#42", pipe.observed().ChannelKey)
			return
		}
		select {
		case <-deadline:
			t.Fatal("a webhook payload published on WebhookInboundSubject never reached the pipeline")
		case <-time.After(25 * time.Millisecond):
		}
	}
}

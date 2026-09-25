package livemirror

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	natstest "github.com/nats-io/nats-server/v2/test"
	natsgo "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// This test mirrors internal/cmd/webd/main_test.go's WatchMessages/WatchSessionStatus
// NATS fixtures (same embedded nats-server + dual-conn pattern) so the
// extraction is a verified parity move, not a rewrite.

func TestWatchOutbound_DecodesRequestedKindsAndStopsAfterCancel(t *testing.T) {
	srv := natstest.RunServer(&natsserver.Options{Port: -1})
	defer srv.Shutdown()
	subConn, err := natsgo.Connect(srv.ClientURL())
	require.NoError(t, err)
	defer subConn.Close()
	pubConn, err := natsgo.Connect(srv.ClientURL())
	require.NoError(t, err)
	defer pubConn.Close()

	const ns, name = "default", "s1"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := WatchOutbound(ctx, subConn, ns, name, channelevents.KindUserMessage, channelevents.KindPlanUpdate)
	require.NoError(t, err)
	require.NotNil(t, ch)
	require.NoError(t, subConn.Flush()) // subscriptions registered server-side before we publish

	publish := func(subj string, data []byte) error { return pubConn.Publish(subj, data) }

	require.NoError(t, channelevents.PublishOut(publish, ns, name, channelevents.KindUserMessage,
		channelevents.OutboundUserMessagePayload{Text: "here is your answer"}))
	require.NoError(t, pubConn.Flush())

	select {
	case env := <-ch:
		assert.Equal(t, channelevents.KindUserMessage, env.Kind)
		var pl channelevents.OutboundUserMessagePayload
		require.NoError(t, json.Unmarshal(env.Payload, &pl))
		assert.Equal(t, "here is your answer", pl.Text)
	case <-time.After(2 * time.Second):
		t.Fatal("no envelope emitted for out.user_message")
	}

	require.NoError(t, channelevents.PublishOut(publish, ns, name, channelevents.KindPlanUpdate,
		channelevents.PlanUpdatePayload{PlanName: "demo-plan"}))
	require.NoError(t, pubConn.Flush())

	select {
	case env := <-ch:
		assert.Equal(t, channelevents.KindPlanUpdate, env.Kind)
		var pl channelevents.PlanUpdatePayload
		require.NoError(t, json.Unmarshal(env.Payload, &pl))
		assert.Equal(t, "demo-plan", pl.PlanName)
	case <-time.After(2 * time.Second):
		t.Fatal("no envelope emitted for out.plan_update")
	}

	// A kind never passed to WatchOutbound must never be delivered.
	require.NoError(t, channelevents.PublishOut(publish, ns, name, channelevents.KindNotification,
		channelevents.NotificationPayload{Text: "not requested"}))
	require.NoError(t, pubConn.Flush())
	select {
	case env := <-ch:
		t.Fatalf("unexpected envelope for a kind never subscribed: %+v", env)
	case <-time.After(200 * time.Millisecond):
		// expected: no delivery
	}

	cancel()
	require.NoError(t, subConn.Flush())
	// Give the ctx-cancel goroutine a moment to unsubscribe before publishing
	// again, so the assertion below actually exercises the "stopped" behavior
	// rather than a race with delivery.
	time.Sleep(50 * time.Millisecond)

	require.NoError(t, channelevents.PublishOut(publish, ns, name, channelevents.KindUserMessage,
		channelevents.OutboundUserMessagePayload{Text: "after cancel"}))
	require.NoError(t, pubConn.Flush())

	select {
	case env, ok := <-ch:
		if ok {
			t.Fatalf("received an envelope after ctx cancel: %+v", env)
		}
	case <-time.After(200 * time.Millisecond):
		// expected: no further emit once ctx is cancelled
	}
}

// A per-session subscription pins the subject, so anything delivered on it
// belongs to that session — but Envelope.Session inside the body is still
// publisher-controlled JSON, and a runner holds publish on its own "out.>"
// subtree. Forwarding the envelope verbatim would let a session label its own
// live-view events with ANOTHER session's namespace/name, which the session-view
// page writes straight to the authorized viewer's websocket. The mislabelled
// envelope is dropped, not relabelled: a publisher and its own subject
// disagreeing is a bug or an attack, never normal traffic.
func TestWatchOutbound_DropsEnvelopeClaimingAnotherSession(t *testing.T) {
	srv := natstest.RunServer(&natsserver.Options{Port: -1})
	defer srv.Shutdown()
	subConn, err := natsgo.Connect(srv.ClientURL())
	require.NoError(t, err)
	defer subConn.Close()
	pubConn, err := natsgo.Connect(srv.ClientURL())
	require.NoError(t, err)
	defer pubConn.Close()

	const ns, name = "default", "s1"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := WatchOutbound(ctx, subConn, ns, name, channelevents.KindNotification)
	require.NoError(t, err)
	require.NotNil(t, ch)
	require.NoError(t, subConn.Flush())

	// Built for a DIFFERENT session, published on s1's own subject.
	forged, err := channelevents.BuildEnvelope(ns, "s2", channelevents.KindNotification,
		channelevents.NotificationPayload{Text: "labelled with someone else's session"})
	require.NoError(t, err)
	body, err := json.Marshal(forged)
	require.NoError(t, err)
	require.NoError(t, pubConn.Publish(
		channelevents.SubjectOut(channelevents.SubjectPrefix(ns, name), channelevents.KindNotification), body))
	require.NoError(t, pubConn.Flush())

	select {
	case env := <-ch:
		t.Fatalf("an envelope claiming another session must never be forwarded: %+v", env.Session)
	case <-time.After(300 * time.Millisecond):
		// expected: dropped
	}

	// The honest publish on the same subscription must still arrive — proving
	// the check refuses the mismatch rather than the subject.
	require.NoError(t, channelevents.PublishOut(
		func(subj string, data []byte) error { return pubConn.Publish(subj, data) },
		ns, name, channelevents.KindNotification,
		channelevents.NotificationPayload{Text: "honest"}))
	require.NoError(t, pubConn.Flush())

	select {
	case env := <-ch:
		assert.Equal(t, ns, env.Session.Namespace)
		assert.Equal(t, name, env.Session.Name)
	case <-time.After(2 * time.Second):
		t.Fatal("the honest envelope was not emitted")
	}
}

func TestWatchOutbound_SubscribeErrorClosesChannel(t *testing.T) {
	ctx := context.Background()
	ch, err := WatchOutbound(ctx, failingSubscriber{}, "default", "s1", channelevents.KindUserMessage)
	require.Error(t, err)
	assert.Nil(t, ch)
}

// failingSubscriber is a NATSSubscriber stub whose every Subscribe call
// errors, exercising WatchOutbound's fail-closed cleanup path (no leaked
// subscriptions, channel not returned) without a live NATS connection.
type failingSubscriber struct{}

func (failingSubscriber) Subscribe(subj string, cb natsgo.MsgHandler) (*natsgo.Subscription, error) {
	return nil, errSubscribeFailed
}

var errSubscribeFailed = errors.New("subscribe failed")

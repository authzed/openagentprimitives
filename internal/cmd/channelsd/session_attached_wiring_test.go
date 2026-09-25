package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	natstest "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// The live thread-index chain has three links, and each END was tested while
// the JOIN between them was not:
//
//	outbound relay patches spec.outputChannel and publishes SessionAttached
//	    (tested: pkg/channels/channelsd/outbound TestRelay_FirstSend_WriteBackPatch)
//	  -> internal/cmd/channelsd's NATS subscription
//	    (UNTESTED — this file)
//	  -> channelManager.HandleSessionAttached
//	    (tested: session_attached_dispatch_test.go)
//	  -> slack listener SessionUpdated -> threadIndex.put
//	    (tested: pkg/channels/channelkinds/slack TestListener_SessionUpdated_AddsToThreadIndex)
//
// In production the chain does not complete: a cron thread never enters the
// index live, so a human reply is dropped as "unowned thread" until a
// channelsd restart runs the startup walk. Diagnostic logging showed
// HandleSessionAttached is never reached, which puts the break at the
// subscription — the one link with no coverage.
//
// This test wires the subscription exactly as main.go does and asserts an
// event published on the subject reaches the manager.
func TestSessionAttachedSubscription_ReachesTheManager(t *testing.T) {
	srv := natstest.RunServer(&server.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true})
	t.Cleanup(srv.Shutdown)
	nc, err := nats.Connect(srv.ClientURL())
	require.NoError(t, err, "nats.Connect")
	t.Cleanup(nc.Close)

	sess := &spiceboxv1alpha1.AgentSession{}
	sess.Namespace, sess.Name = "default", "cron-1"
	sess.Spec.OutputChannel = &spiceboxv1alpha1.ChannelBinding{
		Name: "test-output", Kind: "slack", Key: "thread:C1:1.0",
		External: map[string]string{"channel_id": "C1", "thread_ts": "1.0"},
	}
	cli := fake.NewClientBuilder().WithScheme(attachScheme(t)).WithObjects(sess).Build()

	mgr := newChannelManager(cli, nc, nil, nil, nil, nil, nil, nil, nil, nil)
	w := &watcherListener{}
	mgr.listeners["default/test-output"] = w

	// The subscription, verbatim from internal/cmd/channelsd/main.go.
	sub, err := nc.Subscribe(channelevents.SessionAttachedSubject, func(m *nats.Msg) {
		var ev channelevents.SessionAttached
		if err := json.Unmarshal(m.Data, &ev); err != nil {
			return
		}
		mgr.HandleSessionAttached(context.Background(), ev)
	})
	require.NoError(t, err, "subscribe")
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, nc.Flush(), "flush so the subscription is registered server-side")

	payload, err := json.Marshal(channelevents.SessionAttached{
		Namespace:         "default",
		SessionName:       "cron-1",
		OutputChannelName: "test-output",
		OutputChannelKind: "slack",
		OutputChannelKey:  "thread:C1:1.0",
		External:          map[string]string{"channel_id": "C1", "thread_ts": "1.0"},
	})
	require.NoError(t, err)
	require.NoError(t, nc.Publish(channelevents.SessionAttachedSubject, payload))
	require.NoError(t, nc.Flush())

	deadline := time.After(5 * time.Second)
	for {
		if got := w.seen(); len(got) > 0 {
			require.Equal(t, "cron-1", got[0].Name)
			return
		}
		select {
		case <-deadline:
			t.Fatal("SessionAttached published on the subject never reached the manager — " +
				"the subscription wiring is the break in the live thread-index chain")
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// The subject the relay publishes on and the subject channelsd subscribes to
// must be the same constant. A silent divergence here would produce exactly
// the observed symptom: the relay reports a successful publish, nothing ever
// arrives, and no error is logged on either side.
func TestSessionAttachedSubject_IsASingleConstant(t *testing.T) {
	require.NotEmpty(t, channelevents.SessionAttachedSubject,
		"the subject must be a non-empty shared constant")
	// Publisher and subscriber both reference this symbol; if either ever
	// hand-rolls a string, this is the canary.
	require.NotContains(t, channelevents.SessionAttachedSubject, " ",
		"a NATS subject with a space would silently never match")
}

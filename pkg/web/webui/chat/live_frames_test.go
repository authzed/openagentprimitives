// pkg/web/webui/chat/live_frames_test.go
//
// Browser-visible live-frame flow: these tests wire the real production shape
// (embedded NATS + real outbound.Relay + the Registry as SenderResolver + a
// real *browser.Host → recording sink, with OnTurnActivity routed exactly as
// registry.startRelay does) and publish the `.out.*` envelopes a runner emits
// during a turn. They assert the exact browser.Msg* frames a browser tab would
// receive — the ground truth for "did the agent reply / turn-complete actually
// reach the chat UI live", which the reported "stuck starting" / "stuck
// working" regressions are about.
package chat

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/outbound"
)

// newLiveChatFixture wires one live session end-to-end: a real *browser.Host
// bound to a recording sink, the Registry as the relay's SenderResolver, and
// OnTurnActivity routed as production does. Returns the registry, a live NATS
// conn to publish `.out.*` on, and the sink the browser frames land in.
func newLiveChatFixture(t *testing.T, name, chanName string) (*Registry, *nats.Conn, *testSink) {
	t.Helper()
	k8s := newFakeK8sClient(t, chatAgentSession(name, chanName))

	entry := newSessionEntry(newChatSessionNamespace, name, "user:owner")
	entry.senders = newTestBrowserHost(t, nil, chanName, name, entry)
	sink := &testSink{}
	require.True(t, entry.attach(sink))

	reg := newRegistryWithBuilder(&fakeDeps{k8s: k8s}, newFakeBuilder().build)
	putTestEntry(reg, newChatKey(name), entry)

	nc := connectTestNATS(t)
	relay := &outbound.Relay{NC: nc, K8s: k8s, Senders: reg, OnTurnActivity: reg.onTurnActivity}
	require.NoError(t, relay.Start(context.Background()))
	t.Cleanup(func() { _ = relay.Stop(context.Background()) })
	return reg, nc, sink
}

func hasUserMessage(events []any, text string) bool {
	for _, e := range events {
		if m, ok := e.(browser.MsgUserMessage); ok && m.Text == text {
			return true
		}
	}
	return false
}

func hasTurnComplete(events []any) bool {
	for _, e := range events {
		if m, ok := e.(browser.MsgTurnActivity); ok && !m.Active {
			return true
		}
	}
	return false
}

// TestLiveFrames_FirstTurnSequence_AllReachBrowser mimics the browser-visible
// frame sequence of a first message: the user's own send is mirrored back
// (user_echo), the agent replies (user_message), and the turn completes
// (turn_activity active:false, which clears the "working" throbber). Every
// frame must reach the attached sink — a lost reply is the "stuck starting"
// regression, a lost turn-complete is the "stuck working while idle" one.
func TestLiveFrames_FirstTurnSequence_AllReachBrowser(t *testing.T) {
	const name, chanName = "sess-1", "chan-1"
	_, nc, sink := newLiveChatFixture(t, name, chanName)

	echo, err := channelevents.BuildEnvelope(newChatSessionNamespace, name, channelevents.KindUserEcho,
		channelevents.UserEchoPayload{Text: "hello", RequestID: "req-1", Via: "urn:ap:view:chat"})
	require.NoError(t, err)
	publishChatOut(t, nc, name, echo)

	reply, err := channelevents.BuildEnvelope(newChatSessionNamespace, name, channelevents.KindUserMessage,
		channelevents.OutboundUserMessagePayload{Text: "hi there"})
	require.NoError(t, err)
	publishChatOut(t, nc, name, reply)

	ta, err := channelevents.BuildEnvelope(newChatSessionNamespace, name, channelevents.KindTurnActivity,
		channelevents.TurnActivityPayload{Active: false, Cause: "awaiting_user_message"})
	require.NoError(t, err)
	publishChatOut(t, nc, name, ta)

	require.Eventually(t, func() bool {
		ev, _ := sink.snapshot()
		return hasUserMessage(ev, "hi there") && hasTurnComplete(ev)
	}, 3*time.Second, 10*time.Millisecond,
		"the agent reply AND the turn-complete must both reach the browser sink")
}

// TestLiveFrames_ReplyDeliveredAfterUserEcho isolates the "stuck starting"
// concern: a user_echo (the web chat's own message, mirrored back) published
// immediately BEFORE the agent's reply must not swallow or block the reply —
// both frames reach the browser.
func TestLiveFrames_ReplyDeliveredAfterUserEcho(t *testing.T) {
	const name, chanName = "sess-1", "chan-1"
	_, nc, sink := newLiveChatFixture(t, name, chanName)

	echo, err := channelevents.BuildEnvelope(newChatSessionNamespace, name, channelevents.KindUserEcho,
		channelevents.UserEchoPayload{Text: "hello", RequestID: "req-1", Via: "urn:ap:view:chat"})
	require.NoError(t, err)
	publishChatOut(t, nc, name, echo)

	reply, err := channelevents.BuildEnvelope(newChatSessionNamespace, name, channelevents.KindUserMessage,
		channelevents.OutboundUserMessagePayload{Text: "the answer"})
	require.NoError(t, err)
	publishChatOut(t, nc, name, reply)

	require.Eventually(t, func() bool {
		ev, _ := sink.snapshot()
		return hasUserMessage(ev, "the answer")
	}, 3*time.Second, 10*time.Millisecond,
		"the agent reply must reach the browser even when a user_echo precedes it")
}

// TestLiveFrames_TurnActivityComplete_ReachesBrowser isolates the "stuck
// working" concern: turn_activity(active:false) — the coarse turn-complete
// signal — must reach the browser sink as MsgTurnActivity so the UI clears its
// throbber even for a turn that produced no final user_message.
func TestLiveFrames_TurnActivityComplete_ReachesBrowser(t *testing.T) {
	const name, chanName = "sess-1", "chan-1"
	_, nc, sink := newLiveChatFixture(t, name, chanName)

	ta, err := channelevents.BuildEnvelope(newChatSessionNamespace, name, channelevents.KindTurnActivity,
		channelevents.TurnActivityPayload{Active: false, Cause: "awaiting_user_message"})
	require.NoError(t, err)
	publishChatOut(t, nc, name, ta)

	require.Eventually(t, func() bool {
		ev, _ := sink.snapshot()
		return hasTurnComplete(ev)
	}, 3*time.Second, 10*time.Millisecond,
		"turn_activity(active:false) must reach the browser to clear the working throbber")
}

package chat

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	natstest "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/outbound"
)

// --- test helpers ------------------------------------------------------

// connectTestNATS starts an embedded, in-process nats-server and returns a
// client connection — mirrors pkg/channels/channelsd/outbound's relay_test.go helper
// (no live cluster; every dependency here is a fake or an in-memory server).
func connectTestNATS(t *testing.T) *nats.Conn {
	t.Helper()
	srv := natstest.RunServer(&server.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true})
	t.Cleanup(srv.Shutdown)
	nc, err := nats.Connect(srv.ClientURL())
	require.NoError(t, err, "nats.Connect")
	t.Cleanup(nc.Close)
	return nc
}

// chatAgentSession builds the minimal AgentSession the outbound relay needs
// to route: channel-attached (non-nil InputChannel), in the chat namespace,
// named exactly like the registry key it corresponds to.
func chatAgentSession(name, channelName string) *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: newChatSessionNamespace},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class: "demo-agent",
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: channelName,
				Kind: browser.KindName,
			},
		},
	}
}

// newTestBrowserHost builds a real *browser.Host (the same type
// wireSessionEntry wires into sessionEntry.senders in production) bound to
// sink. Using the real Host — not a fake resolver — is deliberate: Host's
// SenderFor/SubChannelSenderFor/StreamDeltaSinkFor all IGNORE the
// AgentSession argument they're given (by design: "a browser session always
// routes to this one in-process host"). That's only safe because the
// Registry scopes delivery to the correct Host BEFORE calling into it (see
// sender_resolver.go) — this test would catch a regression in that scoping
// even though the Host itself never looks at which session it was asked
// about.
//
// nc may be nil for the outbound-only tests: the Host's SENDER side never
// publishes, so only a test exercising its Listener (which publishes IN
// envelopes) needs a live connection.
func newTestBrowserHost(t *testing.T, nc *nats.Conn, channelName, sessionName string, sink browser.EventSink) *browser.Host {
	t.Helper()
	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: channelName, Namespace: newChatSessionNamespace},
		Spec:       spiceboxv1alpha1.ChannelSpec{Kind: browser.KindName},
	}
	deps := channelkinds.Deps{Channel: ch}
	if nc != nil {
		deps.NATSPublish = func(subj string, p []byte) error { return nc.Publish(subj, p) }
	}
	host, err := browser.NewHost(browser.HostConfig{
		Deps:        deps,
		Sink:        sink,
		User:        channelkinds.ExternalIdentity{Kind: browser.KindName, ExternalID: "local-user"},
		Namespace:   newChatSessionNamespace,
		SessionName: sessionName,
		Via:         chatVia(t),
	})
	require.NoError(t, err)
	return host
}

// publishChatOut marshals + publishes an outbound envelope on the same
// subject the runner/pipeline would use for a real session
// ("ap.session.<ns>.<name>.out.<kind>"), then flushes so the relay's
// subscription sees it deterministically before the test moves on.
func publishChatOut(t *testing.T, nc *nats.Conn, name string, env channelevents.Envelope) {
	t.Helper()
	subj := channelevents.SubjectOut(channelevents.SubjectPrefix(newChatSessionNamespace, name), env.Kind)
	data, err := json.Marshal(env)
	require.NoError(t, err)
	require.NoError(t, nc.Publish(subj, data))
	require.NoError(t, nc.Flush())
}

// --- Fix 1 regression: outbound envelopes must not leak across sessions --

// TestRegistry_OutboundRelay_ScopesDeliveryToOwningSession is the MANDATORY
// regression guard for the critical cross-session leak: prior to the fix,
// browser.Host ignored the AgentSession it was asked to resolve a Sender
// for, and each chat session ran its OWN outbound.Relay independently
// subscribed to the cluster-wide "ap.session.*.*.out.>" wildcard — so with N
// concurrent conversations, every session's outbound envelope reached every
// OTHER session's browser sink too (plus N× duplicate delivery to the
// correct one).
//
// This test wires the real production shape post-fix: ONE outbound.Relay,
// with the Registry itself as the SenderResolver (sender_resolver.go), two
// live sessionEntries each bound to a distinct sink via a real
// *browser.Host. It publishes one outbound envelope for session A over a
// real (embedded) NATS connection and asserts:
//  1. Session A's sink receives exactly one copy (no N× duplicate).
//  2. Session B's sink receives NOTHING — this is the leak this test guards.
func TestRegistry_OutboundRelay_ScopesDeliveryToOwningSession(t *testing.T) {
	const nameA, chanA = "sess-a", "chan-a"
	const nameB, chanB = "sess-b", "chan-b"

	k8s := newFakeK8sClient(t,
		chatAgentSession(nameA, chanA),
		chatAgentSession(nameB, chanB),
	)

	entryA := newSessionEntry(newChatSessionNamespace, nameA, "user:owner")
	entryA.senders = newTestBrowserHost(t, nil, chanA, nameA, entryA)
	sinkA := &testSink{}
	require.True(t, entryA.attach(sinkA))

	entryB := newSessionEntry(newChatSessionNamespace, nameB, "user:owner")
	entryB.senders = newTestBrowserHost(t, nil, chanB, nameB, entryB)
	sinkB := &testSink{}
	require.True(t, entryB.attach(sinkB))

	reg := newRegistryWithBuilder(&fakeDeps{k8s: k8s}, newFakeBuilder().build)
	putTestEntry(reg, newChatKey(nameA), entryA)
	putTestEntry(reg, newChatKey(nameB), entryB)

	nc := connectTestNATS(t)
	relay := &outbound.Relay{NC: nc, K8s: k8s, Senders: reg}
	require.NoError(t, relay.Start(context.Background()))
	t.Cleanup(func() { _ = relay.Stop(context.Background()) })

	env, err := channelevents.BuildEnvelope(newChatSessionNamespace, nameA, channelevents.KindUserMessage,
		channelevents.OutboundUserMessagePayload{Text: "hello from A"})
	require.NoError(t, err)
	publishChatOut(t, nc, nameA, env)

	require.Eventually(t, func() bool {
		events, _ := sinkA.snapshot()
		return len(events) >= 1
	}, 2*time.Second, 10*time.Millisecond, "session A's own sink must receive its envelope")

	// Give any errant cross-delivery (the bug this test guards) time to
	// land before asserting its absence.
	time.Sleep(200 * time.Millisecond)

	eventsA, _ := sinkA.snapshot()
	assert.Len(t, eventsA, 1, "session A's sink must receive exactly one copy — no duplicate delivery")
	assert.IsType(t, browser.MsgUserMessage{}, eventsA[0])

	eventsB, _ := sinkB.snapshot()
	assert.Empty(t, eventsB, "session B's sink must receive NOTHING from session A's envelope — cross-session leak regression guard")
}

// TestRegistry_OutboundRelay_ScopesDeliveryToOwningSession_Reverse is the
// symmetric case (publish for B, assert A is untouched) — guards against a
// fix that happens to work in one direction only (e.g. an off-by-one in
// which map entry is consulted first).
func TestRegistry_OutboundRelay_ScopesDeliveryToOwningSession_Reverse(t *testing.T) {
	const nameA, chanA = "sess-a", "chan-a"
	const nameB, chanB = "sess-b", "chan-b"

	k8s := newFakeK8sClient(t,
		chatAgentSession(nameA, chanA),
		chatAgentSession(nameB, chanB),
	)

	entryA := newSessionEntry(newChatSessionNamespace, nameA, "user:owner")
	entryA.senders = newTestBrowserHost(t, nil, chanA, nameA, entryA)
	sinkA := &testSink{}
	require.True(t, entryA.attach(sinkA))

	entryB := newSessionEntry(newChatSessionNamespace, nameB, "user:owner")
	entryB.senders = newTestBrowserHost(t, nil, chanB, nameB, entryB)
	sinkB := &testSink{}
	require.True(t, entryB.attach(sinkB))

	reg := newRegistryWithBuilder(&fakeDeps{k8s: k8s}, newFakeBuilder().build)
	putTestEntry(reg, newChatKey(nameA), entryA)
	putTestEntry(reg, newChatKey(nameB), entryB)

	nc := connectTestNATS(t)
	relay := &outbound.Relay{NC: nc, K8s: k8s, Senders: reg}
	require.NoError(t, relay.Start(context.Background()))
	t.Cleanup(func() { _ = relay.Stop(context.Background()) })

	env, err := channelevents.BuildEnvelope(newChatSessionNamespace, nameB, channelevents.KindUserMessage,
		channelevents.OutboundUserMessagePayload{Text: "hello from B"})
	require.NoError(t, err)
	publishChatOut(t, nc, nameB, env)

	require.Eventually(t, func() bool {
		events, _ := sinkB.snapshot()
		return len(events) >= 1
	}, 2*time.Second, 10*time.Millisecond, "session B's own sink must receive its envelope")

	time.Sleep(200 * time.Millisecond)

	eventsB, _ := sinkB.snapshot()
	assert.Len(t, eventsB, 1, "session B's sink must receive exactly one copy — no duplicate delivery")

	eventsA, _ := sinkA.snapshot()
	assert.Empty(t, eventsA, "session A's sink must receive NOTHING from session B's envelope — cross-session leak regression guard")
}

// TestRegistry_SenderFor_UnknownSession_DropsSilently exercises the
// resolver's fail-closed default directly: an AgentSession the registry has
// never heard of (or has already torn down) must resolve to (nil, nil) so
// outbound.Relay drops the envelope, never routing it to an arbitrary Host.
func TestRegistry_SenderFor_UnknownSession_DropsSilently(t *testing.T) {
	reg := newRegistryWithBuilder(&fakeDeps{k8s: newFakeK8sClient(t)}, newFakeBuilder().build)

	unknown := chatAgentSession("does-not-exist", "chan-x")
	sender, err := reg.SenderFor(context.Background(), unknown)
	require.NoError(t, err)
	assert.Nil(t, sender)

	subSender, err := reg.SubChannelSenderFor(context.Background(), unknown, "tool_session")
	require.NoError(t, err)
	assert.Nil(t, subSender)

	sink, err := reg.StreamDeltaSinkFor(context.Background(), unknown)
	require.NoError(t, err)
	assert.Nil(t, sink)
}

// TestRegistry_SenderFor_ReservedPlaceholder_DropsSilently mirrors
// entryForSession's contract for a session whose Adopt is still in
// flight (a nil placeholder) — the same "drop, don't misroute" behavior as
// an unknown session, guarding against a race where an envelope for a
// half-built session reaches a Host before it exists.
func TestRegistry_SenderFor_ReservedPlaceholder_DropsSilently(t *testing.T) {
	reg := newRegistryWithBuilder(&fakeDeps{k8s: newFakeK8sClient(t)}, newFakeBuilder().build)
	putTestReservation(reg, newChatKey("in-flight"), "user:in-flight")

	sess := chatAgentSession("in-flight", "chan-x")
	sender, err := reg.SenderFor(context.Background(), sess)
	require.NoError(t, err)
	assert.Nil(t, sender)
}

// --- Fix 3: Adopt must not insert into a shut-down registry --------------

// TestRegistry_AdoptSession_ClosedDuringWiring_StopsInProcessButPreservesCR
// simulates Shutdown flipping the closed flag WHILE a build is in flight — the
// exact race the fix closes. Before the fix, Shutdown's snapshot of "sessions
// to tear down" only ever looked at already-non-nil entries, so a reserved
// (entry-less) slot for an Adopt in flight was invisible to it;
// publish's own post-wire check only asked "is my slot still in the
// map" (yes — nothing removed it), so it would happily publish a live entry
// into a registry whose relay and reaper had already stopped, leaking the
// relay wiring forever.
//
// The abort must do IN-PROCESS cleanup only: stop the relay/watcher and drop
// the reserved placeholder, but NEVER delete the freshly-created AgentSession
// CR. The operator owns CR lifecycle; a webd restart racing a just-started
// session must not make that session vanish (that is the "stuck on starting,
// then gone" class of bug).
func TestRegistry_AdoptSession_ClosedDuringWiring_StopsInProcessButPreservesCR(t *testing.T) {
	k8s := newFakeK8sClient(t, validAgentClass("demo-agent"))
	fb := newFakeBuilder()
	reg := newRegistryWithBuilder(&fakeDeps{k8s: k8s}, fb.build)

	fb.mu.Lock()
	fb.beforeReturn = func() {
		reg.mu.Lock()
		reg.closed = true
		reg.mu.Unlock()
	}
	fb.mu.Unlock()

	name, err := reg.adoptNewTestSession(context.Background(), "demo-agent", "hi", "user:owner")
	require.Error(t, err)
	assert.Empty(t, name)
	assert.ErrorIs(t, err, ErrRegistryClosed)

	require.Len(t, fb.built, 1, "the build ran and created its CR before the shutdown was observed")
	built := fb.built[0]
	// In-process cleanup happens (stop), but the CR is preserved (no deleteK8s).
	assert.True(t, fb.wasStopped(built), "the in-flight entry's stop() must be called when the registry closed mid-build")
	assert.False(t, fb.wasDeletedK8s(built),
		"a shutdown landing mid-build must NOT delete the session CR (operator owns CR lifecycle)")

	// No trace of the session — including the reserved placeholder — must
	// remain in the in-memory table.
	reg.mu.Lock()
	_, present := reg.sessions[newChatKey(built)]
	sessionCount := len(reg.sessions)
	reg.mu.Unlock()
	assert.False(t, present)
	assert.Zero(t, sessionCount, "the reserved placeholder must not survive a close-during-build race")
}

// TestRegistry_AdoptSession_AfterShutdown_RejectedUpFront covers the
// simpler, non-racy case: Adopt called after Shutdown has already
// completed must fail fast (at slot reservation) rather than attempt a
// build that will only be torn down afterward.
func TestRegistry_AdoptSession_AfterShutdown_RejectedUpFront(t *testing.T) {
	k8s := newFakeK8sClient(t, validAgentClass("demo-agent"))
	fb := newFakeBuilder()
	reg := newRegistryWithBuilder(&fakeDeps{k8s: k8s}, fb.build)
	reg.startReaper()
	reg.shutdown(context.Background())

	_, err := reg.adoptNewTestSession(context.Background(), "demo-agent", "hi", "user:owner")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRegistryClosed)
	assert.Empty(t, fb.built, "the builder must never run once the registry is closed")
}

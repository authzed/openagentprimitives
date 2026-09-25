// pkg/web/webui/chat/webchat_e2e_test.go
//
// End-to-end through the ENTIRE browser-facing web-chat stack, wired exactly as
// production does: embedded NATS + a real outbound.Relay (with OnTurnActivity) +
// a real *browser.Host sender + the real wsHandler over a real websocket + a
// real watchSessionHealth polling a real (fake-client) AgentSession. A test
// publishes the `.out.*` frames a runner emits and drives the AgentSession phase,
// then asserts what a browser tab actually RECEIVES over /chat/ws.
//
// This is the seam the split unit tests missed: it exercises frame delivery,
// the ws keepalive, the turn-complete backstop, and the wake-latch together,
// end to end, instead of one layer at a time.
package chat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/outbound"
	"github.com/authzed/openagentprimitives/pkg/web/viewurn"
	"github.com/authzed/openagentprimitives/pkg/web/webui/internal/wstest"
)

// chatVia is the web chat surface's view URN — the same value wireSessionEntry
// mints and stamps on every envelope the chat publishes.
func chatVia(t *testing.T) string {
	t.Helper()
	via, err := viewurn.Format(viewurn.TypeChat, "", "")
	require.NoError(t, err)
	return via
}

// webChatE2E holds the wired-up stack for one session.
type webChatE2E struct {
	reg  *Registry
	nc   *nats.Conn
	k8s  client.Client
	name string
	srv  *httptest.Server
}

// combinedE2ESession is an AgentSession CR that carries BOTH the Spec the
// outbound relay routes on (InputChannel = browser) AND the Status.Phase
// watchSessionHealth polls.
func combinedE2ESession(name, chanName, phase string) *spiceboxv1alpha1.AgentSession {
	s := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: newChatSessionNamespace},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:        "demo-agent",
			InputChannel: &spiceboxv1alpha1.ChannelBinding{Name: chanName, Kind: browser.KindName},
		},
	}
	s.Status.Phase = phase
	return s
}

// startWebChatE2E wires the full stack and returns the fixture. Callers set the
// timing knobs via fastPoll first (package vars); the watcher goroutine is
// stopped-and-joined on cleanup BEFORE fastPoll restores them, so there is no
// package-var race.
func startWebChatE2E(t *testing.T, phase string) *webChatE2E {
	t.Helper()
	const name, chanName = "sess-e2e", "chan-e2e"
	k8s := newFakeK8sClient(t, combinedE2ESession(name, chanName, phase))

	nc := connectTestNATS(t)
	entry := newSessionEntry(newChatSessionNamespace, name, "user:owner")
	host := newTestBrowserHost(t, nc, chanName, name, entry)
	entry.senders = host
	// The real listener, bound as production binds it: the fixture exercises the
	// INBOUND leg too (the resurface request a freshly-attached tab publishes),
	// not just outbound frame delivery.
	entry.listener = &boundListener{l: host.Listener(), ns: newChatSessionNamespace, sessionName: name}

	d := &wsTestDeps{fakeDeps{k8s: k8s}}
	reg := newRegistryWithBuilder(d, newFakeBuilder().build)
	putTestEntry(reg, newChatKey(name), entry)

	relay := &outbound.Relay{NC: nc, K8s: k8s, Senders: reg, OnTurnActivity: reg.onTurnActivity}
	require.NoError(t, relay.Start(context.Background()))
	t.Cleanup(func() { _ = relay.Stop(context.Background()) })

	// Real watchSessionHealth, wired as production: it emits to the session's
	// sinks (entry.Emit) and, on a terminal, drives Registry.teardown — which
	// emits the session_ended frame the browser sees.
	hctx, hcancel := context.WithCancel(context.Background())
	hdone := make(chan struct{})
	go func() {
		defer close(hdone)
		watchSessionHealth(hctx, d, newChatSessionNamespace, name, chanName, entry.Emit, func(tn terminalNotice) {
			reg.teardown(newChatKey(name), tn)
		})
	}()
	t.Cleanup(func() { hcancel(); <-hdone })

	srv := httptest.NewServer(wsServeMux(withTestSubject("user:owner", wsHandler(d, reg))))
	t.Cleanup(srv.Close)
	return &webChatE2E{reg: reg, nc: nc, k8s: k8s, name: name, srv: srv}
}

// dialWS opens the browser websocket and waits for the sink to attach.
func (f *webChatE2E) dialWS(t *testing.T) *websocket.Conn {
	t.Helper()
	wsURL := wsDialURL(f.srv, newChatSessionNamespace, f.name)
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Origin": []string{testTrustedOrigin}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.Eventually(t, func() bool {
		e, ok := f.reg.lookup(newChatKey(f.name))
		if !ok {
			return false
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		return len(e.sinks) == 1
	}, 2*time.Second, 10*time.Millisecond, "the ws sink must attach")
	return conn
}

// setPhase updates the session's Status.Phase on the (fake) client the watcher
// polls — the E2E way to drive a lifecycle transition.
func (f *webChatE2E) setPhase(t *testing.T, phase string) {
	t.Helper()
	var live spiceboxv1alpha1.AgentSession
	require.NoError(t, f.k8s.Get(context.Background(), client.ObjectKey{Namespace: newChatSessionNamespace, Name: f.name}, &live))
	live.Status.Phase = phase
	require.NoError(t, f.k8s.Update(context.Background(), &live))
}

func (f *webChatE2E) publish(t *testing.T, kind channelevents.Kind, payload any) {
	t.Helper()
	env, err := channelevents.BuildEnvelope(newChatSessionNamespace, f.name, kind, payload)
	require.NoError(t, err)
	publishChatOut(t, f.nc, f.name, env)
}

// waitForFrame reads frames off conn until one of type wantType arrives (return
// it) or the deadline passes (fail). Frames of other types are skipped.
func waitForFrame(t *testing.T, conn *websocket.Conn, wantType string) wsFrame {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(wstest.Deadline(3*time.Second)))
	for {
		var f wsFrame
		require.NoError(t, conn.ReadJSON(&f), "reading ws frame while waiting for %q", wantType)
		if f.Type == wantType {
			return f
		}
	}
}

// assertNoFrame reads for `window` and fails if any frame of type badType
// arrives. A read timeout (no such frame) is the success path.
//
// `window` is used raw, with no wstest.Scale: a read that waits for NOTHING
// has to stay short, unlike the positive reads in this file. Its only caller
// passes 120ms, and a scaled window would buy no extra signal — the frame it
// is asserting the ABSENCE of is one the server either emits immediately or
// never emits at all.
func assertNoFrame(t *testing.T, conn *websocket.Conn, badType string, window time.Duration) {
	t.Helper()
	deadline := time.Now().Add(window)
	require.NoError(t, conn.SetReadDeadline(deadline))
	for {
		var f wsFrame
		if err := conn.ReadJSON(&f); err != nil {
			return // read deadline reached with no bad frame — success
		}
		if f.Type == badType {
			t.Fatalf("unexpected %q frame reached the browser: %+v", badType, f)
		}
	}
}

// TestWebChatE2E_AttachAsksForParkedPromptAndGetsIt is the regression test for
// a credential prompt lost to a late subscriber. A parked prompt is delivered
// by a ONE-SHOT live publish and its dedup guarantees it is never re-sent, so a
// tab that opens its websocket after that instant saw a durably-parked session
// with nothing at all on screen — no card, no error, no spinner — and the only
// re-send trigger was an inbound user message the waiting user has no reason to
// send.
//
// End to end over the real stack: attaching must publish a resurface_request on
// the session's IN subject, and the prompt channelsd re-publishes in response
// must reach the browser as an interaction_request frame. The subscriber here
// stands in for channelsd's HandleResurfaceRequest → resurfacePending (covered
// on its own in pkg/channels/channelsd/pipeline).
func TestWebChatE2E_AttachAsksForParkedPromptAndGetsIt(t *testing.T) {
	fastPoll(t, 10*time.Millisecond, time.Hour)
	f := startWebChatE2E(t, spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials)

	// Stand in for channelsd's subscription: capture the request the attach
	// publishes. The callback only hands it to the test body — the re-publish
	// happens there, off the NATS dispatch goroutine.
	asked := make(chan channelevents.ResurfaceRequestPayload, 4)
	sub, err := f.nc.Subscribe(
		channelevents.SubjectIn(channelevents.SubjectPrefix(newChatSessionNamespace, f.name), channelevents.KindResurfaceRequest),
		func(m *nats.Msg) {
			var env channelevents.Envelope
			if jerr := json.Unmarshal(m.Data, &env); jerr != nil {
				return
			}
			var pl channelevents.ResurfaceRequestPayload
			if jerr := json.Unmarshal(env.Payload, &pl); jerr != nil {
				return
			}
			asked <- pl
		})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, f.nc.Flush())

	conn := f.dialWS(t)

	select {
	case pl := <-asked:
		require.Equal(t, chatVia(t), pl.Via, "the request names the surface that attached")
	case <-time.After(3 * time.Second):
		t.Fatal("attaching a chat tab must ask channelsd to re-surface the parked prompt")
	}

	// What resurfacePending's regenerate leg does in response: re-publish the
	// parked credential prompt on OUT. It must reach THIS tab.
	f.publish(t, channelevents.KindInteractionRequest, channelevents.InteractionRequestPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: newChatSessionNamespace, Name: f.name},
		Category:        categories.CredentialLink,
		RequestRef:      "cred-req-fresh",
		Lead:            "Connect your accounts",
		Actions: []channelevents.InteractionAction{{
			ID: "connect_accounts", Label: "Connect your accounts",
			Kind: channelevents.ActionKindLink, URL: "https://example.test/link?d=x&sig=y",
		}},
		Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceRequester},
	})

	frame := waitForFrame(t, conn, "interaction_request")
	require.Equal(t, f.name, frame.Session.Name)
}

// TestWebChatE2E_AgentReplyReachesBrowser: a runner's respond_to_user
// (KindUserMessage) must travel NATS → relay → browser sender → wsSink → the
// browser websocket, as a "user_message" frame. This is the "first message
// stuck on 'starting'" path, end to end.
func TestWebChatE2E_AgentReplyReachesBrowser(t *testing.T) {
	fastPoll(t, 10*time.Millisecond, time.Hour)
	f := startWebChatE2E(t, spiceboxv1alpha1.AgentSessionPhaseRunning)
	conn := f.dialWS(t)

	f.publish(t, channelevents.KindUserMessage, channelevents.OutboundUserMessagePayload{Text: "here is your answer"})

	frame := waitForFrame(t, conn, "user_message")
	require.Equal(t, f.name, frame.Session.Name)
}

// TestWebChatE2E_TurnCompleteReachesBrowser: the runner's
// turn_activity(active:false) must reach the browser as a "turn_activity"
// frame (the signal that clears the "working" throbber).
func TestWebChatE2E_TurnCompleteReachesBrowser(t *testing.T) {
	fastPoll(t, 10*time.Millisecond, time.Hour)
	f := startWebChatE2E(t, spiceboxv1alpha1.AgentSessionPhaseRunning)
	conn := f.dialWS(t)

	f.publish(t, channelevents.KindTurnActivity,
		channelevents.TurnActivityPayload{Active: false, Cause: "awaiting_user_message"})

	waitForFrame(t, conn, "turn_activity")
}

// TestWebChatE2E_IdlePhase_ClearsWorkingViaBackstop: even with NO
// runner-published turn_activity, when the AgentSession settles into Idle the
// health watcher's backstop emits a turn-complete to the browser — the
// authoritative "session is idle → clear working" fix.
func TestWebChatE2E_IdlePhase_ClearsWorkingViaBackstop(t *testing.T) {
	fastPoll(t, 10*time.Millisecond, time.Hour)
	f := startWebChatE2E(t, spiceboxv1alpha1.AgentSessionPhaseRunning)
	conn := f.dialWS(t)

	f.setPhase(t, spiceboxv1alpha1.AgentSessionPhaseIdle)

	frame := waitForFrame(t, conn, "turn_activity")
	inner, ok := frame.Payload.(map[string]any)
	require.True(t, ok, "turn_activity payload shape")
	require.Equal(t, false, inner["active"], "the backstop must signal turn-complete (active:false)")
}

// TestWebChatE2E_WakeFromIdle_NoFalseCouldntStart: a session that started, went
// Idle, then was woken back to Pending (a new inbound / an artifact-view
// annotation) must NOT be declared "couldn't start" — no session_ended frame
// reaches the browser. This is the annotation-crash regression, end to end.
func TestWebChatE2E_WakeFromIdle_NoFalseCouldntStart(t *testing.T) {
	fastPoll(t, 10*time.Millisecond, 20*time.Millisecond) // tiny startup grace
	f := startWebChatE2E(t, spiceboxv1alpha1.AgentSessionPhaseRunning)
	conn := f.dialWS(t)

	// Let the watcher observe Running (latch hasStarted) well past the grace,
	// then drive Idle → Pending (the wake).
	time.Sleep(60 * time.Millisecond)
	f.setPhase(t, spiceboxv1alpha1.AgentSessionPhaseIdle)
	time.Sleep(30 * time.Millisecond)
	f.setPhase(t, spiceboxv1alpha1.AgentSessionPhasePending)

	// Well past the grace: a woken session must not produce a "couldn't start"
	// session_ended frame.
	assertNoFrame(t, conn, "session_ended", 120*time.Millisecond)
}

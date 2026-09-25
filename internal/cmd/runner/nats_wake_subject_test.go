package main

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	natstest "github.com/nats-io/nats-server/v2/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// Two IN kinds are spelled as literal subject suffixes rather than declared as
// channelevents.Kind constants: the runner builds in.metaagent_request with
// fmt.Sprintf (internal/cmd/runner/main.go, ColdStartRequestPublish) and the Slack
// metaagent callback builds in.metaagent_approval_applied the same way. Both
// are consumed by authzd (internal/cmd/authzd/main.go). Spelled the same way here so the
// table below is the complete IN namespace, not just the declared part of it.
const (
	kindMetaagentRequest         channelevents.Kind = "metaagent_request"
	kindMetaagentApprovalApplied channelevents.Kind = "metaagent_approval_applied"
)

// startEmbeddedNATS boots a plain (unauthenticated) in-process nats-server —
// the fixture internal/cmd/channelsd, pkg/channels/channelsd/e2e and pkg/web/webui/chat all use.
// initNATS dials it through the real apnats.ConnectFromEnv, so the connection
// under test is the production one, including its echo behaviour.
func startEmbeddedNATS(t *testing.T) *natsserver.Server {
	t.Helper()
	srv := natstest.RunServer(&natsserver.Options{
		Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true,
	})
	t.Cleanup(srv.Shutdown)
	return srv
}

// startWakePump calls the real initNATS against srv for (ns, name) and returns
// the runtime whose inboundCh is the await_user_message wake-up channel.
func startWakePump(t *testing.T, srv *natsserver.Server, ns, name string) *natsRuntime {
	t.Helper()
	// ConnectFromEnv reads these; blank them so the embedded server's plain
	// listener is dialed even when the developer's shell has them set.
	t.Setenv("NATS_CREDS_PATH", "")
	t.Setenv("NATS_CA_PATH", "")

	rt, err := initNATS(context.Background(), srv.ClientURL(), channelevents.SubjectPrefix(ns, name))
	require.NoError(t, err, "initNATS against the embedded server")
	require.NotNil(t, rt, "initNATS must return a runtime for a non-empty URL")
	t.Cleanup(rt.close)
	return rt
}

// envelopeFor marshals a minimal valid envelope of kind k for (ns, name) — the
// body every IN publisher puts on the wire.
func envelopeFor(t *testing.T, ns, name string, k channelevents.Kind) []byte {
	t.Helper()
	env := channelevents.Envelope{
		Version:     1,
		Kind:        k,
		Session:     channelevents.SessionRef{Namespace: ns, Name: name},
		PublishedAt: time.Now().UTC(),
		Payload:     []byte("{}"),
	}
	b, err := json.Marshal(env)
	require.NoError(t, err, "marshal %s envelope", k)
	return b
}

// woke reports whether a wake token lands on ch within d.
func woke(ch <-chan struct{}, d time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(d):
		return false
	}
}

// TestInitNATSWakeSubscription_OnlyUserMessageWakesAwait pins the contract of
// the await_user_message wake-up pump: a token on inboundCh means "a new user
// turn is in the inbox", and channelsd's Pipeline.publishWakeup —
// <prefix>.in.user_message — is the ONLY publisher that means that.
//
// The rows below are the complete <prefix>.in.* namespace, so the table doubles
// as the executable enumeration of every publisher that legitimately lands on
// the subject this subscription used to match. The `note` on each row records
// who publishes it and who consumes it; a narrowed subject that silently
// stopped delivering a legitimate message would be a worse bug than the one
// under test, so the enumeration is the evidence, not an afterthought.
//
// Every message is published on the runtime's OWN connection, which is exactly
// how the runner's InteractionRequestPublish / TimeoutAppliedPublish /
// ColdStartRequestPublish reach the bus: same *nats.Conn, and pkg/platform/nats never
// sets nats.NoEcho, so core NATS delivers them straight back to this
// subscription. A runner that wakes on its own approval prompt returns
// "acknowledged" from the next await_user_message, drains an empty inbox, and
// burns an LLM round trip instead of parking at Idle.
func TestInitNATSWakeSubscription_OnlyUserMessageWakesAwait(t *testing.T) {
	const ns = "test-ns"

	cases := []struct {
		name     string
		kind     channelevents.Kind
		note     string
		wantWake bool
	}{
		{
			name:     "in.user_message (channelsd publishWakeup): wakes await",
			kind:     channelevents.KindUserMessage,
			note:     "published by channelsd Pipeline.publishWakeup after the new user turn is appended to the inbox; the one kind that means a user replied",
			wantWake: true,
		},
		{
			name:     "in.interaction_request (runner's own approval/gate prompt): no self-wake",
			kind:     channelevents.KindInteractionRequest,
			note:     "published by THIS runner (Loop.InteractionRequestPublish) on this same conn; consumed by channelsd HandleInteractionRequest",
			wantWake: false,
		},
		{
			name:     "in.interaction_applied (runner's own TimeoutAppliedPublish): no self-wake",
			kind:     channelevents.KindInteractionApplied,
			note:     "published by THIS runner (Loop.TimeoutAppliedPublish) and by channelsd; the runner resumes off the .out. copy via subscribeInteractionApplied",
			wantWake: false,
		},
		{
			name:     "in.metaagent_request (runner's own ColdStartRequestPublish): no self-wake",
			kind:     kindMetaagentRequest,
			note:     "published by THIS runner (Loop.ColdStartRequestPublish) and by the Slack metaagent listener; consumed by authzd",
			wantWake: false,
		},
		{
			name:     "in.interrupt_request (has its own subscriber): no spurious wake",
			kind:     channelevents.KindInterruptRequest,
			note:     "published by channelsd/local/builtin listeners; consumed by the runner's dedicated subscribeInterruptRequest, and Loop.Interrupt no-ops on a parked session",
			wantWake: false,
		},
		{
			name:     "in.tool_session_input (routed to the tool bridge): no spurious wake",
			kind:     channelevents.KindToolSessionInput,
			note:     "published by channelsd's live-interactive-tool branch, which returns before publishWakeup; consumed by the runner's dedicated subscribeToolSessionInput",
			wantWake: false,
		},
		{
			name:     "in.app_tool_call (request/reply responder): no spurious wake",
			kind:     channelevents.KindAppToolCall,
			note:     "published by pkg/web/webui/interact as a RequestIn; answered synchronously by the runner's dedicated subscribeAppToolCall, which needs no loop goroutine",
			wantWake: false,
		},
		{
			name:     "in.view_message (wakes indirectly via publishWakeup): no direct wake",
			kind:     channelevents.KindViewMessage,
			note:     "published by a browser/TUI view as a RequestIn; channelsd's HandleViewMessage funnels it through Deliver -> publishWakeup, so it still wakes await as in.user_message",
			wantWake: false,
		},
		{
			name:     "in.interaction_decision (channelsd's decision pipe): no spurious wake",
			kind:     channelevents.KindInteractionDecision,
			note:     "published by channel-kind listeners and `oap session approve`; consumed by channelsd HandleInteractionDecision",
			wantWake: false,
		},
		{
			name:     "in.resurface_request (channelsd re-posts parked prompts): no spurious wake",
			kind:     channelevents.KindResurfaceRequest,
			note:     "published by a view attaching to the session; consumed by channelsd HandleResurfaceRequest",
			wantWake: false,
		},
		{
			name:     "in.restart_trigger (observability only): no spurious wake",
			kind:     channelevents.KindRestartTrigger,
			note:     "published best-effort by channelsd's restart publisher; nothing subscribes, and the runner never reads status.PendingRestart",
			wantWake: false,
		},
		{
			name:     "in.metaagent_approval_applied (authzd's callback): no spurious wake",
			kind:     kindMetaagentApprovalApplied,
			note:     "published by the Slack metaagent callback; consumed by authzd",
			wantWake: false,
		},
	}

	srv := startEmbeddedNATS(t)
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// One session (and so one subscription) per row, so a row's publish
			// can never be observed by a sibling's pump.
			name := fmt.Sprintf("sess-%d", i)
			rt := startWakePump(t, srv, ns, name)

			subject := channelevents.SubjectIn(channelevents.SubjectPrefix(ns, name), tc.kind)
			require.NoError(t, rt.conn.Publish(subject, envelopeFor(t, ns, name, tc.kind)),
				"publish %s on the runner's own connection", subject)
			require.NoError(t, rt.conn.Flush(), "flush %s", subject)

			// A positive row gets a generous ceiling; a negative row waits long
			// enough that loopback self-delivery (sub-millisecond, and already
			// round-tripped through Flush) would have landed many times over.
			if tc.wantWake {
				assert.True(t, woke(rt.inboundCh, 5*time.Second),
					"%s must wake await_user_message — %s", subject, tc.note)
				return
			}
			assert.False(t, woke(rt.inboundCh, 300*time.Millisecond),
				"%s must NOT plant a false 'user replied' token — %s", subject, tc.note)
		})
	}
}

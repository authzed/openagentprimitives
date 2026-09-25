package installcmd

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	apnats "github.com/authzed/openagentprimitives/pkg/platform/nats"
	"github.com/authzed/openagentprimitives/pkg/platform/nats/natstest"
)

// TestWebdNATSGrantAllowsInboxReply guards the request-reply invariant: webd's
// builtin web-chat listener and the live-view submit view_message via NATS
// RequestIn, which subscribes to a per-connection reply inbox. Without that
// inbox in SubAllow the reply is denied and every view-originated send times
// out even though the message WAS delivered (a spurious "failed to send
// message: … view_message … nats: timeout"). This test turns that silent
// regression into a build failure.
//
// The inbox entry is asserted through the same helper that mints it, not as a
// literal: the string webd SUBSCRIBES on is derived from the grant's Name by
// apnats.InboxSubjectFor, and pkg/platform/nats.buildOptions derives the client's
// CustomInboxPrefix from the same function over the same name. Pinning a
// literal here would let the two drift apart, which breaks every reply.
func TestWebdNATSGrantAllowsInboxReply(t *testing.T) {
	h := natstest.New(t)

	assert.True(t, h.SubscribeAllowed(t, webdNATSGrant, apnats.InboxPrefixFor(webdNATSUser)+".abc.def"),
		"webd does NATS request-reply (view_message); its reply inbox must be subscribable or replies time out")
	assert.True(t, h.SubscribeAllowed(t, webdNATSGrant, "ap.session.ns1.sess1.out.user_message"),
		"webd must still subscribe to session out-subjects for the chat/live-view mirror")
	assert.False(t, h.SubscribeAllowed(t, webdNATSGrant, "_INBOX.>"),
		"webd must NOT be able to read every principal's replies off the shared inbox root")
}

// TestWebdNATSGrantPublishSurface pins webd's publish rights to the five
// inbound subjects its code actually reaches, and proves everything else is
// refused by the server.
//
// The grant used to omit PubAllow entirely. An omitted PubAllow is publish-ANY
// (see the UserGrant doc comment), so the browser-facing process held publish
// rights on the whole control bus — the revocation bus, every session's
// out-subjects (forging agent replies), and every inbound kind including
// user_message, whose channelsd handler is the audit-signing writer webd is
// deliberately kept away from (pkg/channels/channelkinds/clienthosted.SubmitUserMessage
// documents exactly that: webd holds no memory-write credential, so it must
// round-trip through view_message instead).
//
// Each permitted subject below is a verified call site; each denied one is a
// subject no webd code path publishes. If a new webd surface needs one, add it
// here and to webdNATSGrant together — a missing entry surfaces as a logged
// publish error, not silence.
func TestWebdNATSGrantPublishSurface(t *testing.T) {
	h := natstest.New(t)

	// The inbound subjects webd genuinely publishes.
	permitted := map[string]string{
		"ap.session.ns1.sess1.in.view_message":         "chat SubmitUserMessage + /interact user_message|annotation_batch|mcp_ui_action → RequestViewMessage",
		"ap.session.ns1.sess1.in.interrupt_request":    "chat SubmitInterrupt → clienthosted.SubmitInterrupt",
		"ap.session.ns1.sess1.in.interaction_decision": "chat SubmitDecision → clienthosted.SubmitInteractionDecision (the browser approve/deny button)",
		"ap.session.ns1.sess1.in.resurface_request":    "chat RequestResurface → channelkinds.PublishResurfaceRequest",
		"ap.session.ns1.sess1.in.app_tool_call":        "webui/interact app-tool-call → RequestIn(KindAppToolCall)",
		// The agent-UI data path. Its absence shipped: every `source: tool`
		// binding was refused at publish and surfaced only as the caller's own
		// timeout, because NATS reports a permissions violation asynchronously
		// on the connection and leaves the request waiting for a reply nobody
		// may send. This suite could not catch it because the map below is the
		// hand-written list it checks — it proves the grant matches what is
		// written here, which is only as complete as what someone wrote.
		"ap.session.ns1.sess1.in.ui_data_binding": "webui/agentui bindings → uibindings/tool RequestIn(KindUIDataBinding)",
		"ap.session.ns1.sess1.in.ui_presence":     "webui/agentui live socket → the runner's idle-deadline heartbeat (KindUIPresence)",
		"ap.session.ns1.sess1.in.ui_action":       "webui/agentui actions handler → the runner's action invoke (KindUIAction)",
		// A subject absent from BOTH this map and the grant passes this test
		// vacuously — this row exists only because a human read the grant and
		// wrote it down, the same limit the ui_data_binding comment above
		// names. See channelwebhook_test.go's own coverage for the route's
		// actual publish call; this row only proves the SERVER permits it.
		channelevents.WebhookInboundSubject: "pkg/web/webui/channelwebhook's verified-delivery handoff to channelsd",
	}
	for subject, why := range permitted {
		assert.True(t, h.PublishAllowed(t, webdNATSGrant, subject),
			"webd must be able to publish %s — %s", subject, why)
	}

	// Everything else on the bus.
	denied := map[string]string{
		"ap.revocation":                               "webd never constructs a revocation.Publisher; only the operator emits revokes",
		"ap.monitoring.events":                        "PublishMonitoring is called only from channelsd's pipeline and the Slack kind, neither of which webd runs",
		"ap.session.ns1.sess1.out.user_message":       "out-subjects are the agent's voice; webd only subscribes to them",
		"ap.session.ns1.sess1.out.notification":       "same — forging one would put words in the agent's mouth",
		"ap.session.ns1.sess1.in.user_message":        "channelsd is the sole inbound writer (it holds the audit-signing seed); webd must use view_message",
		"ap.session.ns1.sess1.in.tool_session_input":  "no webd code path publishes tool-session input",
		"ap.session.ns1.sess1.in.metaagent_request":   "runner-only subject",
		"ap.session.ns1.sess1.in.interaction_applied": "runner/channelsd emit applied; webd renders it",
		"ap.session.ns1.sess1.history.request":        "webd reads history over HTTP (livemirror.ReadHistory), not NATS",
		"ap.channel.session_attached":                 "the only publisher is the outbound relay's patchOutputChannel, unreachable in webd (every builtin sender returns a zero SubChannelSendResult, so captured is empty and it returns early)",
	}
	for subject, why := range denied {
		assert.False(t, h.PublishAllowed(t, webdNATSGrant, subject),
			"webd must NOT be able to publish %s — %s", subject, why)
	}
}

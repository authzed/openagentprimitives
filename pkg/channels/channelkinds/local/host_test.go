package local

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

func newTestHost(t *testing.T, sink EventSink) *Host {
	t.Helper()
	h, err := NewHost(HostConfig{
		Deps:        channelkinds.Deps{Channel: testChannel()},
		Sink:        sink,
		User:        channelkinds.ExternalIdentity{Kind: "local", ExternalID: "local-user"},
		Namespace:   "default",
		SessionName: "sess-1",
	})
	require.NoError(t, err)
	return h
}

func TestNewHost_RejectsNilSink(t *testing.T) {
	_, err := NewHost(HostConfig{Deps: channelkinds.Deps{Channel: testChannel()}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sink")
}

func TestNewHost_RejectsNilChannel(t *testing.T) {
	_, err := NewHost(HostConfig{Sink: &RecordingSink{}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "channel")
}

func TestHost_SenderFor_RoutesUserMessageToSink(t *testing.T) {
	sink := &RecordingSink{}
	h := newTestHost(t, sink)
	sender, err := h.SenderFor(context.Background(), ownSession())
	require.NoError(t, err)
	require.NotNil(t, sender)
	env, err := channelevents.BuildEnvelope("default", "sess-1", channelevents.KindUserMessage,
		channelevents.OutboundUserMessagePayload{Text: "done"})
	require.NoError(t, err)
	_, err = sender.Send(context.Background(),
		channelkinds.SessionInfo{Namespace: "default", Name: "sess-1"}, env)
	require.NoError(t, err)
	require.Len(t, sink.Events(), 1)
	assert.IsType(t, MsgUserMessage{}, sink.Events()[0])
}

func TestHost_SubChannelSenderFor(t *testing.T) {
	h := newTestHost(t, &RecordingSink{})
	for _, name := range []string{"message", "tool_session", "permission_request", "queued_messages"} {
		s, err := h.SubChannelSenderFor(context.Background(), ownSession(), name)
		require.NoError(t, err, "sub-channel %q", name)
		assert.NotNil(t, s, "sub-channel %q must resolve", name)
	}
	s, err := h.SubChannelSenderFor(context.Background(), ownSession(), "unknown")
	require.NoError(t, err)
	assert.Nil(t, s, "unknown sub-channel resolves to nil")
}

func TestHost_StreamDeltaSinkFor(t *testing.T) {
	// Off by default: no AgentClass opting into showAssistantStream (the test
	// host has no K8s client) → a nil sink, which the outbound relay treats as
	// "drop the stream". The TUI must not show the agent's raw LLM narrative
	// unless the class explicitly enables it.
	h := newTestHost(t, &RecordingSink{})
	sds, err := h.StreamDeltaSinkFor(context.Background(), ownSession())
	require.NoError(t, err)
	assert.Nil(t, sds, "streaming is off unless the AgentClass opts in")

	// Opted in: an AgentClass with showAssistantStream=true → a real sink.
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))
	class := &spiceboxv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "streamer"}}
	class.Spec.Channels = &spiceboxv1alpha1.ChannelsConfig{ShowAssistantStream: true}
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(class).Build()
	h2, err := NewHost(HostConfig{
		Deps:        channelkinds.Deps{Channel: testChannel(), K8sClient: cli},
		Sink:        &RecordingSink{},
		User:        channelkinds.ExternalIdentity{Kind: "local", ExternalID: "local-user"},
		Namespace:   "default",
		SessionName: "s",
	})
	require.NoError(t, err)
	sess := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "s"}}
	sess.Spec.Class = "streamer"
	sds2, err := h2.StreamDeltaSinkFor(context.Background(), sess)
	require.NoError(t, err)
	assert.NotNil(t, sds2, "opted-in AgentClass → real stream sink")
}

// TestNewHost_RejectsUnscopedSession guards the fail-closed construction: a
// Host with no (namespace, session) to scope to would match nothing, silently
// dropping its own session's every reply.
func TestNewHost_RejectsUnscopedSession(t *testing.T) {
	_, err := NewHost(HostConfig{
		Deps:        channelkinds.Deps{Channel: testChannel()},
		Sink:        &RecordingSink{},
		SessionName: "sess-1",
	})
	require.Error(t, err, "a Host with no namespace must be refused")
	assert.Contains(t, err.Error(), "namespace")

	_, err = NewHost(HostConfig{
		Deps:      channelkinds.Deps{Channel: testChannel()},
		Sink:      &RecordingSink{},
		Namespace: "default",
	})
	require.Error(t, err, "a Host with no session name must be refused")
	assert.Contains(t, err.Error(), "sessionName")
}

func TestHost_Listener_HasChannelKeyAndUser(t *testing.T) {
	h := newTestHost(t, &RecordingSink{})
	l := h.Listener()
	require.NotNil(t, l)
	assert.NotEmpty(t, l.ChannelKey, "host must assign a stable channelKey")
	assert.Equal(t, "local-user", l.Ext.ExternalID.String())
}

// TestHost_Listener_ThreadsNamespaceAndSessionName guards the wiring
// RequestViewMessage depends on: HostConfig.Namespace/SessionName must reach
// the Listener it hands back, or SubmitUserMessage addresses the wrong (or an
// empty) ap.session.<ns>.<name>.in.view_message subject.
func TestHost_Listener_ThreadsNamespaceAndSessionName(t *testing.T) {
	h := newTestHost(t, &RecordingSink{})
	l := h.Listener()
	require.NotNil(t, l)
	assert.Equal(t, "default", l.Namespace, "HostConfig.Namespace must reach the Listener")
	assert.Equal(t, "sess-1", l.SessionName, "HostConfig.SessionName must reach the Listener")
}

// ownSession is the AgentSession newTestHost's Host is scoped to.
func ownSession() *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "sess-1"},
	}
}

// foreignSession is any OTHER session in the cluster — a Slack- or
// cron-driven one the outbound relay also receives, because it subscribes the
// cluster-wide "ap.session.*.*.out.>" and the CLI's NATS grant is "ap.>".
func foreignSession() *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "other-sess"},
	}
}

// TestHost_ForeignSessionInteractionRequestNeverReachesTheSink is the sharp
// edge of the resolver's session-blindness: not a display leak, a hijacked
// decision.
//
// A foreign session's interaction_request resolving to this host's
// interactionSender emits a MsgInteractionRequest into the CLI user's sink;
// chat_tui.go sets m.interaction unconditionally, opening its BLOCKING
// decision modal — and the key the user presses is submitted by
// decideInteraction under the CLI's OWN session name with the FOREIGN
// requestRef. The decision is misrouted and the foreign session's real prompt
// goes unanswered.
//
// The Host must therefore refuse to resolve a sender for any session but its
// own, which tells outbound.Relay to drop the envelope.
func TestHost_ForeignSessionInteractionRequestNeverReachesTheSink(t *testing.T) {
	sink := &RecordingSink{}
	h := newTestHost(t, sink)

	sender, err := h.SubChannelSenderFor(context.Background(), foreignSession(), "interaction")
	require.NoError(t, err)

	// Drive the sender exactly as outbound.Relay would, so a non-nil resolution
	// shows what it actually costs rather than only failing the nil assertion.
	if sender != nil {
		env, buildErr := channelevents.BuildEnvelope("default", "other-sess",
			channelevents.KindInteractionRequest,
			channelevents.InteractionRequestPayload{
				AgentSessionRef: channelevents.SessionRef{Namespace: "default", Name: "other-sess"},
				Category:        "tool_approval",
				RequestRef:      "foreign-request-ref",
				Lead:            "Approve this tool call?",
				Actions: []channelevents.InteractionAction{
					{ID: "approve", Label: "Approve", Kind: channelevents.ActionKindDecision},
					{ID: "deny", Label: "Deny", Kind: channelevents.ActionKindDecision},
				},
				Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceApprovers},
			})
		require.NoError(t, buildErr)
		_, _ = sender.Send(context.Background(),
			channelkinds.SessionInfo{Namespace: "default", Name: "other-sess"}, env)
	}

	assert.Nil(t, sender,
		"a foreign session's interaction sub-channel must not resolve to this host's sender")
	assert.Empty(t, sink.Events(),
		"a foreign session's interaction_request must never reach this TUI's sink: it opens the CLI user's blocking decision modal bound to a foreign requestRef, which decideInteraction then submits under the CLI's own session name")
}

// TestHost_ResolversAreScopedToTheOwnSession covers the rest of the surface the
// same relay reaches: the main sender (the agent's reply text) and the
// stream-delta sink. A nil session is fail-closed for the same reason — the
// relay only ever passes the session it loaded, so nil means "cannot tell".
func TestHost_ResolversAreScopedToTheOwnSession(t *testing.T) {
	sink := &RecordingSink{}
	h := newTestHost(t, sink)
	ctx := context.Background()

	own, err := h.SenderFor(ctx, ownSession())
	require.NoError(t, err)
	assert.NotNil(t, own, "the host's own session must resolve to its sender")

	foreign, err := h.SenderFor(ctx, foreignSession())
	require.NoError(t, err)
	assert.Nil(t, foreign, "another session's reply text must not render into this TUI")

	nilSess, err := h.SenderFor(ctx, nil)
	require.NoError(t, err)
	assert.Nil(t, nilSess, "an unidentifiable session is fail-closed")

	sds, err := h.StreamDeltaSinkFor(ctx, foreignSession())
	require.NoError(t, err)
	assert.Nil(t, sds, "another session's assistant stream must not render into this TUI")
}

// TestHost_Accepts is the relay's pre-Get scope (outbound.Relay.Accept),
// answered from the same one (namespace, session) pair the resolver methods
// use — so the two can never disagree about which session this TUI serves.
func TestHost_Accepts(t *testing.T) {
	h := newTestHost(t, &RecordingSink{})
	assert.True(t, h.Accepts("default", "sess-1"), "the host's own session is accepted")
	assert.False(t, h.Accepts("default", "other-sess"), "another session in the same namespace is rejected")
	assert.False(t, h.Accepts("other-ns", "sess-1"), "the same name in another namespace is rejected")
	assert.False(t, h.Accepts("", ""), "an empty ref is rejected")
}

func TestHost_SatisfiesSenderResolver(t *testing.T) {
	// Compile-time: *Host must satisfy outbound.SenderResolver. Asserted
	// in the cmd/oap wiring; here we just exercise the three methods.
	h := newTestHost(t, &RecordingSink{})
	_, err := h.SenderFor(context.Background(), nil)
	require.NoError(t, err)
}

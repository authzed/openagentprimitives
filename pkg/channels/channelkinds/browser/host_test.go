package browser

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
		User:        channelkinds.ExternalIdentity{Kind: KindName, ExternalID: "local-user"},
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
	sender, err := h.SenderFor(context.Background(), nil)
	require.NoError(t, err)
	require.NotNil(t, sender)
	env, err := channelevents.BuildEnvelope("default", "s1", channelevents.KindUserMessage,
		channelevents.OutboundUserMessagePayload{Text: "done"})
	require.NoError(t, err)
	_, err = sender.Send(context.Background(),
		channelkinds.SessionInfo{Namespace: "default", Name: "s1"}, env)
	require.NoError(t, err)
	require.Len(t, sink.Events(), 1)
	assert.IsType(t, MsgUserMessage{}, sink.Events()[0])
}

func TestHost_SubChannelSenderFor(t *testing.T) {
	h := newTestHost(t, &RecordingSink{})
	for _, name := range []string{"message", "tool_session", "permission_request", "queued_messages", "interaction"} {
		s, err := h.SubChannelSenderFor(context.Background(), nil, name)
		require.NoError(t, err, "sub-channel %q", name)
		assert.NotNil(t, s, "sub-channel %q must resolve", name)
	}
	s, err := h.SubChannelSenderFor(context.Background(), nil, "unknown")
	require.NoError(t, err)
	assert.Nil(t, s, "unknown sub-channel resolves to nil")
}

func TestHost_StreamDeltaSinkFor(t *testing.T) {
	// Off by default: no AgentClass opting into showAssistantStream (the test
	// host has no K8s client / no session) → a nil sink, which the outbound
	// relay treats as "drop the stream". The web chat must not show the agent's
	// raw LLM narrative unless the class explicitly enables it.
	h := newTestHost(t, &RecordingSink{})
	sds, err := h.StreamDeltaSinkFor(context.Background(), nil)
	require.NoError(t, err)
	assert.Nil(t, sds, "streaming is off unless the AgentClass opts in")

	// Opted in: an AgentClass with showAssistantStream=true → a real sink.
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))
	class := &spiceboxv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "streamer"}}
	class.Spec.Channels = &spiceboxv1alpha1.ChannelsConfig{ShowAssistantStream: true}
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(class).Build()
	h2, err := NewHost(HostConfig{
		Deps: channelkinds.Deps{Channel: testChannel(), K8sClient: cli},
		Sink: &RecordingSink{},
		User: channelkinds.ExternalIdentity{Kind: KindName, ExternalID: "local-user"},
	})
	require.NoError(t, err)
	sess := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "s"}}
	sess.Spec.Class = "streamer"
	sds2, err := h2.StreamDeltaSinkFor(context.Background(), sess)
	require.NoError(t, err)
	assert.NotNil(t, sds2, "opted-in AgentClass → real stream sink")
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

func TestHost_SatisfiesSenderResolver(t *testing.T) {
	// Compile-time: *Host must satisfy outbound.SenderResolver. Asserted
	// in the host process's wiring; here we just exercise the three methods.
	h := newTestHost(t, &RecordingSink{})
	_, err := h.SenderFor(context.Background(), nil)
	require.NoError(t, err)
}

package fake

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/local"
)

// TestFakeKindImplementsUserEcho locks the contract that the fake kind
// implements the "user_echo" sub-channel, so the view→origin-channel mirror is
// round-trip testable without a real Slack workspace.
func TestFakeKindImplementsUserEcho(t *testing.T) {
	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "ch-user-echo"},
		Spec:       spiceboxv1alpha1.ChannelSpec{Kind: "fake", AgentClass: "x"},
	}
	k := Kind{}
	s := k.SubChannelSender("user_echo", channelkinds.Deps{Channel: ch})
	require.NotNil(t, s, "fake kind must implement user_echo so the round-trip is testable")
}

// TestFakeUserEchoSender_RecordsEnvelope proves the user_echo sub-channel
// sender actually records the mirrored envelope (not just returns non-nil),
// so e2e scenarios can assert the origin channel received the echo.
func TestFakeUserEchoSender_RecordsEnvelope(t *testing.T) {
	ResetAllDrivers()
	t.Cleanup(ResetAllDrivers)

	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "ch-user-echo-record"},
		Spec:       spiceboxv1alpha1.ChannelSpec{Kind: "fake", AgentClass: "x"},
	}
	k := Kind{}
	s := k.SubChannelSender("user_echo", channelkinds.Deps{Channel: ch})
	require.NotNil(t, s)

	pl := channelevents.UserEchoPayload{
		Text:   "hello from the view",
		Author: channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U123"},
		Via:    "artifact:sess-1/dash",
	}
	env, err := channelevents.BuildEnvelope("ns", "sess-1", channelevents.KindUserEcho, pl)
	require.NoError(t, err)

	_, err = s.Send(context.Background(), channelkinds.SessionInfo{Namespace: "ns", Name: "sess-1"}, env)
	require.NoError(t, err, "user_echo sender must accept a KindUserEcho envelope")

	got := DriverFor("ns", "ch-user-echo-record").UserEchoes()
	require.Len(t, got, 1, "one user_echo envelope recorded")
	assert.Equal(t, "hello from the view", got[0].Payload.Text)
	assert.Equal(t, "artifact:sess-1/dash", got[0].Payload.Via)
	assert.Equal(t, "sess-1", got[0].SessionRef.Name)
}

// TestFakeUserEchoSender_RejectsUnknownEnvelopeKind mirrors the existing
// fakePermReqSender negative test: the user_echo sender only understands
// KindUserEcho envelopes.
func TestFakeUserEchoSender_RejectsUnknownEnvelopeKind(t *testing.T) {
	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "ch-user-echo-bad"},
		Spec:       spiceboxv1alpha1.ChannelSpec{Kind: "fake", AgentClass: "x"},
	}
	s := Kind{}.SubChannelSender("user_echo", channelkinds.Deps{Channel: ch})
	require.NotNil(t, s)

	env, err := channelevents.BuildEnvelope("ns", "sess",
		channelevents.KindUserMessage, channelevents.OutboundUserMessagePayload{Text: "x"})
	require.NoError(t, err)
	_, err = s.Send(context.Background(), channelkinds.SessionInfo{Namespace: "ns", Name: "sess"}, env)
	require.Error(t, err, "user_echo sender must reject non-user_echo envelope kinds")
}

// TestBrowserLocalDoNotImplementUserEcho locks the negative half of the
// contract: browser and local ARE the view surface, so there is nothing to
// mirror back to — their SubChannelSender default (unknown name → nil) must
// keep returning nil for "user_echo" without any kind-specific change.
func TestBrowserLocalDoNotImplementUserEcho(t *testing.T) {
	assert.Nil(t, (&browser.Kind{}).SubChannelSender("user_echo", channelkinds.Deps{}),
		"the browser IS the channel; nothing to mirror")
	assert.Nil(t, (&local.Kind{}).SubChannelSender("user_echo", channelkinds.Deps{}))
}

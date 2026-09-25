package chat

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// fakeViewMinter is a test ArtifactViewMinter that returns a canned URL.
type fakeViewMinter struct{ url string }

func (m fakeViewMinter) MintArtifactViewLink(artifactID, sessionRef string, subject identity.Principal, backLink string) (string, error) {
	return m.url, nil
}

// TestNewBrowserHost_WiresArtifactViewMinter is the regression test for the
// production bug where webd's browser chat never wired an ArtifactViewMinter
// into the Host it built, so the live_view_offer sub-channel sender's minter
// was nil and the agent's artifact_offer_view was dropped instead of surfacing
// a "View live" link in the browser. It drives the sub-channel sender exactly
// the way the outbound relay does and asserts a MsgLiveViewOffer carrying the
// minted URL reaches the sink — i.e. the chat session builder must thread
// d.ArtifactViewMinter() all the way into the browser Host.
func TestNewBrowserHost_WiresArtifactViewMinter(t *testing.T) {
	const wantURL = "https://webd.example/artifact-view?d=abc&sig=def"
	d := &fakeDeps{
		k8s:    newFakeK8sClient(t),
		minter: fakeViewMinter{url: wantURL},
	}
	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "sess-chan", Namespace: newChatSessionNamespace},
	}
	sink := &browser.RecordingSink{}

	host, err := newBrowserHost(
		d, newChatSessionNamespace, ch, sink,
		channelkinds.ExternalIdentity{Kind: "fake", ExternalID: "u1"},
		identity.RawSubject("user:alice"),
		"sess", "",
	)
	require.NoError(t, err)

	sender, err := host.SubChannelSenderFor(context.Background(), nil, "live_view_offer")
	require.NoError(t, err)
	require.NotNil(t, sender)

	payload, err := json.Marshal(channelevents.LiveViewOfferPayload{ArtifactID: "art-1", RendererKind: "html"})
	require.NoError(t, err)
	env := channelevents.Envelope{Version: 1, Kind: channelevents.KindLiveViewOffer, Payload: payload}

	_, err = sender.Send(context.Background(), channelkinds.SessionInfo{Namespace: newChatSessionNamespace, Name: "sess"}, env)
	require.NoError(t, err, "a wired minter must deliver the live-view offer, not error out")

	require.Len(t, sink.Events(), 1, "want exactly one emitted event")
	m, ok := sink.Events()[0].(browser.MsgLiveViewOffer)
	require.True(t, ok,
		"want MsgLiveViewOffer (the browser 'View live' link), got %T — a nil/unwired minter emits MsgSendError instead",
		sink.Events()[0])
	assert.Equal(t, wantURL, m.URL)
}

// fakeSessionViewMinter is a test SessionViewMinter that returns a canned URL.
type fakeSessionViewMinter struct{ url string }

func (m fakeSessionViewMinter) MintSessionViewLink(sessionRef string, subject identity.Principal, backLink string) (string, error) {
	return m.url, nil
}

// TestNewBrowserHost_WiresSessionViewMinter is the session_view_offer
// counterpart to TestNewBrowserHost_WiresArtifactViewMinter above: it proves
// the chat session builder threads d.SessionViewMinter() all the way into the
// browser Host, so the session_view_offer sub-channel sender's minter is
// non-nil and an interactive-view escalation anchor reaches the browser as a
// MsgLiveViewOffer instead of being dropped (or, before this wiring, an
// unavoidable MsgSendError from a permanently-nil minter).
func TestNewBrowserHost_WiresSessionViewMinter(t *testing.T) {
	const wantURL = "https://webd.example/session-view/default/sess"
	d := &fakeDeps{
		k8s:               newFakeK8sClient(t),
		sessionViewMinter: fakeSessionViewMinter{url: wantURL},
	}
	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "sess-chan", Namespace: newChatSessionNamespace},
	}
	sink := &browser.RecordingSink{}

	host, err := newBrowserHost(
		d, newChatSessionNamespace, ch, sink,
		channelkinds.ExternalIdentity{Kind: "fake", ExternalID: "u1"},
		identity.RawSubject("user:alice"),
		"sess", "",
	)
	require.NoError(t, err)

	sender, err := host.SubChannelSenderFor(context.Background(), nil, "session_view_offer")
	require.NoError(t, err)
	require.NotNil(t, sender)

	payload, err := json.Marshal(channelevents.SessionViewOfferPayload{SessionRef: "default/sess"})
	require.NoError(t, err)
	env := channelevents.Envelope{Version: 1, Kind: channelevents.KindSessionViewOffer, Payload: payload}

	_, err = sender.Send(context.Background(), channelkinds.SessionInfo{Namespace: newChatSessionNamespace, Name: "sess"}, env)
	require.NoError(t, err, "a wired minter must deliver the session-view offer, not error out")

	require.Len(t, sink.Events(), 1, "want exactly one emitted event")
	m, ok := sink.Events()[0].(browser.MsgLiveViewOffer)
	require.True(t, ok,
		"want MsgLiveViewOffer (the browser interactive-view link), got %T — a nil/unwired minter emits MsgSendError instead",
		sink.Events()[0])
	assert.Equal(t, wantURL, m.URL)
}

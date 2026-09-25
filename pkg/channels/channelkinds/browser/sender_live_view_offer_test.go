package browser

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// fakeMinter is a test-only ArtifactViewMinter that returns a canned URL.
type fakeMinter struct {
	url string
	err error
}

func (f *fakeMinter) MintArtifactViewLink(artifactID, sessionRef string, subject identity.Principal, backLink string) (string, error) {
	return f.url, f.err
}

// TestLiveViewOfferSender_WiredMinter_EmitsMsgLiveViewOffer verifies that a
// live_view_offer envelope with a wired fake minter renders a MsgLiveViewOffer
// event with the minted URL.
func TestLiveViewOfferSender_WiredMinter_EmitsMsgLiveViewOffer(t *testing.T) {
	sink := &RecordingSink{}
	s := &liveViewOfferSender{
		sink:      sink,
		minter:    &fakeMinter{url: "https://webd.example.com/artifact-view?d=abc&sig=def"},
		principal: identity.RawSubject("user:alice"),
	}
	env := mustEnv(t, channelevents.KindLiveViewOffer,
		channelevents.LiveViewOfferPayload{ArtifactID: "art-xyz", RendererKind: "html"})

	_, err := s.Send(context.Background(), sessInfo(), env)
	require.NoError(t, err)
	require.Len(t, sink.Events(), 1)
	m, ok := sink.Events()[0].(MsgLiveViewOffer)
	require.True(t, ok, "want MsgLiveViewOffer, got %T", sink.Events()[0])
	assert.Equal(t, "https://webd.example.com/artifact-view?d=abc&sig=def", m.URL)
	assert.Equal(t, "s1", m.Session.Name)
}

// TestLiveViewOfferSender_NilMinter_EmitsSendError verifies that a nil minter
// — this server was never wired to mint browser view links — surfaces LOUDLY:
// a visible MsgSendError plus a returned error, never a silent drop. The agent
// explicitly offered the user a live view; a missing minter is a
// misconfiguration the waiting user and the operator must both see (this is
// the exact bug that silently swallowed a real session's artifact_offer_view).
func TestLiveViewOfferSender_NilMinter_EmitsSendError(t *testing.T) {
	sink := &RecordingSink{}
	s := &liveViewOfferSender{sink: sink, minter: nil}
	env := mustEnv(t, channelevents.KindLiveViewOffer,
		channelevents.LiveViewOfferPayload{ArtifactID: "art-xyz", RendererKind: "html"})

	_, err := s.Send(context.Background(), sessInfo(), env)
	require.Error(t, err, "nil minter must return an error, not skip silently")
	require.Len(t, sink.Events(), 1)
	se, ok := sink.Events()[0].(MsgSendError)
	require.True(t, ok, "want MsgSendError, got %T", sink.Events()[0])
	assert.Contains(t, se.Err, "not configured", "user-visible reason must name the misconfiguration")
}

// TestLiveViewOfferSender_EmptyURL_EmitsSendError verifies that when the minter
// returns ("", nil) — webd's external-URL ConfigMap is not populated — the
// offer surfaces LOUDLY (MsgSendError + returned error) rather than being
// dropped silently. The offer still cannot reach the browser, so the user must
// be told why.
func TestLiveViewOfferSender_EmptyURL_EmitsSendError(t *testing.T) {
	sink := &RecordingSink{}
	s := &liveViewOfferSender{
		sink:   sink,
		minter: &fakeMinter{url: ""},
	}
	env := mustEnv(t, channelevents.KindLiveViewOffer,
		channelevents.LiveViewOfferPayload{ArtifactID: "art-xyz", RendererKind: "html"})

	_, err := s.Send(context.Background(), sessInfo(), env)
	require.Error(t, err, "empty URL must return an error, not skip silently")
	require.Len(t, sink.Events(), 1)
	se, ok := sink.Events()[0].(MsgSendError)
	require.True(t, ok, "want MsgSendError, got %T", sink.Events()[0])
	assert.Contains(t, se.Err, "external URL", "user-visible reason must name the missing external URL")
}

// TestLiveViewOfferSender_MintError_EmitsSendError verifies that a mint error
// surfaces as a MsgSendError event and a non-nil return error.
func TestLiveViewOfferSender_MintError_EmitsSendError(t *testing.T) {
	sink := &RecordingSink{}
	s := &liveViewOfferSender{
		sink:   sink,
		minter: &fakeMinter{err: fmt.Errorf("signing key unavailable")},
	}
	env := mustEnv(t, channelevents.KindLiveViewOffer,
		channelevents.LiveViewOfferPayload{ArtifactID: "art-xyz", RendererKind: "html"})

	_, err := s.Send(context.Background(), sessInfo(), env)
	require.Error(t, err)
	require.Len(t, sink.Events(), 1)
	se, ok := sink.Events()[0].(MsgSendError)
	require.True(t, ok, "want MsgSendError, got %T", sink.Events()[0])
	assert.Contains(t, se.Err, "signing key unavailable")
}

// TestLiveViewOfferSender_MissingArtifactID_EmitsSendError verifies that an
// envelope with an empty artifactId surfaces as a MsgSendError.
func TestLiveViewOfferSender_MissingArtifactID_EmitsSendError(t *testing.T) {
	sink := &RecordingSink{}
	s := &liveViewOfferSender{
		sink:   sink,
		minter: &fakeMinter{url: "https://webd.example.com/artifact-view?d=abc&sig=def"},
	}
	env := mustEnv(t, channelevents.KindLiveViewOffer,
		channelevents.LiveViewOfferPayload{ArtifactID: "", RendererKind: "html"})

	_, err := s.Send(context.Background(), sessInfo(), env)
	require.Error(t, err)
	require.Len(t, sink.Events(), 1)
	_, ok := sink.Events()[0].(MsgSendError)
	require.True(t, ok, "want MsgSendError, got %T", sink.Events()[0])
}

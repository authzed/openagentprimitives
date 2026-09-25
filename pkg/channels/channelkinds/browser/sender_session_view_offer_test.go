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

// fakeSessionViewMinter is a test-only SessionViewMinter that returns a
// canned URL.
type fakeSessionViewMinter struct {
	url string
	err error
}

func (f *fakeSessionViewMinter) MintSessionViewLink(sessionRef string, subject identity.Principal, backLink string) (string, error) {
	return f.url, f.err
}

// TestSessionViewOfferSender_WiredMinter_EmitsMsgLiveViewOffer verifies that
// a session_view_offer envelope with a wired fake minter renders a
// MsgLiveViewOffer event carrying the minted URL — the same rendered-link
// event live_view_offer uses (both are "here's a browser link" notes; a
// dedicated Msg type would need its own sink.go/frontend wiring that doesn't
// exist yet, so reuse gets this fully working end-to-end for free).
func TestSessionViewOfferSender_WiredMinter_EmitsMsgLiveViewOffer(t *testing.T) {
	sink := &RecordingSink{}
	s := &sessionViewOfferSender{
		sink:      sink,
		minter:    &fakeSessionViewMinter{url: "https://webd.example.com/session-view/default/sess-1"},
		principal: identity.RawSubject("user:alice"),
	}
	env := mustEnv(t, channelevents.KindSessionViewOffer,
		channelevents.SessionViewOfferPayload{SessionRef: "default/sess-1"})

	_, err := s.Send(context.Background(), sessInfo(), env)
	require.NoError(t, err)
	require.Len(t, sink.Events(), 1)
	m, ok := sink.Events()[0].(MsgLiveViewOffer)
	require.True(t, ok, "want MsgLiveViewOffer, got %T", sink.Events()[0])
	assert.Equal(t, "https://webd.example.com/session-view/default/sess-1", m.URL)
	assert.Equal(t, "s1", m.Session.Name)
}

// TestSessionViewOfferSender_NilMinter_EmitsSendError verifies that a nil
// minter surfaces LOUDLY: a visible MsgSendError plus a returned error, never
// a silent drop — mirroring liveViewOfferSender's nil-minter convention.
func TestSessionViewOfferSender_NilMinter_EmitsSendError(t *testing.T) {
	sink := &RecordingSink{}
	s := &sessionViewOfferSender{sink: sink, minter: nil}
	env := mustEnv(t, channelevents.KindSessionViewOffer,
		channelevents.SessionViewOfferPayload{SessionRef: "default/sess-1"})

	_, err := s.Send(context.Background(), sessInfo(), env)
	require.Error(t, err, "nil minter must return an error, not skip silently")
	require.Len(t, sink.Events(), 1)
	se, ok := sink.Events()[0].(MsgSendError)
	require.True(t, ok, "want MsgSendError, got %T", sink.Events()[0])
	assert.Contains(t, se.Err, "not configured", "user-visible reason must name the misconfiguration")
}

// TestSessionViewOfferSender_EmptyURL_EmitsSendError verifies that when the
// minter returns ("", nil) — webd's external-URL ConfigMap is not populated
// — the offer surfaces LOUDLY rather than being dropped silently.
func TestSessionViewOfferSender_EmptyURL_EmitsSendError(t *testing.T) {
	sink := &RecordingSink{}
	s := &sessionViewOfferSender{
		sink:   sink,
		minter: &fakeSessionViewMinter{url: ""},
	}
	env := mustEnv(t, channelevents.KindSessionViewOffer,
		channelevents.SessionViewOfferPayload{SessionRef: "default/sess-1"})

	_, err := s.Send(context.Background(), sessInfo(), env)
	require.Error(t, err, "empty URL must return an error, not skip silently")
	require.Len(t, sink.Events(), 1)
	se, ok := sink.Events()[0].(MsgSendError)
	require.True(t, ok, "want MsgSendError, got %T", sink.Events()[0])
	assert.Contains(t, se.Err, "external URL", "user-visible reason must name the missing external URL")
}

// TestSessionViewOfferSender_MintError_EmitsSendError verifies that a mint
// error surfaces as a MsgSendError event and a non-nil return error.
func TestSessionViewOfferSender_MintError_EmitsSendError(t *testing.T) {
	sink := &RecordingSink{}
	s := &sessionViewOfferSender{
		sink:   sink,
		minter: &fakeSessionViewMinter{err: fmt.Errorf("malformed sessionRef")},
	}
	env := mustEnv(t, channelevents.KindSessionViewOffer,
		channelevents.SessionViewOfferPayload{SessionRef: "default/sess-1"})

	_, err := s.Send(context.Background(), sessInfo(), env)
	require.Error(t, err)
	require.Len(t, sink.Events(), 1)
	se, ok := sink.Events()[0].(MsgSendError)
	require.True(t, ok, "want MsgSendError, got %T", sink.Events()[0])
	assert.Contains(t, se.Err, "malformed sessionRef")
}

// TestSessionViewOfferSender_MissingSessionRef_EmitsSendError verifies that
// an envelope with an empty sessionRef surfaces as a MsgSendError.
func TestSessionViewOfferSender_MissingSessionRef_EmitsSendError(t *testing.T) {
	sink := &RecordingSink{}
	s := &sessionViewOfferSender{
		sink:   sink,
		minter: &fakeSessionViewMinter{url: "https://webd.example.com/session-view/default/sess-1"},
	}
	env := mustEnv(t, channelevents.KindSessionViewOffer,
		channelevents.SessionViewOfferPayload{SessionRef: ""})

	_, err := s.Send(context.Background(), sessInfo(), env)
	require.Error(t, err)
	require.Len(t, sink.Events(), 1)
	_, ok := sink.Events()[0].(MsgSendError)
	require.True(t, ok, "want MsgSendError, got %T", sink.Events()[0])
}

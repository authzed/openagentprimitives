package local

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

func screenshotRequestEnv(t *testing.T) channelevents.Envelope {
	t.Helper()
	return mustEnv(t, channelevents.KindInteractionRequest,
		channelevents.InteractionRequestPayload{
			AgentSessionRef: channelevents.SessionRef{Namespace: "default", Name: "s1"},
			Category:        "acting_approval",
			RequestRef:      "act-1",
			Lead:            "Approve this browser action?",
			Screenshot: &channelevents.AttachmentRef{
				RenderName: "shot-abc",
				ArtifactID: "artifact-xyz",
				MIME:       channelevents.ScreenshotMIME,
				Filename:   "page.png",
			},
			Audience: channelevents.InteractionAudience{
				Scope:     channelevents.AudienceRequester,
				Requester: &channelevents.ExternalIdentity{Kind: "user", ExternalID: "user-1"},
			},
		})
}

// TestInteractionSender_Screenshot_WiredMinter_EmitsLinkLine verifies that
// when a browser-originated interaction carries a Screenshot and the sender
// has a working ArtifactViewMinter + principal wired (as host.go wires for
// the "interaction" sub-channel, mirroring live_view_offer's), the rendered
// Text includes the screenshot reference line with the minted URL.
func TestInteractionSender_Screenshot_WiredMinter_EmitsLinkLine(t *testing.T) {
	sink := &RecordingSink{}
	s := &interactionSender{
		sink:      newInertSink(sink),
		minter:    &fakeMinter{url: "https://webd.example.com/artifact-view?d=abc&sig=def"},
		principal: identity.RawSubject("user:alice"),
	}

	_, err := s.Send(context.Background(), sessInfo(), screenshotRequestEnv(t))
	require.NoError(t, err)
	require.Len(t, sink.Events(), 1)
	m, ok := sink.Events()[0].(MsgInteractionRequest)
	require.True(t, ok, "want MsgInteractionRequest, got %T", sink.Events()[0])
	assert.Contains(t, m.Text, "Page screenshot:")
	assert.Contains(t, m.Text, "https://webd.example.com/artifact-view?d=abc&sig=def")
}

// TestInteractionSender_Screenshot_NilMinter_NoLineNoError verifies the
// best-effort contract: an unwired minter must never block or error the
// card — it only means no screenshot line, exactly like a text payload with
// no Screenshot at all.
func TestInteractionSender_Screenshot_NilMinter_NoLineNoError(t *testing.T) {
	sink := &RecordingSink{}
	s := &interactionSender{sink: newInertSink(sink)}

	_, err := s.Send(context.Background(), sessInfo(), screenshotRequestEnv(t))
	require.NoError(t, err, "a nil minter must never fail the card")
	require.Len(t, sink.Events(), 1)
	m, ok := sink.Events()[0].(MsgInteractionRequest)
	require.True(t, ok, "want MsgInteractionRequest, got %T", sink.Events()[0])
	assert.NotContains(t, m.Text, "Page screenshot:")
}

// TestInteractionSender_Screenshot_MinterErrors_NoLineNoError verifies that a
// minting error is swallowed into "no line" rather than propagated as a
// Send error — the card (Lead/Body/Actions) must still be delivered.
func TestInteractionSender_Screenshot_MinterErrors_NoLineNoError(t *testing.T) {
	sink := &RecordingSink{}
	s := &interactionSender{
		sink:      newInertSink(sink),
		minter:    &fakeMinter{err: errors.New("mint failed")},
		principal: identity.RawSubject("user:alice"),
	}

	_, err := s.Send(context.Background(), sessInfo(), screenshotRequestEnv(t))
	require.NoError(t, err, "a minter error must never fail the card")
	require.Len(t, sink.Events(), 1)
	m, ok := sink.Events()[0].(MsgInteractionRequest)
	require.True(t, ok, "want MsgInteractionRequest, got %T", sink.Events()[0])
	assert.NotContains(t, m.Text, "Page screenshot:")
}

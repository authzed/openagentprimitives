package channelevents

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDeliveryOperationReplayAndPayloadPinning(t *testing.T) {
	p := OutboundUserMessagePayload{Text: "Stand up and stretch!"}
	first, err := NewDeliveryOperation("session-uid", "tool-call", p)
	require.NoError(t, err)
	p.Delivery = first
	require.NoError(t, p.Validate())
	replayed, err := NewDeliveryOperation("session-uid", "tool-call", p)
	require.NoError(t, err)
	require.Equal(t, first, replayed, "replay excludes the descriptor from the digest")

	p.Text = "A different reply"
	require.Error(t, p.Validate(), "one operation cannot silently change its body")
	changed, err := NewDeliveryOperation("session-uid", "tool-call", p)
	require.NoError(t, err)
	require.Equal(t, first.ID, changed.ID)
	require.NotEqual(t, first.PayloadDigest, changed.PayloadDigest)

	for _, pair := range [][2]string{{"replacement-uid", "tool-call"}, {"session-uid", "next-call"}} {
		op, err := NewDeliveryOperation(pair[0], pair[1], p)
		require.NoError(t, err)
		require.NotEqual(t, first.ID, op.ID)
	}
	_, err = NewDeliveryOperation("", "tool-call", p)
	require.Error(t, err)
	_, err = NewDeliveryOperation("session-uid", "", p)
	require.Error(t, err)
}

func TestDeliveryOperationPinsAttachmentsAndOpening(t *testing.T) {
	p := OutboundUserMessagePayload{Text: "Report", Attachments: []AttachmentRef{{RenderName: "render", ArtifactID: "original"}}}
	op, err := NewDeliveryOperation("uid", "call", p)
	require.NoError(t, err)
	p.Delivery = op
	p.Attachments[0].ArtifactID = "replacement"
	require.Error(t, p.Validate())
	p.Attachments[0].ArtifactID = "original"
	p.Opening = &SessionOpening{Summary: "Summary", Instructions: "Exact instructions"}
	require.Error(t, p.Validate())
	p.Opening = nil
	require.NoError(t, p.Validate())
	p.Delivery.ID = "arbitrary-model-selected-id"
	require.Error(t, p.Validate())

	a, err := NewDeliveryOperation("ab", "c", p)
	require.NoError(t, err)
	b, err := NewDeliveryOperation("a", "bc", p)
	require.NoError(t, err)
	require.NotEqual(t, a.ID, b.ID, "identity boundaries are unambiguous")
}

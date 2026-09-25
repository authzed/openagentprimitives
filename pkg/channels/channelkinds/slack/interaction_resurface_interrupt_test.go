package slack

import (
	"testing"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
)

// TestAppendInteractionResurfaceInterrupt exercises
// appendInteractionResurfaceInterrupt directly against the generic
// interaction renderer's block output: when the envelope carries a
// non-empty ResurfaceInterruptRequestID, an "Interrupt & Send Now" action
// block — encoded via the generic discInteraction codec, category
// categories.QueuedMessages, actionID "interrupt" — must be appended;
// otherwise the rendered blocks pass through unchanged.
func TestAppendInteractionResurfaceInterrupt(t *testing.T) {
	p := channelevents.InteractionRequestPayload{
		Category:   categories.ToolApproval,
		RequestRef: "appr-1",
		Lead:       "Approval needed",
		Actions: []channelevents.InteractionAction{
			{ID: "approve", Label: "Approve", Kind: channelevents.ActionKindDecision, Style: channelevents.ActionStylePrimary},
			{ID: "deny", Label: "Deny", Kind: channelevents.ActionKindDecision, Style: channelevents.ActionStyleDanger},
		},
	}
	base := buildInteractionRequestBlocks(p, "ns/name")

	withInterrupt := appendInteractionResurfaceInterrupt(base, channelevents.Envelope{ResurfaceInterruptRequestID: "int-9"}, "ns/name")
	require.Len(t, withInterrupt, len(base)+1, "interrupt block appended")

	last, ok := withInterrupt[len(withInterrupt)-1].(*slackapi.ActionBlock)
	require.True(t, ok, "expected the appended block to be *slackapi.ActionBlock")
	require.Len(t, last.Elements.ElementSet, 1, "expected a single-button action block")
	btn, ok := last.Elements.ElementSet[0].(*slackapi.ButtonBlockElement)
	require.True(t, ok, "expected *slackapi.ButtonBlockElement")

	assert.Equal(t, "Interrupt & Send Now", btn.Text.Text, "must reproduce the legacy button label verbatim")
	assert.Equal(t, slackapi.PlainTextType, btn.Text.Type)
	if assert.NotNil(t, btn.Text.Emoji, "legacy button rendered with emoji:true") {
		assert.True(t, *btn.Text.Emoji, "legacy button rendered with emoji:true")
	}
	assert.Equal(t, interactionResurfaceInterruptActionID, btn.ActionID)
	assert.Equal(t, slackapi.StyleDefault, btn.Style, "legacy button carried no style")

	decoded, ok := decodeApprovalButtonValue(btn.Value)
	require.True(t, ok, "appended button's value must decode")
	assert.Equal(t, discInteraction, decoded.V, "must use the generic discInteraction codec, not the legacy discInterrupt one")
	assert.Equal(t, "int-9", decoded.R, "requestRef must be the envelope's ResurfaceInterruptRequestID")
	assert.Equal(t, "interrupt", decoded.D, "actionID must be \"interrupt\", matching decideQueuedInterrupt's bound category")
	assert.Equal(t, categories.QueuedMessages, decoded.C, "category must be QueuedMessages so the click routes to decideQueuedInterrupt")
	assert.Equal(t, "ns/name", decoded.S)

	// Base blocks (approve/deny) must be untouched — the interrupt button is
	// its own trailing action block, not folded into the existing one.
	assert.Equal(t, base, withInterrupt[:len(base)], "existing blocks must be unchanged and unreordered")

	// Empty ResurfaceInterruptRequestID => no append (not a re-surface).
	same := appendInteractionResurfaceInterrupt(base, channelevents.Envelope{}, "ns/name")
	assert.Len(t, same, len(base), "no ResurfaceInterruptRequestID must mean no button")
	assert.Equal(t, base, same)
}

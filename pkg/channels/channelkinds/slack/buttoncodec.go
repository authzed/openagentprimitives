// pkg/channels/channelkinds/slack/buttoncodec.go
//
// Shared encode/decode for Slack block-action button values. Both the senders
// (encode) and the listener (decode) use these so the wire shape lives in one
// place.
//
// A discriminator's string value is a WIRE value: a button posted before a
// rollout is still clickable after it, so changing one silently breaks every
// message already in a channel. Three are produced — the agent-settings modal
// (discShowSettings), Interaction decisions (discInteraction), and the
// Interaction Show-Details modal (discInteractionDetails); anything else is
// treated as foreign and refused by decodeApprovalButtonValue.
package slack

import "encoding/json"

const (
	// discShowSettings discriminates the "Show settings" button on agent
	// messages (show_settings.go); opens the effectiveSettings modal.
	discShowSettings = "show_settings"
	// discInteraction discriminates an Interaction-model decision action
	// (channelevents' ActionKindDecision): identity_choice,
	// permission_request and queued_messages today, any decision-kind
	// category generically — see the C field below.
	discInteraction = "interaction"
	// discInteractionDetails discriminates the Show-Details button
	// buildInteractionRequestBlocks appends whenever
	// InteractionRequestPayload.Details is populated. Decoded by
	// handleInteractionDetailsAction (interaction_details.go).
	discInteractionDetails = "interaction_details"
)

// approvalButtonValue is the compact JSON blob carried as a block-action value.
// Short keys leave headroom under Slack's 2000-char cap.
type approvalButtonValue struct {
	// V names which flow rendered the button; an unrecognized value is refused.
	V string `json:"v"`
	// R is the requestID, or the interaction requestRef under discInteraction.
	R string `json:"r"`
	// D is the decision (approve|deny), or the actionId under discInteraction.
	D string `json:"d"`
	// S is the session this click belongs to, as "namespace/name".
	S string `json:"s"`
	// C is the interaction category ("identity_choice"), empty outside
	// discInteraction. It rides in the button because the render-time
	// InteractionRequestPayload is the only place the category is known, and
	// channelsd's decision pipe looks it up in the channelinteractions registry
	// before it will dispatch the click.
	C string `json:"c,omitempty"`
}

// encodeApprovalButtonValue encodes a non-interaction button value (the agent
// settings modal today): {v:disc, r:requestID, d:decision, s:sessRef}. The C
// (category) field stays empty and is omitted from the wire encoding.
func encodeApprovalButtonValue(disc, requestID, decision, sessRef string) string {
	blob, _ := json.Marshal(approvalButtonValue{V: disc, R: requestID, D: decision, S: sessRef})
	return string(blob)
}

// encodeInteractionButtonValue encodes a generic Interaction-model
// decision-action button value: {v:"interaction", r:requestRef, d:actionId,
// c:category, s:sessRef}. See approvalButtonValue's C field comment for why
// category must ride along here.
func encodeInteractionButtonValue(requestRef, actionID, category, sessRef string) string {
	blob, _ := json.Marshal(approvalButtonValue{V: discInteraction, R: requestRef, D: actionID, C: category, S: sessRef})
	return string(blob)
}

// decodeApprovalButtonValue parses a button value and reports whether it is one
// of the known surviving discriminators.
func decodeApprovalButtonValue(raw string) (approvalButtonValue, bool) {
	var v approvalButtonValue
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return approvalButtonValue{}, false
	}
	switch v.V {
	case discShowSettings, discInteraction, discInteractionDetails:
		return v, true
	default:
		return approvalButtonValue{}, false
	}
}

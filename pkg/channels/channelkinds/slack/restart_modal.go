package slack

import (
	"encoding/json"
	"fmt"

	slackapi "github.com/slack-go/slack"
)

// RestartModalCallbackID is the view callback_id for the
// "Restart from here" modal. Slack's view_submission handler
// dispatches on this.
const RestartModalCallbackID = "ap_restart_modal"

// RestartTextBlockID + RestartTextActionID name the input block + element
// the listener reads back from view_submission state.values.
const (
	RestartTextBlockID  = "restart_text_block"
	RestartTextActionID = "restart_text"
)

// RestartModalArgs are the inputs to BuildRestartModal.
type RestartModalArgs struct {
	OriginalText     string
	DiscardCount     int // number of messages that will be discarded
	SessionNamespace string
	SessionName      string
	ChannelID        string
	ThreadTS         string
	MessageTS        string

	// SiblingFork is true when the resolved session has
	// status.supersededBy set — i.e., the user clicked restart on a
	// session that's already been forked. The modal renders a
	// different header to make clear this creates a sibling fork
	// rather than a fresh restart.
	SiblingFork bool
}

// RestartPrivateMetadata is the modal's private_metadata payload.
// Encoded as JSON; round-tripped to the view_submission handler.
type RestartPrivateMetadata struct {
	SessionNamespace string `json:"ns"`
	SessionName      string `json:"sess"`
	// ChannelID is where the restarted message lives; empty in a DM-only bind.
	ChannelID string `json:"ch"`
	// ThreadTS is the enclosing thread; empty for a top-level message.
	ThreadTS string `json:"th"`
	// MessageTS is the cut point — the message the user chose to restart from.
	MessageTS string `json:"ts"`
}

// BuildRestartModal returns a Slack ModalViewRequest pre-filled with
// the original message text + a header noting the discard count.
func BuildRestartModal(args RestartModalArgs) slackapi.ModalViewRequest {
	pm := EncodeRestartPrivateMetadata(args)
	var headerText string
	if args.SiblingFork {
		headerText = fmt.Sprintf("⚠ This session has already been restarted once. Submitting will create a sibling fork at this point. %d message(s) after the cut point will be excluded.", args.DiscardCount)
	} else {
		headerText = fmt.Sprintf("Editing this message will discard %d message(s) after it.", args.DiscardCount)
	}

	inputElem := slackapi.NewPlainTextInputBlockElement(
		slackapi.NewTextBlockObject(slackapi.PlainTextType, "Type your edited message", false, false),
		RestartTextActionID,
	).WithInitialValue(args.OriginalText).WithMultiline(true)

	blocks := slackapi.Blocks{
		BlockSet: []slackapi.Block{
			slackapi.NewSectionBlock(
				slackapi.NewTextBlockObject(slackapi.MarkdownType, headerText, false, false),
				nil, nil,
			),
			slackapi.NewInputBlock(
				RestartTextBlockID,
				slackapi.NewTextBlockObject(slackapi.PlainTextType, "Edited message", false, false),
				nil,
				inputElem,
			),
		},
	}
	return slackapi.ModalViewRequest{
		Type:            slackapi.VTModal,
		CallbackID:      RestartModalCallbackID,
		Title:           slackapi.NewTextBlockObject(slackapi.PlainTextType, "Restart from here", false, false),
		Submit:          slackapi.NewTextBlockObject(slackapi.PlainTextType, "Restart", false, false),
		Close:           slackapi.NewTextBlockObject(slackapi.PlainTextType, "Cancel", false, false),
		Blocks:          blocks,
		PrivateMetadata: pm,
	}
}

// EncodeRestartPrivateMetadata serializes args into a JSON string
// suitable for view.PrivateMetadata.
func EncodeRestartPrivateMetadata(args RestartModalArgs) string {
	pm := RestartPrivateMetadata{
		SessionNamespace: args.SessionNamespace,
		SessionName:      args.SessionName,
		ChannelID:        args.ChannelID,
		ThreadTS:         args.ThreadTS,
		MessageTS:        args.MessageTS,
	}
	raw, err := json.Marshal(pm)
	if err != nil {
		// RestartPrivateMetadata is a struct of plain strings; Marshal
		// cannot fail on it. Panic here so the impossible case is loud
		// rather than silently producing an empty or corrupt modal.
		panic(fmt.Sprintf("EncodeRestartPrivateMetadata: json.Marshal must not fail on struct of strings: %v", err))
	}
	return string(raw)
}

// DecodeRestartPrivateMetadata parses the view's private_metadata
// back into RestartPrivateMetadata.
func DecodeRestartPrivateMetadata(s string) (RestartPrivateMetadata, error) {
	var pm RestartPrivateMetadata
	if err := json.Unmarshal([]byte(s), &pm); err != nil {
		return pm, fmt.Errorf("DecodeRestartPrivateMetadata: %w", err)
	}
	return pm, nil
}

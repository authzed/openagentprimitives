package local

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// interactionSender is the host-side "interaction" sub-channel sender:
// semantic interaction requests (credential_link, identity_choice, etc.). It
// emits a MsgInteractionRequest carrying both the structured Payload and a
// RenderText markdown floor. The TUI drives decision INPUT off the Payload's
// ActionKindDecision actions (a blocking modal — see chat_tui.go); the Text
// floor is used for the read-only (link / notice) path where no modal opens.
type interactionSender struct {
	sink *inertSink
}

func (s *interactionSender) emitErr(sess channelkinds.SessionInfo, k channelevents.Kind, err error) (channelkinds.SubChannelSendResult, error) {
	s.sink.Emit(MsgSendError{Session: refOf(sess), Kind: k, Err: err.Error(), At: timeNowUTC()})
	return channelkinds.SubChannelSendResult{}, fmt.Errorf("local interaction sender (%s): %w", k, err)
}

func (s *interactionSender) Send(_ context.Context, sess channelkinds.SessionInfo, env channelevents.Envelope) (channelkinds.SubChannelSendResult, error) {
	switch env.Kind {
	case channelevents.KindInteractionRequest:
		var pl channelevents.InteractionRequestPayload
		if err := json.Unmarshal(env.Payload, &pl); err != nil {
			return s.emitErr(sess, env.Kind, err)
		}
		// The ONE sweep any sender in this package still does itself, and it is
		// about ORDERING, not coverage — the sink sweeps these fields anyway.
		// RenderText below composes markdown AROUND these slots, so it must run
		// on already-swept text. See NOTES.md.
		pl = inertInteractionPayload(pl)

		// nil decisionURL: link actions use their own URL, decision actions
		// render as plain labels plus a note to respond from a connected app.
		// The rendered Text is the read-only timeline floor; Payload carries the
		// structured actions the TUI's decision modal binds keys to.
		text := channelinteractions.RenderText(pl, nil)
		s.sink.Emit(MsgInteractionRequest{Session: refOf(sess), Payload: pl, Text: text})
		return channelkinds.SubChannelSendResult{}, nil

	case channelevents.KindInteractionApplied:
		var pl channelevents.InteractionAppliedPayload
		if err := json.Unmarshal(env.Payload, &pl); err != nil {
			return s.emitErr(sess, env.Kind, err)
		}
		// OutcomeText lands in the same timeline as the prompt above and is
		// runner-controlled, so it goes through the same door as everything
		// else this kind emits: one live rendering beside one inert one, for
		// the same interaction, is worse than either alone.
		s.sink.Emit(MsgInteractionApplied{
			Session: refOf(sess), Category: pl.Category,
			OutcomeText: pl.OutcomeText, Outcome: pl.Outcome,
		})
		return channelkinds.SubChannelSendResult{}, nil

	case channelevents.KindInteractionDecisionRejected:
		var pl channelevents.InteractionDecisionRejectedPayload
		if err := json.Unmarshal(env.Payload, &pl); err != nil {
			return s.emitErr(sess, env.Kind, err)
		}
		s.sink.Emit(MsgInteractionRejected{
			Session: refOf(sess), Class: pl.Class,
			Reason:          pl.Reason,
			OriginalOutcome: pl.OriginalOutcome,
		})
		return channelkinds.SubChannelSendResult{}, nil

	default:
		return s.emitErr(sess, env.Kind, fmt.Errorf("unsupported envelope kind for the interaction sender"))
	}
}

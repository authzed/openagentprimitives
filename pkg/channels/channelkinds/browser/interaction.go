package browser

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// interactionSender is the host-side "interaction" sub-channel sender for the
// browser page: KindInteractionRequest → MsgInteractionRequest (a prompt
// the user can act on); KindInteractionApplied → MsgInteractionApplied (the
// decision/timeout outcome, rendered as an in-place edit of the prompt).
// Translation only — the unified Interaction model (pkg/channels/channelevents/interaction.go)
// carries no per-category rendering logic, and neither does this sender.
type interactionSender struct {
	sink EventSink
}

// emitErr emits a visible MsgSendError and returns a wrapped error — both
// surfacing paths fire, matching every sibling sub-channel sender (AGENTS.md:
// never silently drop).
func (s *interactionSender) emitErr(sess channelkinds.SessionInfo, k channelevents.Kind, err error) (channelkinds.SubChannelSendResult, error) {
	s.sink.Emit(MsgSendError{Session: refOf(sess), Kind: k, Err: err.Error(), At: timeNowUTC()})
	return channelkinds.SubChannelSendResult{}, fmt.Errorf("browser interaction sender (%s): %w", k, err)
}

func (s *interactionSender) Send(_ context.Context, sess channelkinds.SessionInfo, env channelevents.Envelope) (channelkinds.SubChannelSendResult, error) {
	switch env.Kind {
	case channelevents.KindInteractionRequest:
		var pl channelevents.InteractionRequestPayload
		if err := json.Unmarshal(env.Payload, &pl); err != nil {
			return s.emitErr(sess, env.Kind, err)
		}
		s.sink.Emit(MsgInteractionRequest{Session: refOf(sess), Payload: pl})
		return channelkinds.SubChannelSendResult{}, nil

	case channelevents.KindInteractionApplied:
		var pl channelevents.InteractionAppliedPayload
		if err := json.Unmarshal(env.Payload, &pl); err != nil {
			return s.emitErr(sess, env.Kind, err)
		}
		s.sink.Emit(MsgInteractionApplied{Session: refOf(sess), Payload: pl})
		return channelkinds.SubChannelSendResult{}, nil

	case channelevents.KindInteractionDecisionRejected:
		var pl channelevents.InteractionDecisionRejectedPayload
		if err := json.Unmarshal(env.Payload, &pl); err != nil {
			return s.emitErr(sess, env.Kind, err)
		}
		s.sink.Emit(MsgInteractionRejected{Session: refOf(sess), Payload: pl})
		return channelkinds.SubChannelSendResult{}, nil

	default:
		return s.emitErr(sess, env.Kind, fmt.Errorf("unsupported envelope kind for the interaction sender"))
	}
}

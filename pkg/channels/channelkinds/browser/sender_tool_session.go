package browser

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// toolSessionSender is the host-side "tool_session" sub-channel sender:
// KindToolSessionEvent (parsed events) + KindToolSessionDelta (raw
// chunks, incl. the terminal exit delta) → render events.
type toolSessionSender struct {
	sink EventSink
}

func (s *toolSessionSender) emitErr(sess channelkinds.SessionInfo, k channelevents.Kind, err error) (channelkinds.SubChannelSendResult, error) {
	s.sink.Emit(MsgSendError{Session: refOf(sess), Kind: k, Err: err.Error(), At: timeNowUTC()})
	return channelkinds.SubChannelSendResult{}, fmt.Errorf("browser tool_session sender (%s): %w", k, err)
}

func (s *toolSessionSender) Send(_ context.Context, sess channelkinds.SessionInfo, env channelevents.Envelope) (channelkinds.SubChannelSendResult, error) {
	switch env.Kind {
	case channelevents.KindToolSessionEvent:
		var pl channelevents.ToolSessionEventPayload
		if err := json.Unmarshal(env.Payload, &pl); err != nil {
			return s.emitErr(sess, env.Kind, err)
		}
		s.sink.Emit(MsgToolSessionEvent{Session: refOf(sess), Payload: pl})
		return channelkinds.SubChannelSendResult{}, nil

	case channelevents.KindToolSessionDelta:
		var pl channelevents.ToolSessionDeltaPayload
		if err := json.Unmarshal(env.Payload, &pl); err != nil {
			return s.emitErr(sess, env.Kind, err)
		}
		s.sink.Emit(MsgToolSessionDelta{Session: refOf(sess), Payload: pl})
		return channelkinds.SubChannelSendResult{}, nil

	default:
		return s.emitErr(sess, env.Kind, fmt.Errorf("unsupported envelope kind for the tool_session sender"))
	}
}

package browser

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// streamDeltaSink is the host-side StreamDeltaSink: it translates each
// KindAssistantStreamDelta envelope into an MsgStreamDelta render event.
// No debounce — the browser page coalesces text_delta runs. Safe for
// concurrent OnDelta (the EventSink is).
type streamDeltaSink struct {
	sink EventSink
}

func (s *streamDeltaSink) OnDelta(_ context.Context, sess channelkinds.SessionInfo, env channelevents.Envelope) error {
	var pl channelevents.AssistantStreamDeltaPayload
	if err := json.Unmarshal(env.Payload, &pl); err != nil {
		s.sink.Emit(MsgSendError{
			Session: refOf(sess), Kind: env.Kind, Err: err.Error(), At: timeNowUTC(),
		})
		return fmt.Errorf("browser stream sink: decode AssistantStreamDeltaPayload: %w", err)
	}
	s.sink.Emit(MsgStreamDelta{Session: refOf(sess), Payload: pl})
	return nil
}

package local

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// streamDeltaSink is the host-side StreamDeltaSink: it translates each
// KindAssistantStreamDelta envelope into an MsgStreamDelta render event.
// No debounce — the bubbletea model coalesces text_delta runs in its
// Update path. Safe for concurrent OnDelta (the EventSink is).
type streamDeltaSink struct {
	sink *inertSink
}

func (s *streamDeltaSink) OnDelta(_ context.Context, sess channelkinds.SessionInfo, env channelevents.Envelope) error {
	var pl channelevents.AssistantStreamDeltaPayload
	if err := json.Unmarshal(env.Payload, &pl); err != nil {
		s.sink.Emit(MsgSendError{
			Session: refOf(sess), Kind: env.Kind, Err: err.Error(), At: timeNowUTC(),
		})
		return fmt.Errorf("local stream sink: decode AssistantStreamDeltaPayload: %w", err)
	}
	// The streamed half of the same reply localSender emits whole, appended to
	// the timeline token by token. The sink sweeps it per chunk, and a sequence
	// split across two chunks is dropped rather than reassembled — which is the
	// point: the terminal would have joined them.
	s.sink.Emit(MsgStreamDelta{Session: refOf(sess), Payload: pl})
	return nil
}

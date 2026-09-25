package local

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

func TestToolSessionSender_Send(t *testing.T) {
	cases := []struct {
		name   string
		env    func(t *testing.T) channelevents.Envelope
		assert func(t *testing.T, ev any)
	}{
		{
			name: "tool_session_event: emits MsgToolSessionEvent",
			env: func(t *testing.T) channelevents.Envelope {
				return mustEnv(t, channelevents.KindToolSessionEvent,
					channelevents.ToolSessionEventPayload{
						ToolCallRef: "tc-1", EventType: "tool_use_start",
						ToolName: "Write", OuterTool: "claude", Reason: "write the README",
					})
			},
			assert: func(t *testing.T, ev any) {
				m, ok := ev.(MsgToolSessionEvent)
				require.True(t, ok, "want MsgToolSessionEvent, got %T", ev)
				assert.Equal(t, "tc-1", m.Payload.ToolCallRef)
				assert.Equal(t, "tool_use_start", m.Payload.EventType)
			},
		},
		{
			name: "tool_session_delta: emits MsgToolSessionDelta",
			env: func(t *testing.T) channelevents.Envelope {
				return mustEnv(t, channelevents.KindToolSessionDelta,
					channelevents.ToolSessionDeltaPayload{
						ToolCallRef: "tc-1", Stream: "stdout", Data: []byte("line\n"),
					})
			},
			assert: func(t *testing.T, ev any) {
				m, ok := ev.(MsgToolSessionDelta)
				require.True(t, ok, "want MsgToolSessionDelta, got %T", ev)
				assert.Equal(t, "stdout", m.Payload.Stream)
				assert.Equal(t, []byte("line\n"), m.Payload.Data)
			},
		},
		{
			name: "tool_session_delta terminal: emits MsgToolSessionDelta with Terminal",
			env: func(t *testing.T) channelevents.Envelope {
				return mustEnv(t, channelevents.KindToolSessionDelta,
					channelevents.ToolSessionDeltaPayload{
						ToolCallRef: "tc-1", Stream: "stdout",
						Terminal: true, ExitReason: "completed", ExitCode: 0,
					})
			},
			assert: func(t *testing.T, ev any) {
				m := ev.(MsgToolSessionDelta)
				assert.True(t, m.Payload.Terminal)
				assert.Equal(t, "completed", m.Payload.ExitReason)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := &RecordingSink{}
			s := &toolSessionSender{sink: newInertSink(sink)}
			_, err := s.Send(context.Background(), sessInfo(), tc.env(t))
			require.NoError(t, err)
			require.Len(t, sink.Events(), 1)
			tc.assert(t, sink.Events()[0])
		})
	}
}

func TestToolSessionSender_UnsupportedKind_EmitsSendError(t *testing.T) {
	sink := &RecordingSink{}
	s := &toolSessionSender{sink: newInertSink(sink)}
	env := mustEnv(t, channelevents.KindUserMessage,
		channelevents.OutboundUserMessagePayload{Text: "x"})
	_, err := s.Send(context.Background(), sessInfo(), env)
	require.Error(t, err)
	assert.IsType(t, MsgSendError{}, sink.Events()[0])
}

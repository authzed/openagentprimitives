package local

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

func TestQueuedMessagesSender_InterruptApplied_EmitsMsgInterruptApplied(t *testing.T) {
	sink := &RecordingSink{}
	s := &queuedMessagesSender{sink: newInertSink(sink)}
	env := mustEnv(t, channelevents.KindInterruptApplied,
		channelevents.InterruptAppliedPayload{RequestID: "req-1", Outcome: "interrupted", Reason: "user requested"})
	res, err := s.Send(context.Background(), sessInfo(), env)
	require.NoError(t, err)
	assert.Equal(t, channelkinds.SubChannelSendResult{}, res)
	require.Len(t, sink.Events(), 1)
	m, ok := sink.Events()[0].(MsgInterruptApplied)
	require.True(t, ok, "want MsgInterruptApplied, got %T", sink.Events()[0])
	assert.Equal(t, "req-1", m.RequestID)
	assert.Equal(t, "interrupted", m.Outcome)
	assert.Equal(t, "user requested", m.Reason)
	assert.Equal(t, "s1", m.Session.Name)
}

func TestQueuedMessagesSender_MalformedPayload_EmitsSendErrorAndReturnsErr(t *testing.T) {
	sink := &RecordingSink{}
	s := &queuedMessagesSender{sink: newInertSink(sink)}
	bad := channelevents.Envelope{
		Version:     1,
		Kind:        channelevents.KindInterruptApplied,
		Session:     channelevents.SessionRef{Namespace: "default", Name: "s1"},
		Payload:     []byte(`{"outcome": 12345}`), // outcome must be a string
		PublishedAt: timeNowUTC(),
	}
	_, err := s.Send(context.Background(), sessInfo(), bad)
	require.Error(t, err, "malformed payload must return an error to the caller")
	require.Len(t, sink.Events(), 1)
	se, ok := sink.Events()[0].(MsgSendError)
	require.True(t, ok, "want MsgSendError, got %T", sink.Events()[0])
	assert.Equal(t, channelevents.KindInterruptApplied, se.Kind)
}

func TestQueuedMessagesSender_UnsupportedKind_IsNoOp(t *testing.T) {
	// Future ack kinds land on this sub-channel too (P2's queue+confirm
	// flow); until then, anything else is a silent no-op — not a
	// surfaced send_error — so a not-yet-handled kind doesn't spam the
	// timeline with warnings.
	sink := &RecordingSink{}
	s := &queuedMessagesSender{sink: newInertSink(sink)}
	env := mustEnv(t, channelevents.KindToolActivity, channelevents.ToolActivityPayload{Tool: "x"})
	res, err := s.Send(context.Background(), sessInfo(), env)
	require.NoError(t, err)
	assert.Equal(t, channelkinds.SubChannelSendResult{}, res)
	assert.Empty(t, sink.Events())
}

package local

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

func TestStreamDeltaSink_OnDelta(t *testing.T) {
	sink := &RecordingSink{}
	sds := &streamDeltaSink{sink: newInertSink(sink)}
	env := mustEnv(t, channelevents.KindAssistantStreamDelta,
		channelevents.AssistantStreamDeltaPayload{EventType: "text_delta", Text: "hel", BlockIdx: 0})
	require.NoError(t, sds.OnDelta(context.Background(), sessInfo(), env))
	require.Len(t, sink.Events(), 1)
	m, ok := sink.Events()[0].(MsgStreamDelta)
	require.True(t, ok, "want MsgStreamDelta, got %T", sink.Events()[0])
	assert.Equal(t, "text_delta", m.Payload.EventType)
	assert.Equal(t, "hel", m.Payload.Text)
}

func TestStreamDeltaSink_MalformedPayload_EmitsSendErrorAndReturnsErr(t *testing.T) {
	sink := &RecordingSink{}
	sds := &streamDeltaSink{sink: newInertSink(sink)}
	bad := channelevents.Envelope{
		Version: 1, Kind: channelevents.KindAssistantStreamDelta,
		Session:     channelevents.SessionRef{Namespace: "default", Name: "s1"},
		Payload:     []byte(`{"blockIdx":"not-an-int"}`),
		PublishedAt: timeNowUTC(),
	}
	err := sds.OnDelta(context.Background(), sessInfo(), bad)
	require.Error(t, err)
	assert.IsType(t, MsgSendError{}, sink.Events()[0])
}

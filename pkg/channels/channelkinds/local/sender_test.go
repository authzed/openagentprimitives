package local

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

func sessInfo() channelkinds.SessionInfo {
	return channelkinds.SessionInfo{Namespace: "default", Name: "s1"}
}

func mustEnv(t *testing.T, k channelevents.Kind, payload any) channelevents.Envelope {
	t.Helper()
	env, err := channelevents.BuildEnvelope("default", "s1", k, payload)
	require.NoError(t, err)
	return env
}

func TestLocalSender_Send(t *testing.T) {
	cases := []struct {
		name   string
		env    func(t *testing.T) channelevents.Envelope
		assert func(t *testing.T, ev any)
	}{
		{
			name: "user_message: emits MsgUserMessage",
			env: func(t *testing.T) channelevents.Envelope {
				return mustEnv(t, channelevents.KindUserMessage,
					channelevents.OutboundUserMessagePayload{Text: "the README is written"})
			},
			assert: func(t *testing.T, ev any) {
				m, ok := ev.(MsgUserMessage)
				require.True(t, ok, "want MsgUserMessage, got %T", ev)
				assert.Equal(t, "the README is written", m.Text)
				assert.Equal(t, "s1", m.Session.Name)
			},
		},
		{
			name: "notification: emits MsgNotification",
			env: func(t *testing.T) channelevents.Envelope {
				return mustEnv(t, channelevents.KindNotification,
					channelevents.NotificationPayload{Text: "running tests", Short: "tests"})
			},
			assert: func(t *testing.T, ev any) {
				m, ok := ev.(MsgNotification)
				require.True(t, ok, "want MsgNotification, got %T", ev)
				assert.Equal(t, "running tests", m.Text)
				assert.Equal(t, "tests", m.Short)
				// A persistent update_status caption is NOT ephemeral by default.
				assert.False(t, m.Ephemeral)
			},
		},
		{
			name: "notification (ephemeral): propagates the Ephemeral flag",
			env: func(t *testing.T) channelevents.Envelope {
				return mustEnv(t, channelevents.KindNotification,
					channelevents.NotificationPayload{Text: "Picking up 1 message you sent.", Ephemeral: true})
			},
			assert: func(t *testing.T, ev any) {
				m, ok := ev.(MsgNotification)
				require.True(t, ok, "want MsgNotification, got %T", ev)
				assert.Equal(t, "Picking up 1 message you sent.", m.Text)
				// A one-shot l.Notify announcement must be distinguishable from a
				// persistent update_status caption, or a surface cannot decay it.
				assert.True(t, m.Ephemeral)
			},
		},
		{
			name: "plan_update: emits MsgPlanUpdate",
			env: func(t *testing.T) channelevents.Envelope {
				return mustEnv(t, channelevents.KindPlanUpdate,
					channelevents.PlanUpdatePayload{
						PlanName: "main",
						Items:    []channelevents.PlanItemRef{{ID: "i1", Label: "step one", Status: "done"}},
					})
			},
			assert: func(t *testing.T, ev any) {
				m, ok := ev.(MsgPlanUpdate)
				require.True(t, ok, "want MsgPlanUpdate, got %T", ev)
				assert.Equal(t, "main", m.Payload.PlanName)
				require.Len(t, m.Payload.Items, 1)
				assert.Equal(t, "step one", m.Payload.Items[0].Label)
			},
		},
		{
			name: "turn_progress: emits MsgTurnProgress carrying the token/elapsed snapshot",
			env: func(t *testing.T) channelevents.Envelope {
				return mustEnv(t, channelevents.KindTurnProgress,
					channelevents.TurnProgressPayload{
						InputTokens: 1200, OutputTokens: 6400, ElapsedSeconds: 34, Seq: 3,
					})
			},
			assert: func(t *testing.T, ev any) {
				m, ok := ev.(MsgTurnProgress)
				require.True(t, ok, "want MsgTurnProgress, got %T", ev)
				assert.Equal(t, int64(1200), m.Payload.InputTokens)
				assert.Equal(t, int64(6400), m.Payload.OutputTokens)
				assert.Equal(t, 34, m.Payload.ElapsedSeconds)
			},
		},
		{
			name: "operation_activity: emits MsgOperationActivity",
			env: func(t *testing.T) channelevents.Envelope {
				return mustEnv(t, channelevents.KindOperationActivity,
					channelevents.OperationActivityPayload{
						Operations: []channelevents.OperationActivityNode{
							{ID: "op-1", Description: "fetch goals", Active: true},
						},
						CompactLine: "fetch goals ‣ querying Linear",
					})
			},
			assert: func(t *testing.T, ev any) {
				m, ok := ev.(MsgOperationActivity)
				require.True(t, ok, "want MsgOperationActivity, got %T", ev)
				assert.Equal(t, "fetch goals ‣ querying Linear", m.Payload.CompactLine)
				require.Len(t, m.Payload.Operations, 1)
				assert.Equal(t, "op-1", m.Payload.Operations[0].ID)
				assert.Equal(t, "s1", m.Session.Name)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := &RecordingSink{}
			s := &localSender{sink: newInertSink(sink)}
			res, err := s.Send(context.Background(), sessInfo(), tc.env(t))
			require.NoError(t, err)
			assert.Equal(t, channelkinds.SubChannelSendResult{}, res)
			events := sink.Events()
			require.Len(t, events, 1)
			tc.assert(t, events[0])
		})
	}
}

func TestLocalSender_MalformedPayload_EmitsSendErrorAndReturnsErr(t *testing.T) {
	sink := &RecordingSink{}
	s := &localSender{sink: newInertSink(sink)}
	bad := channelevents.Envelope{
		Version:     1,
		Kind:        channelevents.KindUserMessage,
		Session:     channelevents.SessionRef{Namespace: "default", Name: "s1"},
		Payload:     json.RawMessage(`{"text": 12345}`), // text must be a string
		PublishedAt: timeNowUTC(),
	}
	_, err := s.Send(context.Background(), sessInfo(), bad)
	require.Error(t, err, "malformed payload must return an error to the caller")
	events := sink.Events()
	require.Len(t, events, 1)
	se, ok := events[0].(MsgSendError)
	require.True(t, ok, "want MsgSendError, got %T", events[0])
	assert.Equal(t, channelevents.KindUserMessage, se.Kind)
}

func TestLocalSender_UnsupportedKind_EmitsSendError(t *testing.T) {
	sink := &RecordingSink{}
	s := &localSender{sink: newInertSink(sink)}
	env := mustEnv(t, channelevents.KindToolActivity, channelevents.ToolActivityPayload{Tool: "x"})
	_, err := s.Send(context.Background(), sessInfo(), env)
	require.Error(t, err)
	require.Len(t, sink.Events(), 1)
	assert.IsType(t, MsgSendError{}, sink.Events()[0])
}

// TestLocalSender_TurnProgress_IsNotASendError pins the user-visible symptom
// directly: the runner's progressReporter publishes a throttled turn snapshot
// roughly every 5 seconds for the whole of an active turn, so a main sender that
// rejects KindTurnProgress as unsupported paints a warning line into the chat
// timeline on every tick. Asserted as "no MsgSendError, no returned error" —
// the render event it emits instead is asserted in TestLocalSender_Send.
func TestLocalSender_TurnProgress_IsNotASendError(t *testing.T) {
	sink := &RecordingSink{}
	s := &localSender{sink: newInertSink(sink)}
	env := mustEnv(t, channelevents.KindTurnProgress,
		channelevents.TurnProgressPayload{
			InputTokens: 1200, OutputTokens: 6400, ElapsedSeconds: 34, Seq: 3,
		})

	_, err := s.Send(context.Background(), sessInfo(), env)
	require.NoError(t, err, "a turn-progress tick is routine traffic, not a sender failure")

	events := sink.Events()
	require.Len(t, events, 1)
	_, isSendErr := events[0].(MsgSendError)
	assert.False(t, isSendErr,
		"turn progress must not surface as a timeline warning on every ~5s tick; got %T", events[0])
}

func TestLocalSender_ToolProgress_EmitsRenderEvent(t *testing.T) {
	sink := &RecordingSink{}
	s := &localSender{sink: newInertSink(sink)}
	env := mustEnv(t, channelevents.KindToolProgress,
		channelevents.ToolProgressPayload{CallID: "tc1", Name: "git clone", ElapsedSeconds: 14})
	_, err := s.Send(context.Background(), sessInfo(), env)
	require.NoError(t, err)
	events := sink.Events()
	require.Len(t, events, 1)
	m, ok := events[0].(MsgToolProgress)
	require.True(t, ok, "want MsgToolProgress, got %T", events[0])
	assert.Equal(t, "git clone", m.Name)
	assert.Equal(t, 14, m.ElapsedSeconds)
	assert.False(t, m.Done)
	assert.Equal(t, "s1", m.Session.Name)
}

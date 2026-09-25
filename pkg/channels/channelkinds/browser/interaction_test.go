package browser

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// TestInteractionSender_Request_EmitsMsgInteractionRequest verifies an
// interaction_request envelope renders as MsgInteractionRequest, carrying the
// payload through untouched (translation only, per sibling senders).
func TestInteractionSender_Request_EmitsMsgInteractionRequest(t *testing.T) {
	sink := &RecordingSink{}
	s := &interactionSender{sink: sink}

	reqPl := channelevents.InteractionRequestPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: "default", Name: "s1"},
		Category:        "credential_link",
		RequestRef:      "req-1",
		Lead:            "Connect your account",
		Actions: []channelevents.InteractionAction{
			{ID: "open", Label: "Connect", Kind: channelevents.ActionKindLink, URL: "https://link.example.com/x"},
		},
		Audience: channelevents.InteractionAudience{
			Scope:     channelevents.AudienceRequester,
			Requester: &channelevents.ExternalIdentity{Kind: "idp", ExternalID: "alice@example.com", Email: "alice@example.com"},
		},
	}
	env := mustEnv(t, channelevents.KindInteractionRequest, reqPl)

	res, err := s.Send(context.Background(), sessInfo(), env)
	require.NoError(t, err)
	assert.Equal(t, channelkinds.SubChannelSendResult{}, res)

	events := sink.Events()
	require.Len(t, events, 1)
	m, ok := events[0].(MsgInteractionRequest)
	require.True(t, ok, "want MsgInteractionRequest, got %T", events[0])
	assert.Equal(t, "s1", m.Session.Name)
	assert.Equal(t, "credential_link", m.Payload.Category)
	assert.Equal(t, "req-1", m.Payload.RequestRef)
	assert.Equal(t, "Connect your account", m.Payload.Lead)
}

// TestInteractionSender_Applied_EmitsMsgInteractionApplied verifies an
// interaction_applied envelope renders as MsgInteractionApplied.
func TestInteractionSender_Applied_EmitsMsgInteractionApplied(t *testing.T) {
	sink := &RecordingSink{}
	s := &interactionSender{sink: sink}

	appPl := channelevents.InteractionAppliedPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: "default", Name: "s1"},
		Category:        "credential_link",
		RequestRef:      "req-1",
		Outcome:         channelevents.OutcomeResolved,
	}
	env := mustEnv(t, channelevents.KindInteractionApplied, appPl)

	res, err := s.Send(context.Background(), sessInfo(), env)
	require.NoError(t, err)
	assert.Equal(t, channelkinds.SubChannelSendResult{}, res)

	events := sink.Events()
	require.Len(t, events, 1)
	m, ok := events[0].(MsgInteractionApplied)
	require.True(t, ok, "want MsgInteractionApplied, got %T", events[0])
	assert.Equal(t, "s1", m.Session.Name)
	assert.Equal(t, "credential_link", m.Payload.Category)
	assert.Equal(t, channelevents.OutcomeResolved, m.Payload.Outcome)
}

// TestInteractionSender_Rejected_EmitsMsgInteractionRejected verifies the
// per-clicker rejection envelope renders as MsgInteractionRejected. The
// already_resolved class is the one that must carry OriginalOutcome through, so
// a spectator whose click lost the race can be told what the prompt resolved to.
func TestInteractionSender_Rejected_EmitsMsgInteractionRejected(t *testing.T) {
	sink := &RecordingSink{}
	s := &interactionSender{sink: sink}

	env := mustEnv(t, channelevents.KindInteractionDecisionRejected,
		channelevents.InteractionDecisionRejectedPayload{
			AgentSessionRef: channelevents.SessionRef{Namespace: "default", Name: "s1"},
			Category:        "tool_approval",
			RequestRef:      "req-1",
			Class:           "already_resolved",
			Reason:          "someone else already answered this",
			OriginalOutcome: channelevents.OutcomeResolved,
		})

	res, err := s.Send(context.Background(), sessInfo(), env)
	require.NoError(t, err)
	assert.Equal(t, channelkinds.SubChannelSendResult{}, res)

	events := sink.Events()
	require.Len(t, events, 1)
	m, ok := events[0].(MsgInteractionRejected)
	require.True(t, ok, "want MsgInteractionRejected, got %T", events[0])
	assert.Equal(t, "s1", m.Session.Name)
	assert.Equal(t, "already_resolved", m.Payload.Class)
	assert.Equal(t, "someone else already answered this", m.Payload.Reason)
	assert.Equal(t, channelevents.OutcomeResolved, m.Payload.OriginalOutcome)
}

// TestInteractionSender_MalformedPayload_EmitsSendErrorAndReturnsErr mirrors
// the sibling senders' fail-loud-on-decode-error contract (e.g.
// TestBrowserSender_MalformedPayload_EmitsSendErrorAndReturnsErr).
func TestInteractionSender_MalformedPayload_EmitsSendErrorAndReturnsErr(t *testing.T) {
	sink := &RecordingSink{}
	s := &interactionSender{sink: sink}
	bad := channelevents.Envelope{
		Version:     1,
		Kind:        channelevents.KindInteractionRequest,
		Session:     channelevents.SessionRef{Namespace: "default", Name: "s1"},
		Payload:     []byte(`{"category": 12345}`), // category must be a string
		PublishedAt: timeNowUTC(),
	}
	_, err := s.Send(context.Background(), sessInfo(), bad)
	require.Error(t, err, "malformed payload must return an error to the caller")

	events := sink.Events()
	require.Len(t, events, 1)
	se, ok := events[0].(MsgSendError)
	require.True(t, ok, "want MsgSendError, got %T", events[0])
	assert.Equal(t, channelevents.KindInteractionRequest, se.Kind)
}

// TestInteractionSender_UnsupportedKind_EmitsSendError verifies an unknown
// envelope kind on the interaction sender fails loud: a visible MsgSendError
// plus a returned error (never a silent drop, per AGENTS.md).
func TestInteractionSender_UnsupportedKind_EmitsSendError(t *testing.T) {
	sink := &RecordingSink{}
	s := &interactionSender{sink: sink}
	env := mustEnv(t, channelevents.KindNotification, channelevents.NotificationPayload{Text: "x"})

	_, err := s.Send(context.Background(), sessInfo(), env)
	require.Error(t, err, "unknown kind on the interaction sender must fail loud")

	events := sink.Events()
	require.Len(t, events, 1)
	assert.IsType(t, MsgSendError{}, events[0])
}

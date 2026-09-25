package local

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

func TestInteractionSender_Send(t *testing.T) {
	cases := []struct {
		name    string
		env     func(t *testing.T) channelevents.Envelope
		check   func(t *testing.T, ev any)
		wantErr bool
	}{
		{
			name: "interaction_request: credential_link renders text with Lead and link URL",
			env: func(t *testing.T) channelevents.Envelope {
				expiresAt := time.Now().Add(time.Minute)
				return mustEnv(t, channelevents.KindInteractionRequest,
					channelevents.InteractionRequestPayload{
						AgentSessionRef: channelevents.SessionRef{Namespace: "default", Name: "s1"},
						Category:        "credential_link",
						RequestRef:      "cred-1",
						Lead:            "Link your GitHub account",
						Body:            "Click the link to authenticate",
						Actions: []channelevents.InteractionAction{
							{
								ID:    "link-1",
								Label: "Authenticate on GitHub",
								Kind:  channelevents.ActionKindLink,
								URL:   "https://github.com/login",
							},
						},
						Audience: channelevents.InteractionAudience{
							Scope:     channelevents.AudienceRequester,
							Requester: &channelevents.ExternalIdentity{Kind: "user", ExternalID: "user-1"},
						},
						ExpiresAt: &expiresAt,
					})
			},
			check: func(t *testing.T, ev any) {
				m, ok := ev.(MsgInteractionRequest)
				require.True(t, ok, "want MsgInteractionRequest, got %T", ev)
				assert.Equal(t, "s1", m.Session.Name)
				assert.NotEmpty(t, m.Text, "rendered text must be non-empty")
				assert.Contains(t, m.Text, "Link your GitHub account", "text must contain Lead")
				assert.Contains(t, m.Text, "https://github.com/login", "text must contain link URL")
			},
		},
		{
			name: "interaction_request: identity_choice renders its 3-way decision actions as readable choices",
			env: func(t *testing.T) channelevents.Envelope {
				return mustEnv(t, channelevents.KindInteractionRequest,
					channelevents.InteractionRequestPayload{
						AgentSessionRef: channelevents.SessionRef{Namespace: "default", Name: "s1"},
						Category:        "identity_choice",
						RequestRef:      "idc-1",
						Lead:            "Which identity should this agent use for this session?",
						Actions: []channelevents.InteractionAction{
							{ID: "agent", Kind: channelevents.ActionKindDecision, Label: "Run as the agent"},
							{ID: "userPassthrough", Kind: channelevents.ActionKindDecision, Label: "Run as me"},
							{ID: "cancel", Kind: channelevents.ActionKindDecision, Label: "Cancel"},
						},
						Audience: channelevents.InteractionAudience{
							Scope:     channelevents.AudienceRequester,
							Requester: &channelevents.ExternalIdentity{Kind: "user", ExternalID: "user-1"},
						},
					})
			},
			check: func(t *testing.T, ev any) {
				m, ok := ev.(MsgInteractionRequest)
				require.True(t, ok, "want MsgInteractionRequest, got %T", ev)
				assert.Contains(t, m.Text, "Which identity should this agent use for this session?", "text must contain Lead")
				// All 3 decision-action labels must be individually enumerated —
				// the local/TUI floor has no clickable buttons (T11 scopes local
				// decision INPUT as a follow-up), so this is the only surface a
				// local user has for reading the choices.
				assert.Contains(t, m.Text, "Run as the agent", "agent choice must be enumerated")
				assert.Contains(t, m.Text, "Run as me", "userPassthrough choice must be enumerated")
				assert.Contains(t, m.Text, "Cancel", "cancel choice must be enumerated")
				assert.Contains(t, m.Text, "respond from a connected app",
					"decision actions with no local input must point the user at a connected surface")
			},
		},
		{
			name: "interaction_applied: emits MsgInteractionApplied with outcome",
			env: func(t *testing.T) channelevents.Envelope {
				return mustEnv(t, channelevents.KindInteractionApplied,
					channelevents.InteractionAppliedPayload{
						AgentSessionRef: channelevents.SessionRef{Namespace: "default", Name: "s1"},
						Category:        "credential_link",
						RequestRef:      "cred-1",
						Outcome:         channelevents.OutcomeResolved,
						OutcomeText:     "GitHub account linked successfully",
					})
			},
			check: func(t *testing.T, ev any) {
				m, ok := ev.(MsgInteractionApplied)
				require.True(t, ok, "want MsgInteractionApplied, got %T", ev)
				assert.Equal(t, "s1", m.Session.Name)
				assert.Equal(t, channelevents.OutcomeResolved, m.Outcome)
				assert.Equal(t, "GitHub account linked successfully", m.OutcomeText)
			},
		},
		{
			name: "interaction_decision_rejected: emits MsgInteractionRejected carrying the original outcome",
			env: func(t *testing.T) channelevents.Envelope {
				return mustEnv(t, channelevents.KindInteractionDecisionRejected,
					channelevents.InteractionDecisionRejectedPayload{
						AgentSessionRef: channelevents.SessionRef{Namespace: "default", Name: "s1"},
						Category:        "tool_approval",
						RequestRef:      "req-1",
						Class:           "already_resolved",
						Reason:          "someone else already answered this",
						OriginalOutcome: channelevents.OutcomeResolved,
					})
			},
			check: func(t *testing.T, ev any) {
				m, ok := ev.(MsgInteractionRejected)
				require.True(t, ok, "want MsgInteractionRejected, got %T", ev)
				assert.Equal(t, "s1", m.Session.Name)
				assert.Equal(t, "already_resolved", m.Class)
				assert.Equal(t, "someone else already answered this", m.Reason)
				assert.Equal(t, channelevents.OutcomeResolved, m.OriginalOutcome)
			},
		},
		{
			name: "unknown kind: errors",
			env: func(t *testing.T) channelevents.Envelope {
				return mustEnv(t, channelevents.KindUserMessage,
					channelevents.OutboundUserMessagePayload{Text: "hello"})
			},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := &RecordingSink{}
			s := &interactionSender{sink: newInertSink(sink)}
			_, err := s.Send(context.Background(), sessInfo(), tc.env(t))

			if tc.wantErr {
				assert.Error(t, err)
				// Error path should emit MsgSendError
				require.Len(t, sink.Events(), 1)
				assert.IsType(t, MsgSendError{}, sink.Events()[0])
				return
			}

			require.NoError(t, err)
			require.Len(t, sink.Events(), 1)
			tc.check(t, sink.Events()[0])
		})
	}
}

// TestInteractionSender_MalformedPayload_EmitsSendErrorAndReturnsErr mirrors
// the sibling senders' fail-loud-on-decode-error contract (e.g.
// TestLocalSender_MalformedPayload_EmitsSendErrorAndReturnsErr). A decode
// failure on a well-known kind must NOT be mistaken for the unknown-kind path:
// both fail loud, but only this one proves the decode guard exists.
func TestInteractionSender_MalformedPayload_EmitsSendErrorAndReturnsErr(t *testing.T) {
	sink := &RecordingSink{}
	s := &interactionSender{sink: newInertSink(sink)}
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

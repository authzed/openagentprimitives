package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// The NATS SUBJECT is the routing authority for every inbound subscription,
// not Envelope.Session.
//
// Both wrappers subscribe cluster-wide ("ap.session.*.*.in.<kind>"), while a
// runner's per-session NATS JWT permits publishing under exactly one
// "ap.session.<ns>.<own-name>.>" tree — which covers ".in." as well as
// ".out.". So the subject is the only session identity NATS authorized, and
// Envelope.Session is publisher-controlled JSON. These tests ARE the attack:
// an envelope published on session A's inbound subject that claims session B
// must never reach a handler acting on B.
//
// interaction_decision is called out explicitly because it is the path that
// resolves a parked approval.

// forgedDecision is an interaction_decision envelope claiming `claimNS/claimName`.
func forgedDecision(t *testing.T, claimNS, claimName string) []byte {
	t.Helper()
	env, err := channelevents.BuildEnvelope(claimNS, claimName, channelevents.KindInteractionDecision,
		channelevents.InteractionDecisionPayload{
			AgentSessionRef: channelevents.SessionRef{Namespace: claimNS, Name: claimName},
			Category:        "tool_approval",
			RequestRef:      "req-1",
			ActionID:        "approve",
			Decider: channelevents.ExternalIdentity{
				Kind: "slack", TeamScope: "T1", ExternalID: "U1", Email: "decider@example.test",
			},
		})
	require.NoError(t, err)
	b, err := json.Marshal(env)
	require.NoError(t, err)
	return b
}

func TestEnvelopeHandlerRoutesOffTheSubject(t *testing.T) {
	const victimNS, victimName = "team-a", "sess-victim"
	const attackerNS, attackerName = "team-a", "sess-attacker"

	cases := []struct {
		name        string
		subject     string
		data        []byte
		wantHandled bool
	}{
		{
			name:        "forged interaction_decision (published on the attacker's subject, claims the victim): dropped, handler never runs",
			subject:     channelevents.SubjectIn(channelevents.SubjectPrefix(attackerNS, attackerName), channelevents.KindInteractionDecision),
			data:        forgedDecision(t, victimNS, victimName),
			wantHandled: false,
		},
		{
			name:        "cross-namespace forgery (same session name, other namespace): dropped",
			subject:     channelevents.SubjectIn(channelevents.SubjectPrefix("team-b", victimName), channelevents.KindInteractionDecision),
			data:        forgedDecision(t, victimNS, victimName),
			wantHandled: false,
		},
		{
			name:        "honest publish (subject and envelope agree): dispatched",
			subject:     channelevents.SubjectIn(channelevents.SubjectPrefix(victimNS, victimName), channelevents.KindInteractionDecision),
			data:        forgedDecision(t, victimNS, victimName),
			wantHandled: true,
		},
		{
			name:        "unparseable subject: dropped rather than trusted to the envelope",
			subject:     "nonsense",
			data:        forgedDecision(t, victimNS, victimName),
			wantHandled: false,
		},
		{
			name:        "empty subject (no bus identity at all): dropped",
			subject:     "",
			data:        forgedDecision(t, victimNS, victimName),
			wantHandled: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var handled []channelevents.SessionRef
			h := envelopeHandler(testLogger(t), "interaction_decision", "HandleInteractionDecision",
				func(_ context.Context, env channelevents.Envelope) error {
					handled = append(handled, env.Session)
					return nil
				})

			h(&nats.Msg{Subject: tc.subject, Data: tc.data})

			if tc.wantHandled {
				require.Len(t, handled, 1, "an honest publish must still be dispatched")
				assert.Equal(t, victimNS, handled[0].Namespace)
				assert.Equal(t, victimName, handled[0].Name)
				return
			}
			assert.Empty(t, handled,
				"the decision handler must NEVER act on a session the publisher was not authorized to publish for")
		})
	}
}

// respondingHandler carries the same rule — and must still ALWAYS reply, so a
// rejected request surfaces a reason instead of a spinner (AGENTS.md: no
// silent errors).
func TestRespondingHandlerRoutesOffTheSubject(t *testing.T) {
	cases := []struct {
		name        string
		subject     string
		wantHandled bool
		wantError   string
	}{
		{
			name:        "forged view_message claiming another session: not delivered, replies with the reason",
			subject:     channelevents.SubjectIn(channelevents.SubjectPrefix("ns", "other"), channelevents.KindViewMessage),
			wantHandled: false,
			wantError:   "session does not match",
		},
		{
			name:        "unparseable subject: not delivered, replies with the reason",
			subject:     "ap.session.ns.s.out.view_message", // an OUT subject is not an inbound identity
			wantHandled: false,
			wantError:   "subject",
		},
		{
			name:        "honest publish (subject and envelope agree): delivered",
			subject:     channelevents.SubjectIn(channelevents.SubjectPrefix("ns", "s"), channelevents.KindViewMessage),
			wantHandled: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var handled bool
			var replied []byte
			h := respondingHandler(testLogger(t), "view_message", "HandleViewMessage",
				func(context.Context, channelevents.Envelope) (channelevents.ViewMessageResultPayload, error) {
					handled = true
					return channelevents.ViewMessageResultPayload{Outcome: "routed"}, nil
				},
				func(_ *nats.Msg, b []byte) error { replied = b; return nil })

			// mustEnvelope builds a view_message for session ns/s.
			h(&nats.Msg{Subject: tc.subject, Data: mustEnvelope(t), Reply: "_INBOX.test"})

			require.NotNil(t, replied, "handler MUST reply on every path, including a refused one")
			var res channelevents.ViewMessageResultPayload
			require.NoError(t, json.Unmarshal(replied, &res))

			assert.Equal(t, tc.wantHandled, handled,
				"delivery must follow the subject-authorized session, never the envelope's claim")
			if tc.wantHandled {
				assert.Empty(t, res.Error)
				assert.Equal(t, "routed", res.Outcome)
				return
			}
			assert.Equal(t, "internal_error", res.Outcome)
			assert.Contains(t, res.Error, tc.wantError)
		})
	}
}

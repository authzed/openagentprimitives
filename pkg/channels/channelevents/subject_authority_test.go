package channelevents_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// The cross-check is the gate every wildcard consumer of a session subject runs
// before acting: the subject is the identity NATS authorized, the envelope's
// Session is publisher-controlled JSON. These cases ARE the attack — an envelope
// published on one session's subject claiming another.

func envFor(t *testing.T, ns, name string, k channelevents.Kind) channelevents.Envelope {
	t.Helper()
	env, err := channelevents.BuildEnvelope(ns, name, k,
		channelevents.NotificationPayload{Text: "x"})
	require.NoError(t, err)
	return env
}

func TestAuthorizeInSubject(t *testing.T) {
	const victimNS, victimName = "team-a", "sess-victim"
	inSubject := func(ns, name string) string {
		return channelevents.SubjectIn(channelevents.SubjectPrefix(ns, name),
			channelevents.KindInteractionDecision)
	}

	cases := []struct {
		name    string
		subject string
		wantErr error
	}{
		{
			name:    "subject and envelope agree: authorized, returns the pair",
			subject: inSubject(victimNS, victimName),
		},
		{
			name:    "attacker's own subject claiming the victim: ErrSessionMismatch",
			subject: inSubject(victimNS, "sess-attacker"),
			wantErr: channelevents.ErrSessionMismatch,
		},
		{
			name:    "same session name in another namespace: ErrSessionMismatch",
			subject: inSubject("team-b", victimName),
			wantErr: channelevents.ErrSessionMismatch,
		},
		{
			name:    "an OUT subject is not an inbound identity: ErrUnroutableSubject",
			subject: channelevents.SubjectOut(channelevents.SubjectPrefix(victimNS, victimName), channelevents.KindInteractionDecision),
			wantErr: channelevents.ErrUnroutableSubject,
		},
		{
			name:    "unparseable subject: ErrUnroutableSubject",
			subject: "nonsense",
			wantErr: channelevents.ErrUnroutableSubject,
		},
		{
			name:    "empty subject (no bus identity at all): ErrUnroutableSubject",
			subject: "",
			wantErr: channelevents.ErrUnroutableSubject,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := envFor(t, victimNS, victimName, channelevents.KindInteractionDecision)
			ns, name, err := channelevents.AuthorizeInSubject(tc.subject, env)
			if tc.wantErr == nil {
				require.NoError(t, err)
				assert.Equal(t, victimNS, ns)
				assert.Equal(t, victimName, name)
				return
			}
			require.ErrorIs(t, err, tc.wantErr)
			assert.Empty(t, ns, "a refused subject must not hand back a session to act on")
			assert.Empty(t, name)
			assert.Contains(t, err.Error(), victimName,
				"the error must name the claimed session so a log locates the publisher")
		})
	}
}

func TestAuthorizeOutSubject(t *testing.T) {
	const ns, name = "team-a", "sess-1"
	outSubject := func(ns, name string) string {
		return channelevents.SubjectOut(channelevents.SubjectPrefix(ns, name),
			channelevents.KindNotification)
	}

	cases := []struct {
		name    string
		subject string
		wantErr error
	}{
		{
			name:    "subject and envelope agree: authorized",
			subject: outSubject(ns, name),
		},
		{
			name:    "envelope labels its event with another session: ErrSessionMismatch",
			subject: outSubject(ns, "sess-other"),
			wantErr: channelevents.ErrSessionMismatch,
		},
		{
			name:    "an IN subject is not an outbound identity: ErrUnroutableSubject",
			subject: channelevents.SubjectIn(channelevents.SubjectPrefix(ns, name), channelevents.KindNotification),
			wantErr: channelevents.ErrUnroutableSubject,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := envFor(t, ns, name, channelevents.KindNotification)
			gotNS, gotName, err := channelevents.AuthorizeOutSubject(tc.subject, env)
			if tc.wantErr == nil {
				require.NoError(t, err)
				assert.Equal(t, ns, gotNS)
				assert.Equal(t, name, gotName)
				return
			}
			require.ErrorIs(t, err, tc.wantErr)
			assert.Empty(t, gotNS)
			assert.Empty(t, gotName)
		})
	}
}

// A dotted Kind (assistant.stream.delta is the only one) must round-trip: the
// parsers accept six-or-MORE tokens, and the cross-check must not narrow that.
func TestAuthorizeOutSubject_DottedKindStillAuthorized(t *testing.T) {
	const ns, name = "team-a", "sess-1"
	env := envFor(t, ns, name, channelevents.KindAssistantStreamDelta)
	gotNS, gotName, err := channelevents.AuthorizeOutSubject(
		channelevents.SubjectOut(channelevents.SubjectPrefix(ns, name), channelevents.KindAssistantStreamDelta), env)
	require.NoError(t, err, "a dotted kind's subject must still authorize")
	assert.Equal(t, ns, gotNS)
	assert.Equal(t, name, gotName)
}

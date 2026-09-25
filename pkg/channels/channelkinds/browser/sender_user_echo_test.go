package browser

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// TestUserEchoSender_EmitsMsgUserEcho verifies a user_echo envelope renders a
// MsgUserEcho carrying the view-originated text, a human-readable author label,
// the surface Via, and the idempotency RequestID (the web chat uses the last to
// suppress the echo of a message it already showed optimistically).
func TestUserEchoSender_EmitsMsgUserEcho(t *testing.T) {
	sink := &RecordingSink{}
	s := &userEchoSender{sink: sink}
	env := mustEnv(t, channelevents.KindUserEcho, channelevents.UserEchoPayload{
		Text:      "please tighten the header spacing",
		Author:    channelevents.ExternalIdentity{Kind: "slack", Email: "reviewer@example.com", ExternalID: "U123"},
		Via:       "urn:ap:view:artifact:art-1/annotations",
		RequestID: "req-42",
	})

	_, err := s.Send(context.Background(), sessInfo(), env)
	require.NoError(t, err)
	require.Len(t, sink.Events(), 1)
	m, ok := sink.Events()[0].(MsgUserEcho)
	require.True(t, ok, "want MsgUserEcho, got %T", sink.Events()[0])
	assert.Equal(t, "please tighten the header spacing", m.Text)
	assert.Equal(t, "reviewer@example.com", m.Author, "email is the preferred author label")
	assert.Equal(t, "urn:ap:view:artifact:art-1/annotations", m.Via)
	assert.Equal(t, "req-42", m.RequestID)
	assert.Equal(t, "s1", m.Session.Name)
}

// TestUserEchoSender_AuthorFallback verifies the author label degrades from
// email → external id → a generic placeholder, never empty.
func TestUserEchoSender_AuthorFallback(t *testing.T) {
	cases := []struct {
		name   string
		author channelevents.ExternalIdentity
		want   string
	}{
		{name: "email preferred", author: channelevents.ExternalIdentity{Email: "a@example.com", ExternalID: "U1"}, want: "a@example.com"},
		{name: "external id when no email", author: channelevents.ExternalIdentity{ExternalID: "U1"}, want: "U1"},
		{name: "generic placeholder when empty", author: channelevents.ExternalIdentity{}, want: "someone"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := &RecordingSink{}
			s := &userEchoSender{sink: sink}
			env := mustEnv(t, channelevents.KindUserEcho, channelevents.UserEchoPayload{Text: "hi", Author: tc.author})

			_, err := s.Send(context.Background(), sessInfo(), env)
			require.NoError(t, err)
			require.Len(t, sink.Events(), 1)
			m := sink.Events()[0].(MsgUserEcho)
			assert.Equal(t, tc.want, m.Author)
		})
	}
}

// TestUserEchoSender_BadPayload_EmitsSendError verifies a malformed payload
// surfaces LOUDLY as a MsgSendError plus a returned error, never a silent drop.
func TestUserEchoSender_BadPayload_EmitsSendError(t *testing.T) {
	sink := &RecordingSink{}
	s := &userEchoSender{sink: sink}
	env := channelevents.Envelope{Version: 1, Kind: channelevents.KindUserEcho, Payload: []byte("{not json")}

	_, err := s.Send(context.Background(), sessInfo(), env)
	require.Error(t, err)
	require.Len(t, sink.Events(), 1)
	_, ok := sink.Events()[0].(MsgSendError)
	require.True(t, ok, "want MsgSendError, got %T", sink.Events()[0])
}

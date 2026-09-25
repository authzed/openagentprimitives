package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/go-logr/logr"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// respondingHandler must ALWAYS reply — on decode failure, on handler error,
// and on success. A handler that returns without responding leaves the browser
// spinning until channelkinds.ViewMessageTimeout. That is a silent error.
func TestRespondingHandlerAlwaysReplies(t *testing.T) {
	cases := []struct {
		name      string
		data      []byte
		fn        func(env channelevents.Envelope) (channelevents.ViewMessageResultPayload, error)
		wantError string
	}{
		{
			name:      "malformed envelope: replies with error, does not hang",
			data:      []byte(`{`),
			fn:        func(channelevents.Envelope) (channelevents.ViewMessageResultPayload, error) { panic("unreachable") },
			wantError: "decode envelope",
		},
		{
			name: "handler error: replies with the reason",
			data: mustEnvelope(t),
			fn: func(channelevents.Envelope) (channelevents.ViewMessageResultPayload, error) {
				return channelevents.ViewMessageResultPayload{Outcome: "internal_error", Error: "boom"}, errors.New("boom")
			},
			wantError: "boom",
		},
		{
			name: "success: replies with the decision, no error",
			data: mustEnvelope(t),
			fn: func(channelevents.Envelope) (channelevents.ViewMessageResultPayload, error) {
				return channelevents.ViewMessageResultPayload{Outcome: "routed"}, nil
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var replied []byte
			m := &nats.Msg{Subject: viewMessageSubject, Data: tc.data, Reply: "_INBOX.test"}
			h := respondingHandler(testLogger(t), "view_message", "HandleViewMessage",
				func(_ context.Context, env channelevents.Envelope) (channelevents.ViewMessageResultPayload, error) {
					return tc.fn(env)
				},
				func(_ *nats.Msg, b []byte) error { replied = b; return nil })
			h(m)

			require.NotNil(t, replied, "handler MUST reply; silence makes the caller wait out its timeout")
			var res channelevents.ViewMessageResultPayload
			require.NoError(t, json.Unmarshal(replied, &res))
			if tc.wantError != "" {
				assert.Contains(t, res.Error, tc.wantError)
			} else {
				assert.Empty(t, res.Error)
				assert.Equal(t, "routed", res.Outcome)
			}
		})
	}
}

// TestRespondingHandlerRepliesEvenWhenRespondFails guards the "last resort"
// branch: even if the injected respond func itself errors (e.g. the NATS
// connection dropped), the handler must not panic and must still have
// attempted exactly one reply call with a well-formed payload.
func TestRespondingHandlerRepliesEvenWhenRespondFails(t *testing.T) {
	var attempted []byte
	m := &nats.Msg{Subject: viewMessageSubject, Data: mustEnvelope(t), Reply: "_INBOX.test"}
	h := respondingHandler(testLogger(t), "view_message", "HandleViewMessage",
		func(context.Context, channelevents.Envelope) (channelevents.ViewMessageResultPayload, error) {
			return channelevents.ViewMessageResultPayload{Outcome: "routed"}, nil
		},
		func(_ *nats.Msg, b []byte) error { attempted = b; return errors.New("conn closed") })

	require.NotPanics(t, func() { h(m) })
	require.NotNil(t, attempted, "respond must still be attempted even though it will fail")
}

// viewMessageSubject is the authorized inbound subject for the session
// mustEnvelope builds for. These tests are about the always-reply contract, not
// about subject authority (TestRespondingHandlerRoutesOffTheSubject owns that),
// so they publish honestly — a msg with no Subject would now be refused before
// fn ever runs and would exercise the wrong branch.
var viewMessageSubject = channelevents.SubjectIn(
	channelevents.SubjectPrefix("ns", "s"), channelevents.KindViewMessage)

func mustEnvelope(t *testing.T) []byte {
	t.Helper()
	env, err := channelevents.BuildEnvelope("ns", "s", channelevents.KindViewMessage,
		channelevents.ViewMessagePayload{Text: "hi"})
	require.NoError(t, err)
	b, err := json.Marshal(env)
	require.NoError(t, err)
	return b
}

func testLogger(t *testing.T) logr.Logger {
	t.Helper()
	return logr.Discard()
}

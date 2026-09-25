package local

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// fakeAgentUIMinter is a test-only channelkinds.AgentUIMinter that returns a
// canned URL or error.
type fakeAgentUIMinter struct {
	url string
	err error
}

func (f *fakeAgentUIMinter) MintAgentUILink(_ string) (string, error) {
	return f.url, f.err
}

// TestAgentUIOfferSender_Send is table-driven over the sender's five gates —
// parse payload, missing sessionRef, nil minter, mint error, empty minted
// URL — plus the happy path. The four failure rows each assert their two
// halves (the returned error, and the emitted MsgSendError) with assert, not
// require, so that a sender which satisfies only one half still fails the
// test with BOTH failure messages visible, rather than the second half going
// unobserved because the first aborted the row.
func TestAgentUIOfferSender_Send(t *testing.T) {
	const mintedURL = "https://webd.example.com/sessions?session=default%2Fsess-1"

	cases := []struct {
		name    string
		minter  channelkinds.AgentUIMinter // zero value (nil field) is a TRUE nil interface, not a typed-nil pointer
		payload channelevents.AgentUIOfferPayload
		// wantErrContains is empty only for the happy-path row.
		wantErrContains string
	}{
		{
			name:    "a wired minter emits the link note",
			minter:  &fakeAgentUIMinter{url: mintedURL},
			payload: channelevents.AgentUIOfferPayload{SessionRef: "default/sess-1"},
		},
		{
			name:            "a payload with no sessionRef fails loudly",
			minter:          &fakeAgentUIMinter{url: mintedURL},
			payload:         channelevents.AgentUIOfferPayload{SessionRef: ""},
			wantErrContains: "sessionRef",
		},
		{
			// minter left unset: a true nil channelkinds.AgentUIMinter interface
			// value, per AGENTS.md's "nil interfaces" incident — a typed-nil
			// *fakeAgentUIMinter would satisfy != nil and panic on first call
			// instead of exercising this gate.
			name:            "a nil minter fails loudly",
			payload:         channelevents.AgentUIOfferPayload{SessionRef: "default/sess-1"},
			wantErrContains: "not configured",
		},
		{
			name:            "a mint error fails loudly",
			minter:          &fakeAgentUIMinter{err: fmt.Errorf("malformed sessionRef")},
			payload:         channelevents.AgentUIOfferPayload{SessionRef: "default/sess-1"},
			wantErrContains: "malformed sessionRef",
		},
		{
			name:            "an empty minted URL fails loudly",
			minter:          &fakeAgentUIMinter{url: ""},
			payload:         channelevents.AgentUIOfferPayload{SessionRef: "default/sess-1"},
			wantErrContains: "external URL",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := &RecordingSink{}
			s := &agentUIOfferSender{sink: newInertSink(sink), minter: tc.minter}

			env := mustEnv(t, channelevents.KindAgentUIOffer, tc.payload)
			_, err := s.Send(context.Background(), sessInfo(), env)
			events := sink.Events()

			if tc.wantErrContains == "" {
				// Happy path: not subject to the independent-halves discipline
				// below (there is only one correct outcome, not two failure modes
				// to distinguish), so require is fine here.
				require.NoError(t, err)
				require.Len(t, events, 1, "sender must emit exactly one render event")
				m, ok := events[0].(MsgAgentUIOffer)
				require.True(t, ok, "want MsgAgentUIOffer, got %T", events[0])
				assert.Equal(t, mintedURL, m.URL)
				assert.Equal(t, "s1", m.Session.Name)
				return
			}

			// Independent fact 1: an error was returned to the caller. A sender
			// that shows the warning and then returns nil would fail ONLY this
			// line, leaving the relay's own log empty.
			assert.Error(t, err, "a failure path must return an error, not skip silently")
			// Independent fact 2: the sender emitted exactly one visible
			// MsgSendError. A sender that returns the error and emits nothing
			// would fail ONLY this line — the silent-to-the-user failure this
			// sender's loudness exists to prevent.
			assert.Len(t, events, 1, "sender must still emit a visible MsgSendError on a failure path")
			if len(events) == 1 {
				se, ok := events[0].(MsgSendError)
				assert.True(t, ok, "want MsgSendError, got %T", events[0])
				if ok {
					assert.Contains(t, se.Err, tc.wantErrContains, "user-visible reason must name the failure")
				}
			}
		})
	}
}

// TestSubChannelSenderFor_AgentUIOffer returns a non-nil sender for this
// host's own session and nil for a foreign one — the host is scoped to the
// one session `oap` is chatting with, and resolving a sender for any other
// renders another conversation into this terminal.
func TestSubChannelSenderFor_AgentUIOffer(t *testing.T) {
	h := newTestHost(t, &RecordingSink{})

	own, err := h.SubChannelSenderFor(context.Background(), ownSession(), channelkinds.SubChannelAgentUIOffer)
	require.NoError(t, err)
	assert.NotNil(t, own, "the host's own session must resolve to a sender")

	foreign, err := h.SubChannelSenderFor(context.Background(), foreignSession(), channelkinds.SubChannelAgentUIOffer)
	require.NoError(t, err)
	assert.Nil(t, foreign, "another session's agent_ui_offer must not resolve to this host's sender")
}

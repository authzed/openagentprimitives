package channelevents

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKindAgentMessageSend_ValidAndImplemented(t *testing.T) {
	assert.True(t, KindAgentMessageSend.Valid())
	assert.True(t, KindAgentMessageSend.Implemented())
	assert.Equal(t, Kind("agent_message_send"), KindAgentMessageSend)
}

// The retired `agent_message` kind must stay retired. It was the direction
// whose subject named the RECEIVER, leaving the sender as an unauthenticated
// payload claim; both producers now publish agent_message_send on the sender's
// own subject instead. A publisher still emitting the old name must be
// refused at the publish boundary rather than quietly ignored.
func TestRetiredAgentMessageKind_IsNeitherValidNorImplemented(t *testing.T) {
	retired := Kind("agent_message")
	assert.False(t, retired.Valid(), "the retired receiver-addressed kind must not be publishable")
	assert.False(t, retired.Implemented(), "nothing in tree consumes the retired kind")
}

func TestAgentMessageSendPayload_JSONRoundtrip(t *testing.T) {
	in := AgentMessageSendPayload{
		To:   SessionRef{Namespace: "demo-ns", Name: "lead-1"},
		Text: "please review the draft",
	}
	raw, err := json.Marshal(in)
	require.NoError(t, err)

	var out AgentMessageSendPayload
	require.NoError(t, json.Unmarshal(raw, &out))
	assert.Equal(t, in, out)
}

// The payload must carry no sender field. The sender is the session the NATS
// subject authorized, and a `from` on the wire would be a second, weaker
// answer to the same question — the exact shape this direction replaced.
func TestAgentMessageSendPayload_CarriesNoSenderField(t *testing.T) {
	raw, err := json.Marshal(AgentMessageSendPayload{
		To:   SessionRef{Namespace: "demo-ns", Name: "lead-1"},
		Text: "hi",
	})
	require.NoError(t, err)

	var generic map[string]any
	require.NoError(t, json.Unmarshal(raw, &generic))
	assert.NotContains(t, generic, "from",
		"the sender is authenticated by the subject; it must never become a payload claim")
	assert.ElementsMatch(t, []string{"to", "text"}, keysOf(generic))
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

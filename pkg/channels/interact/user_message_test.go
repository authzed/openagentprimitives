package interact

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

func TestUserMessageSubmit_RoutesViaNATSWithVia(t *testing.T) {
	var gotEnv channelevents.Envelope
	deps := Deps{NATSRequest: func(_ string, data []byte, _ time.Duration) ([]byte, error) {
		require.NoError(t, json.Unmarshal(data, &gotEnv))
		return json.Marshal(channelevents.ViewMessageResultPayload{Outcome: "routed"})
	}}
	k, ok := Get("user_message")
	require.True(t, ok, "user_message must be registered")
	assert.Equal(t, "interact", k.Permission())

	res, err := k.Submit(context.Background(), deps, "ns", "sess",
		"user:"+base64.RawURLEncoding.EncodeToString([]byte("alice@example.com")),
		"urn:ap:view:artifact:artifact-3f2a1b8c",
		json.RawMessage(`{"text":"the CTA padding is wrong"}`))
	require.NoError(t, err, "Submit must succeed when NATS request routes the message")
	assert.Equal(t, "routed", res.Outcome)

	var pl channelevents.ViewMessagePayload
	require.NoError(t, json.Unmarshal(gotEnv.Payload, &pl))
	assert.Equal(t, "the CTA padding is wrong", pl.Text)
	assert.Equal(t, "urn:ap:view:artifact:artifact-3f2a1b8c", pl.Via)
	assert.Equal(t, "alice@example.com", pl.Author.Email.String(), "subject must decode back to the original email")
	assert.Equal(t, "idp", pl.Author.Kind.String())
}

func TestUserMessageSubmit_EmptyTextRejectedBeforeNATS(t *testing.T) {
	k, ok := Get("user_message")
	require.True(t, ok, "user_message must be registered")

	natsCalled := false
	deps := Deps{NATSRequest: func(_ string, _ []byte, _ time.Duration) ([]byte, error) {
		natsCalled = true
		return nil, nil
	}}
	_, err := k.Submit(context.Background(), deps, "ns", "sess", "user:x", "", json.RawMessage(`{"text":"  "}`))
	require.Error(t, err, "empty (whitespace-only) text is refused before any NATS request")
	assert.False(t, natsCalled, "NATS request must never be made when text is empty")
}

func TestUserMessageSubmit_UnparseableRawRejected(t *testing.T) {
	k, ok := Get("user_message")
	require.True(t, ok)

	natsCalled := false
	deps := Deps{NATSRequest: func(_ string, _ []byte, _ time.Duration) ([]byte, error) {
		natsCalled = true
		return nil, nil
	}}
	_, err := k.Submit(context.Background(), deps, "ns", "sess", "user:x", "", json.RawMessage(`not json`))
	require.Error(t, err, "malformed raw payload must be rejected, not silently treated as empty text")
	assert.False(t, natsCalled)
}

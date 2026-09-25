package channelevents

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

func TestEnqueueAckKind_ValidAndImplemented(t *testing.T) {
	assert.True(t, KindEnqueueAck.Valid(), "%s must be Valid", KindEnqueueAck)
	assert.True(t, KindEnqueueAck.Implemented(), "%s must be Implemented", KindEnqueueAck)
}

func TestEnqueueAckPayload_RoundTrip(t *testing.T) {
	env, err := BuildEnvelope("ns", "s1", KindEnqueueAck, EnqueueAckPayload{
		RequestID:  "req-1",
		Requester:  ExternalIdentity{Kind: "slack", ExternalID: "U_BOB"},
		SessionRef: "default/foo",
		Caption:    "reviewing the terraform plan",
	})
	assert.NoError(t, err)
	assert.Equal(t, KindEnqueueAck, env.Kind)

	var got EnqueueAckPayload
	assert.NoError(t, json.Unmarshal(env.Payload, &got))
	assert.Equal(t, "req-1", got.RequestID)
	assert.Equal(t, identity.Kind("slack"), got.Requester.Kind)
	assert.Equal(t, identity.RawExternalID("U_BOB"), got.Requester.ExternalID)
	assert.Equal(t, "default/foo", got.SessionRef)
	assert.Equal(t, "reviewing the terraform plan", got.Caption)
}

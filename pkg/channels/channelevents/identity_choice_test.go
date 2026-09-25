package channelevents

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIdentityChoiceRequestEnvelope_roundTrips(t *testing.T) {
	assert.True(t, KindIdentityChoiceRequest.Valid())
	assert.True(t, KindIdentityChoiceRequest.Implemented())

	pl := IdentityChoiceRequestPayload{
		RequestID:        "r1",
		Requester:        ExternalIdentity{Kind: "slack", ExternalID: "U_SAM"},
		AgentDisplayName: "the on-call bot",
		Mode:             "dynamic",
		Recommended:      "userPassthrough",
		Reason:           "reading a shared doc",
		ChoiceTTL:        "5m",
	}
	env, err := BuildEnvelope("ns", "s1", KindIdentityChoiceRequest, pl)
	require.NoError(t, err)
	assert.Equal(t, KindIdentityChoiceRequest, env.Kind)

	var got IdentityChoiceRequestPayload
	require.NoError(t, json.Unmarshal(env.Payload, &got))
	assert.Equal(t, pl, got)
}

func TestIdentityChoiceAppliedEnvelope_roundTrips(t *testing.T) {
	assert.True(t, KindIdentityChoiceApplied.Valid())
	assert.True(t, KindIdentityChoiceApplied.Implemented())

	pl := IdentityChoiceAppliedPayload{
		RequestID:        "r1",
		Action:           "agent",
		ApproverID:       "U_SAM",
		Reason:           "timeout",
		AgentDisplayName: "the on-call bot",
		ResponseURL:      "https://hooks.slack.example/response",
	}
	env, err := BuildEnvelope("ns", "s1", KindIdentityChoiceApplied, pl)
	require.NoError(t, err)
	assert.Equal(t, KindIdentityChoiceApplied, env.Kind)

	var got IdentityChoiceAppliedPayload
	require.NoError(t, json.Unmarshal(env.Payload, &got))
	assert.Equal(t, pl, got)
}

func TestIdentityChoiceDecisionEnvelope_roundTrips(t *testing.T) {
	assert.True(t, KindIdentityChoiceDecision.Valid())
	assert.True(t, KindIdentityChoiceDecision.Implemented())

	pl := IdentityChoiceDecisionPayload{
		RequestID:   "r1",
		Approver:    ExternalIdentity{Kind: "slack", ExternalID: "U_SAM"},
		Action:      "userPassthrough",
		ResponseURL: "https://hooks.slack.example/response",
	}
	env, err := BuildEnvelope("ns", "s1", KindIdentityChoiceDecision, pl)
	require.NoError(t, err)
	assert.Equal(t, KindIdentityChoiceDecision, env.Kind)

	var got IdentityChoiceDecisionPayload
	require.NoError(t, json.Unmarshal(env.Payload, &got))
	assert.Equal(t, pl, got)
}

package local

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/clienthosted"
)

// TestListener_SubmitInteractionDecision_PublishesEnvelope: the
// generic-interaction decision path publishes a KindInteractionDecision
// envelope on the session IN subject carrying Category/RequestRef/ActionID,
// with the local single user stamped as the Decider.
func TestListener_SubmitInteractionDecision_PublishesEnvelope(t *testing.T) {
	var gotSubj string
	var gotBody []byte
	l := newListener(clienthosted.Listener{
		Deps: channelkinds.Deps{
			Channel: testChannel(),
			NATSPublish: func(subj string, body []byte) error {
				gotSubj, gotBody = subj, body
				return nil
			},
		},
		Ext: channelkinds.ExternalIdentity{Kind: "local", ExternalID: "local-user"},
	})
	err := l.SubmitInteractionDecision(context.Background(), "default", "s1", "req-1", "tool_approval", "approve")
	require.NoError(t, err)
	assert.Contains(t, gotSubj, "ap.component.interaction_decision.default.s1")

	var env channelevents.Envelope
	require.NoError(t, json.Unmarshal(gotBody, &env))
	assert.Equal(t, channelevents.KindInteractionDecision, env.Kind)

	var pl channelevents.InteractionDecisionPayload
	require.NoError(t, json.Unmarshal(env.Payload, &pl))
	assert.Equal(t, "tool_approval", pl.Category)
	assert.Equal(t, "req-1", pl.RequestRef)
	assert.Equal(t, "approve", pl.ActionID)
	// The denormalized session ref is renderer convenience, not routing (the
	// envelope's Session is authoritative), but a consumer that reads it must not
	// find it empty on decisions that happen to come from this surface.
	assert.Equal(t, "default", pl.AgentSessionRef.Namespace)
	assert.Equal(t, "s1", pl.AgentSessionRef.Name)
	assert.Equal(t, "local-user", pl.Decider.ExternalID.String())
	assert.Equal(t, "local", pl.Decider.Kind.String())
}

// TestListener_SubmitInteractionDecision_WithoutTransportFailsLoudly is the
// wiring-error guard: a nil NATSPublish must be a loud error, not a nil-deref.
func TestListener_SubmitInteractionDecision_WithoutTransportFailsLoudly(t *testing.T) {
	l := newListener(clienthosted.Listener{Deps: channelkinds.Deps{}})
	err := l.SubmitInteractionDecision(context.Background(), "default", "s1", "req-1", "tool_approval", "approve")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no NATS publish wired")
}

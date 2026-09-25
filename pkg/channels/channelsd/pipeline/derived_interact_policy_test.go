package pipeline

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
)

// derivedInteractClass is agentClassForTest's counterpart for a class that
// DECLARED no interact policy and had one derived onto its status — the shape
// the AgentClass reconciler produces for a class whose input carries no human
// and whose single role=output Channel names a membership.
func derivedInteractClass(t *testing.T, declared, derived string) *spiceboxv1alpha1.AgentClass {
	t.Helper()
	ac := agentClassForTest(t, declared)
	ac.Status.DerivedSessionInteractPermission = derived
	return ac
}

// The snapshot must read the EFFECTIVE policy. Reading only the declared half
// left status.appliedInteractPermission empty for a derived class, so no
// participant tuple was written and nobody in the room the agent posts into
// could interact with the session it announced — the derivation's whole point.
func TestDeliver_AppliesADerivedInteractPolicy(t *testing.T) {
	ch := newChannel("c1")
	agentClass := derivedInteractClass(t, "", "slack_channel:C0DEMO123#member")
	p, az, _, _, cli := newPipeline(t, ch, agentClass)

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "fake", ExternalID: "U1", Email: "a@example.com"},
		ChannelKey:  "thread:C1:derived",
		MessageText: "hello",
	})
	require.NoError(t, err, "Deliver")
	require.Equal(t, channelkinds.OutcomeRouted, dec.Outcome)

	assert.Equal(t, 1, az.participantCalls, "TouchInteractParticipant calls")
	assert.Equal(t, "slack_channel:C0DEMO123#member", az.lastParticipantSubject)

	var sess spiceboxv1alpha1.AgentSession
	require.NoError(t, cli.Get(context.Background(),
		client.ObjectKey{Namespace: "default", Name: dec.Session.Name}, &sess), "get session")

	// The snapshot is what a later fork or restart reads to carry the policy
	// onto the child, so an empty one silently drops it there too.
	assert.Equal(t, "slack_channel:C0DEMO123#member", sess.Status.AppliedInteractPermission)
	cond := conditions.Find(sess.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionInteractPolicyApplied)
	if assert.NotNil(t, cond, "condition %q (have: %+v)",
		spiceboxv1alpha1.AgentSessionConditionInteractPolicyApplied, sess.Status.Conditions) {
		assert.Equal(t, spiceboxv1alpha1.ReasonInteractPolicyApplied, cond.Reason,
			"a derived policy is applied, not reported as unconfigured")
	}
}

// Precedence is the accessor's, not this call site's: an authored value fills
// no absence and must never be overridden by a stale derived one.
func TestDeliver_DeclaredInteractPolicyBeatsTheDerivedOne(t *testing.T) {
	ch := newChannel("c1")
	agentClass := derivedInteractClass(t, "group:eng#member", "slack_channel:C0DEMO123#member")
	p, az, _, _, cli := newPipeline(t, ch, agentClass)

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "fake", ExternalID: "U1", Email: "a@example.com"},
		ChannelKey:  "thread:C1:declared",
		MessageText: "hello",
	})
	require.NoError(t, err, "Deliver")
	require.Equal(t, channelkinds.OutcomeRouted, dec.Outcome)

	assert.Equal(t, "group:eng#member", az.lastParticipantSubject)

	var sess spiceboxv1alpha1.AgentSession
	require.NoError(t, cli.Get(context.Background(),
		client.ObjectKey{Namespace: "default", Name: dec.Session.Name}, &sess), "get session")
	assert.Equal(t, "group:eng#member", sess.Status.AppliedInteractPermission)
}

// pkg/channels/channelsd/pipeline/queued_interrupt_test.go
//
// decideQueuedInterrupt — the bound decision handler for the queued_messages
// interaction category (the mid-turn "Interrupt & Send Now" click).
// Tests call p.decideQueuedInterrupt directly (not through
// HandleInteractionDecision), since the handler performs no standing check of
// its own — the generic pipe already ran the category's DecideParticipant
// policy (CheckInteract, fail-closed) before invoking it. Its only job is to
// republish the click as a KindInterruptRequest on the runner's IN subject and
// return Suppressed:true so the pipe skips the synchronous applied (the bridge
// resolves the card later from the runner's interrupt_applied).
package pipeline

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
)

// TestDecideQueuedInterrupt verifies the bound handler publishes exactly one
// KindInterruptRequest on the session's IN subject, round-tripping the
// RequestID + ResponseURL + clicker identity from the decision, and returns a
// suppressed Outcome (so the generic pipe does not publish the synchronous
// interaction_applied — the bridge does that from the runner's echo).
func TestDecideQueuedInterrupt(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "sess1", Namespace: "default"},
		Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
	}
	p, _, _, nats, _ := newPipeline(t, sess)

	clicker := channelevents.ExternalIdentity{
		Kind: "slack", ExternalID: "U_CLICKER", Email: "clicker@example.com", TeamScope: "T1",
	}
	out, err := p.decideQueuedInterrupt(context.Background(), channelinteractions.Decision{
		Session: channelevents.SessionRef{Namespace: sess.Namespace, Name: sess.Name},
		Payload: channelevents.InteractionDecisionPayload{
			Category:    categories.QueuedMessages,
			RequestRef:  "queued-req-1",
			ActionID:    "interrupt",
			Decider:     clicker,
			ResponseRef: "https://hooks.slack.com/actions/T/1/interrupt",
		},
	})
	require.NoError(t, err, "decideQueuedInterrupt must not error on a resolvable session")
	assert.True(t, out.Suppressed, "queued_messages resolves out-of-band — the Outcome must be Suppressed")
	assert.Empty(t, out.Result, "a suppressed Outcome carries no Result")

	// Exactly one publish, on the IN interrupt_request subject.
	require.Len(t, nats.subjects, 1, "decideQueuedInterrupt must publish exactly one envelope")
	assert.Equal(t, "ap.session.default.sess1.in.interrupt_request", nats.subjects[0],
		"the interrupt request must go on the runner's IN subject")

	var env channelevents.Envelope
	require.NoError(t, json.Unmarshal(nats.payloads[0], &env), "unmarshal envelope")
	assert.Equal(t, channelevents.KindInterruptRequest, env.Kind, "kind")

	var pl channelevents.InterruptRequestPayload
	require.NoError(t, json.Unmarshal(env.Payload, &pl), "unmarshal interrupt request payload")
	assert.Equal(t, "queued-req-1", pl.RequestID, "RequestID round-trips the decision's RequestRef (the enqueue-ack correlation id)")
	assert.Equal(t, "default/sess1", pl.SessionRef, "SessionRef")
	assert.Equal(t, clicker.ExternalID, pl.Requester.ExternalID, "Requester carries the clicker's raw external id")
	assert.Equal(t, clicker.Email, pl.Requester.Email, "Requester carries the clicker's email")
	assert.Equal(t, "https://hooks.slack.com/actions/T/1/interrupt", pl.ResponseURL,
		"ResponseURL round-trips the decision's ResponseRef so the applied echo can edit the ephemeral in place")
}

// TestBindQueuedInterruptHandler wires decideQueuedInterrupt into the
// interaction registry as the bound handler for categories.QueuedMessages.
func TestBindQueuedInterruptHandler(t *testing.T) {
	saved := channelinteractions.All()
	channelinteractions.ResetBindings()
	t.Cleanup(func() {
		channelinteractions.Reset()
		for _, c := range saved {
			channelinteractions.Register(c)
		}
		channelinteractions.ResetBindings()
	})

	p, _, _, _, _ := newPipeline(t)
	BindQueuedInterruptHandler(p)
	_, bound := channelinteractions.HandlerFor(categories.QueuedMessages)
	assert.True(t, bound, "BindQueuedInterruptHandler must bind the queued_messages decision handler")
}

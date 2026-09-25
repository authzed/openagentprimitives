package pipeline

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
)

// fixtureOtherParkCategory parks at AwaitingCredentials (the credential_link
// shape) rather than AwaitingDecision, proving the durable cross-check is
// scoped to the AwaitingDecision park only. Distinct from
// fixtureRegenerateCategory (resurface_test.go), which uses
// ResurfaceRegenerate: this one uses ResurfaceCached so the test exercises the
// same republish path as the AwaitingDecision case, varying only Park.
const fixtureOtherParkCategory = "fixture_other_park"

// requestKinds decodes every payload fakeNATS captured into its envelope
// Kind, in publish order — lets a test assert on *what* was published
// without hand-rolling json.Unmarshal at every call site.
func requestKinds(t *testing.T, nats *fakeNATS) []channelevents.Kind {
	t.Helper()
	kinds := make([]channelevents.Kind, 0, len(nats.payloads))
	for _, p := range nats.payloads {
		var env channelevents.Envelope
		require.NoError(t, json.Unmarshal(p, &env), "unmarshal captured envelope")
		kinds = append(kinds, env.Kind)
	}
	return kinds
}

// TestResurface_InteractionLeg_DurableCrossCheck pins the stale-guard on the
// cached resurface leg: an AwaitingDecision-parking prompt whose decision
// already landed must not be republished. The parked_prompt record alone can
// outlive a decision applied on another replica, so Status.PendingInteractions
// is the authority. Categories parking at a different phase never get a
// PendingInteractions entry at all and must be unaffected by the cross-check —
// proven by the third subtest.
func TestResurface_InteractionLeg_DurableCrossCheck(t *testing.T) {
	registerInteractionCategory(t, channelinteractions.DecideApprovers) // fixtureInteractionCategory: Park=AwaitingDecision, Resurface=ResurfaceCached
	channelinteractions.Register(channelinteractions.Category{
		Name:      fixtureOtherParkCategory,
		Park:      spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials,
		Tone:      channelinteractions.ToneRoutine,
		Deciders:  channelinteractions.DecideRequester,
		Resurface: channelinteractions.ResurfaceCached,
	})

	sessKey := client.ObjectKey{Namespace: "ns", Name: "s"}
	approver := channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_X", Email: "x@example.com"}
	newSess := func(phase string) *spiceboxv1alpha1.AgentSession {
		return &spiceboxv1alpha1.AgentSession{
			ObjectMeta: metav1.ObjectMeta{Namespace: sessKey.Namespace, Name: sessKey.Name},
			Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: phase},
		}
	}

	t.Run("parking category, RequestRef still in PendingInteractions: republished", func(t *testing.T) {
		mem := newTestMemory(t)
		notePendingInteractionRequest(t, mem, sessKey, fixtureInteractionCategory, "req-live",
			channelevents.InteractionRequestPayload{
				Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceApprovers, Approvers: []channelevents.ExternalIdentity{approver}},
			})
		nats := &fakeNATS{}
		p := &Pipeline{Mem: mem, NATS: nats}
		s := newSess(spiceboxv1alpha1.AgentSessionPhaseAwaitingDecision)
		s.Status.PendingInteractions = []spiceboxv1alpha1.PendingInteraction{
			{RequestID: "req-live", RequestRef: "req-live", Category: fixtureInteractionCategory, RequestedAt: metav1.Now()},
		}

		p.resurfacePending(context.Background(), s)

		assert.Contains(t, requestKinds(t, nats), channelevents.KindInteractionRequest, "still-pending prompt is resurfaced")
	})

	t.Run("parking category, RequestRef absent from PendingInteractions (decision already landed): not republished", func(t *testing.T) {
		mem := newTestMemory(t)
		notePendingInteractionRequest(t, mem, sessKey, fixtureInteractionCategory, "req-resolved",
			channelevents.InteractionRequestPayload{
				Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceApprovers, Approvers: []channelevents.ExternalIdentity{approver}},
			})
		nats := &fakeNATS{}
		p := &Pipeline{Mem: mem, NATS: nats}
		s := newSess(spiceboxv1alpha1.AgentSessionPhaseAwaitingDecision)
		// PendingInteractions intentionally left empty: the decision already
		// landed (e.g. on another replica) and cleared the durable entry while
		// the parked_prompt record was still outstanding here.

		p.resurfacePending(context.Background(), s)

		assert.Empty(t, nats.subjects, "resolved prompt must not be re-fired")
	})

	t.Run("non-AwaitingDecision-parking category, RequestRef absent from PendingInteractions: still republished", func(t *testing.T) {
		mem := newTestMemory(t)
		notePendingInteractionRequest(t, mem, sessKey, fixtureOtherParkCategory, "req-other-park",
			channelevents.InteractionRequestPayload{})
		nats := &fakeNATS{}
		p := &Pipeline{Mem: mem, NATS: nats}
		s := newSess(spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials)
		// PendingInteractions intentionally left empty: a category parking
		// outside AwaitingDecision (the credential_link shape) never gets a
		// PendingInteractions entry, so the cross-check must not gate it — it
		// keeps resurfacing under the publish-then-clear ordering guard alone.

		p.resurfacePending(context.Background(), s)

		require.Len(t, nats.subjects, 1, "a category parking outside AwaitingDecision must be unaffected by the durable cross-check")
		assert.Equal(t, channelevents.KindInteractionRequest, requestKinds(t, nats)[0])
	})
}

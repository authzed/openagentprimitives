package pipeline

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// TestHandleInteractionDecision_PublishesRejection proves EVERY reject path
// publishes a per-clicker KindInteractionDecisionRejected rather than returning
// nil: not_authorized (standing/no-canonical), already_resolved (spectator),
// and category_mismatch. A silent return leaves the clicker staring at a card
// that never resolves. Per row: exactly one rejection with the right Class
// publishes, NO Applied is published (a rejected click never resolves the
// card), the bound handler never runs, and the parked prompt survives so the
// real approver can still act.
//
// The DecideResourceOwners rejects are covered by
// TestHandleInteractionDecision_DecideResourceOwners; the per-path
// standing/mismatch/handler-error rejects are also asserted in
// interaction_decision_test.go / identity_choice_decision_test.go. This table
// is the single place that exercises them together as "no path stays silent".
func TestHandleInteractionDecision_PublishesRejection(t *testing.T) {
	cases := []struct {
		name         string
		wantClass    string
		promptCached bool
		// arrange registers the category/categories, binds a handler that would
		// resolve if reached (so a wrongly-admitted click surfaces as an Applied),
		// seeds any cache state, and returns the decision envelope to feed. It runs
		// against a pipeline whose Engine DENIES standing (checkApproveResult:false)
		// so the DecideApprovers path rejects.
		arrange func(t *testing.T, p *Pipeline, sessKey client.ObjectKey, mem memory.Memory, calls *int) channelevents.Envelope
	}{
		{
			name:         "unauthorized decider → not_authorized rejection, prompt survives",
			wantClass:    "not_authorized",
			promptCached: true,
			arrange: func(t *testing.T, p *Pipeline, sessKey client.ObjectKey, mem memory.Memory, calls *int) channelevents.Envelope {
				registerInteractionCategory(t, channelinteractions.DecideApprovers)
				channelinteractions.Bind(fixtureInteractionCategory,
					trackingHandler(calls, channelinteractions.Outcome{Result: channelevents.OutcomeApproved}, nil))
				decider := channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_RANDOM", Email: "random@example.com"}
				notePendingInteractionRequest(t, mem, sessKey, fixtureInteractionCategory, "req-1", approversFixtureRequest(decider))
				return mustBuildInteractionDecision(t, sessKey, fixtureInteractionCategory, "req-1", "approve", decider)
			},
		},
		{
			name:         "no-canonical decider (no email/subject) → not_authorized rejection",
			wantClass:    "not_authorized",
			promptCached: false,
			arrange: func(t *testing.T, p *Pipeline, sessKey client.ObjectKey, mem memory.Memory, calls *int) channelevents.Envelope {
				registerInteractionCategory(t, channelinteractions.DecideApprovers)
				channelinteractions.Bind(fixtureInteractionCategory,
					trackingHandler(calls, channelinteractions.Outcome{Result: channelevents.OutcomeApproved}, nil))
				// No email and no Subject: identity.FromExternal(...).Canonical() returns
				// ErrSyntheticSubject (the decider-canonical resolution does NOT opt into
				// AllowSynthetic), so the pipe rejects at the no-canonical branch — BEFORE
				// the policy switch, so no cached request is needed.
				decider := channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_GUEST"}
				return mustBuildInteractionDecision(t, sessKey, fixtureInteractionCategory, "req-1", "approve", decider)
			},
		},
		{
			name:         "already-resolved spectator → already_resolved rejection with original decider/outcome",
			wantClass:    "already_resolved",
			promptCached: true,
			arrange: func(t *testing.T, p *Pipeline, sessKey client.ObjectKey, mem memory.Memory, calls *int) channelevents.Envelope {
				registerInteractionCategory(t, channelinteractions.DecideApprovers)
				channelinteractions.Bind(fixtureInteractionCategory,
					trackingHandler(calls, channelinteractions.Outcome{Result: channelevents.OutcomeApproved}, nil))
				first := channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_FIRST", Email: "first@example.com"}
				notePendingInteractionRequest(t, mem, sessKey, fixtureInteractionCategory, "req-1", approversFixtureRequest(first))
				// Seed the resolvedCache directly so the later click is a spectator.
				// p.Now is a fixed 2026-04-28 stub; the cache's TTL check uses the real
				// wall clock, so a stub-stamped entry would read back already-expired —
				// stamp the real clock here (mirrors the spectator test in
				// interaction_decision_test.go).
				// Keyed by CATEGORY too: a resolution under one category no longer
				// blocks another's click on the same id (see resolvedKey).
				p.resolvedCache.put(fixtureInteractionCategory, "req-1", resolvedDecision{Approver: first, Decision: channelevents.OutcomeApproved, ResolvedAt: time.Now()})
				late := channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_LATE", Email: "late@example.com"}
				return mustBuildInteractionDecision(t, sessKey, fixtureInteractionCategory, "req-1", "approve", late)
			},
		},
		{
			name:         "category mismatch → category_mismatch rejection, prompt survives",
			wantClass:    "category_mismatch",
			promptCached: true,
			arrange: func(t *testing.T, p *Pipeline, sessKey client.ObjectKey, mem memory.Memory, calls *int) channelevents.Envelope {
				const otherCategory = "fixture_interaction_other"
				registerInteractionCategory(t, channelinteractions.DecideApprovers) // registers fixtureInteractionCategory
				channelinteractions.Register(channelinteractions.Category{
					Name:      otherCategory,
					Park:      spiceboxv1alpha1.AgentSessionPhaseAwaitingDecision,
					Tone:      channelinteractions.ToneRoutine,
					Deciders:  channelinteractions.DecideApprovers,
					Resurface: channelinteractions.ResurfaceCached,
				})
				decider := channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_ALICE", Email: "alice@example.com"}
				// Cache a fixtureInteractionCategory request but decide under otherCategory
				// for the SAME requestRef — the integrity check rejects before standing.
				channelinteractions.Bind(otherCategory,
					trackingHandler(calls, channelinteractions.Outcome{Result: channelevents.OutcomeApproved}, nil))
				notePendingInteractionRequest(t, mem, sessKey, fixtureInteractionCategory, "req-1", approversFixtureRequest(decider))
				return mustBuildInteractionDecision(t, sessKey, otherCategory, "req-1", "approve", decider)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cli := newFakeK8sClientWithExistingSession(t, "U_ALICE")
			sessKey := sessKeyForFixture()
			natsRec := &fakeNATS{}
			az := &fakeAuthz{checkApproveResult: false} // DecideApprovers standing DENIED
			p := newTestPipeline(t, cli, az, natsRec)
			mem := newTestMemory(t)
			p.Mem = mem

			calls := 0
			env := tc.arrange(t, p, sessKey, mem, &calls)
			require.NoError(t, p.HandleInteractionDecision(context.Background(), env),
				"a rejected click is a fail-closed reject, not a plumbing error")

			assert.Equal(t, 0, calls, "the bound handler must not run on a rejected click")
			assert.False(t, sawPublishedKind(natsRec, channelevents.KindInteractionApplied),
				"no Applied publish for a rejected click")
			require.True(t, sawPublishedKind(natsRec, channelevents.KindInteractionDecisionRejected),
				"a rejection MUST be published (no silent return nil)")

			rej := findInteractionDecisionRejected(t, natsRec)
			assert.Equal(t, tc.wantClass, rej.Class, "rejection Class")
			assert.Equal(t, "req-1", rej.RequestRef, "rejection RequestRef")
			assert.Equal(t, env.Session, rej.AgentSessionRef, "rejection stamps the envelope-derived session ref")

			if tc.wantClass == "already_resolved" {
				require.NotNil(t, rej.OriginalDecider, "already_resolved names the original decider")
				assert.Equal(t, "U_FIRST", string(rej.OriginalDecider.ExternalID), "original decider carried")
				assert.Equal(t, string(channelevents.OutcomeApproved), rej.OriginalOutcome, "original outcome carried")
			}

			if tc.promptCached {
				assert.Len(t, outstandingPrompts(t, p, sessKey), 1,
					"a rejected click must leave the cached prompt parked for the real approver")
			}
		})
	}
}

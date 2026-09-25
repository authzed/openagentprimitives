package pipeline

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
)

// Cross-restart idempotency. The in-process resolvedCache is the warm
// already-resolved gate, and NewPipeline builds it empty; every other input a
// re-decision needs survives durably. So without a durable gate, a second
// approver's click after a restart re-runs the bound handler in full — for
// tool_approval a fresh SpiceDB grant tuple (TTL=0 = a session-wide backstop
// that silently auto-approves the next identical call) plus a second, possibly
// contradicting, interaction_applied.
//
// Second clicks are ordinary: sendRequest fans one prompt out per approver and
// only the clicker's copy is edited, so approvers 2..n keep armed buttons — and
// after a restart the delivery store is empty, so not even the recorded copies
// can be edited.
//
// The durable witness is the parked-prompt tombstone clearPendingPrompt writes
// on the first decision. This test models the restart the only way that matters:
// a SECOND Pipeline over the SAME durable stores.
func TestHandleInteractionDecision_AfterRestart_DurablyResolvedPrompt_IsSpectator(t *testing.T) {
	registerInteractionCategory(t, channelinteractions.DecideApprovers)
	calls := 0
	channelinteractions.Bind(fixtureInteractionCategory, trackingHandler(&calls, channelinteractions.Outcome{
		Result: channelevents.OutcomeApproved,
	}, nil))

	cli := newFakeK8sClientWithExistingSession(t, "U_ALICE")
	sessKey := sessKeyForFixture()
	mem := newTestMemory(t)

	// --- channelsd process #1: approver A decides. ---
	firstNATS := &fakeNATS{}
	p1 := newTestPipeline(t, cli, &fakeAuthz{checkApproveResult: true}, firstNATS)
	p1.Mem = mem
	alice := channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_ALICE", Email: "alice@example.com"}
	notePendingInteractionRequest(t, mem, sessKey, fixtureInteractionCategory, "req-1", approversFixtureRequest(alice))
	require.NoError(t, p1.HandleInteractionDecision(context.Background(),
		mustBuildInteractionDecision(t, sessKey, fixtureInteractionCategory, "req-1", "approve", alice)),
		"first decision (approver A)")
	require.Equal(t, 1, calls, "the first decision runs the bound handler")
	require.Empty(t, outstandingPrompts(t, p1, sessKey), "the first decision resolves the parked prompt")

	// --- channelsd restarts: a brand-new Pipeline, empty resolvedCache, same
	// durable memory and same API objects. Approver B's card is still armed. ---
	secondNATS := &fakeNATS{}
	p2 := newTestPipeline(t, cli, &fakeAuthz{checkApproveResult: true}, secondNATS)
	p2.Mem = mem
	bob := channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_BOB", Email: "bob@example.com"}
	require.NoError(t, p2.HandleInteractionDecision(context.Background(),
		mustBuildInteractionDecision(t, sessKey, fixtureInteractionCategory, "req-1", "approve", bob)),
		"post-restart decision (approver B)")

	assert.Equal(t, 1, calls,
		"the bound handler must NOT re-run after a restart: a second grant write is a session-wide backstop nothing revokes")
	assert.False(t, sawPublishedKind(secondNATS, channelevents.KindInteractionApplied),
		"no second interaction_applied — the runner already resumed on the first one")
	rej := findInteractionDecisionRejected(t, secondNATS)
	assert.Equal(t, "already_resolved", rej.Class, "the post-restart click is a spectator")
	assert.Equal(t, "req-1", rej.RequestRef, "rejection names the request that was clicked")
	assert.Equal(t, bob.ExternalID, rej.Clicker.ExternalID, "rejection is addressed to the late clicker")
}

// Same hole, no restart needed: the gate-side timeout path clears the pending
// entry and the parked prompt but never touches resolvedCache, so without the
// durable gate a click landing after the timeout runs the handler in full,
// writing a grant for a request the runner already denied.
func TestHandleInteractionDecision_AfterTimeoutApplied_IsSpectator(t *testing.T) {
	registerInteractionCategory(t, channelinteractions.DecideApprovers)
	calls := 0
	channelinteractions.Bind(fixtureInteractionCategory, trackingHandler(&calls, channelinteractions.Outcome{
		Result: channelevents.OutcomeApproved,
	}, nil))

	cli := newFakeK8sClientWithExistingSession(t, "U_ALICE")
	sessKey := sessKeyForFixture()
	natsRec := &fakeNATS{}
	p := newTestPipeline(t, cli, &fakeAuthz{checkApproveResult: true}, natsRec)
	p.Now = func() time.Time { return time.Now() }
	mem := newTestMemory(t)
	p.Mem = mem

	alice := channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_ALICE", Email: "alice@example.com"}
	notePendingInteractionRequest(t, mem, sessKey, fixtureInteractionCategory, "req-1", approversFixtureRequest(alice))
	seedPendingInteraction(t, cli, sessKey, spiceboxv1alpha1.PendingInteraction{
		RequestID:   "req-1",
		Category:    fixtureInteractionCategory,
		RequestRef:  "req-1",
		RequestedAt: metav1.NewTime(time.Now()),
	})

	// The runner's gate-side timeout watcher: interaction_applied(expired) on IN.
	expired, err := channelevents.BuildEnvelope(sessKey.Namespace, sessKey.Name,
		channelevents.KindInteractionApplied,
		channelevents.InteractionAppliedPayload{
			AgentSessionRef: channelevents.SessionRef{Namespace: sessKey.Namespace, Name: sessKey.Name},
			Category:        fixtureInteractionCategory,
			RequestRef:      "req-1",
			Outcome:         channelevents.OutcomeExpired,
		})
	require.NoError(t, err, "build timeout applied envelope")
	require.NoError(t, p.HandleInteractionApplied(context.Background(), expired), "timeout applied")
	require.Empty(t, outstandingPrompts(t, p, sessKey), "the timeout path resolves the parked prompt")

	// The approver clicks Approve on their still-armed card, after the timeout.
	require.NoError(t, p.HandleInteractionDecision(context.Background(),
		mustBuildInteractionDecision(t, sessKey, fixtureInteractionCategory, "req-1", "approve", alice)),
		"post-timeout click")

	assert.Equal(t, 0, calls,
		"the bound handler must NOT run after the gate timed out — the grant would outlive a request the runner already denied")
	assert.False(t, sawPublishedKind(natsRec, channelevents.KindInteractionApplied),
		"no interaction_applied(approved) contradicting the timeout")
	rej := findInteractionDecisionRejected(t, natsRec)
	assert.Equal(t, "already_resolved", rej.Class, "the post-timeout click is a spectator")
}

// A category whose prompt was never durably noted (ResurfaceRegenerate, or a
// note that failed) must be unaffected: absence of a record is NOT a verdict.
// This is the fail-closed regression the audit's own fix sketch would have
// caused — it proposed keying on the absence of a status.PendingInteractions
// entry, which conflates "resolved" with "never parked".
func TestHandleInteractionDecision_NoDurablePromptRecord_StillDecides(t *testing.T) {
	registerInteractionCategory(t, channelinteractions.DecideApprovers)
	calls := 0
	channelinteractions.Bind(fixtureInteractionCategory, trackingHandler(&calls, channelinteractions.Outcome{
		Result: channelevents.OutcomeApproved,
	}, nil))

	cli := newFakeK8sClientWithExistingSession(t, "U_ALICE")
	sessKey := sessKeyForFixture()
	natsRec := &fakeNATS{}
	p := newTestPipeline(t, cli, &fakeAuthz{checkApproveResult: true}, natsRec)
	p.Mem = newTestMemory(t) // empty: no parked-prompt record at all

	alice := channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_ALICE", Email: "alice@example.com"}
	require.NoError(t, p.HandleInteractionDecision(context.Background(),
		mustBuildInteractionDecision(t, sessKey, fixtureInteractionCategory, "req-1", "approve", alice)),
		"decision with no durable prompt record")

	assert.Equal(t, 1, calls, "an un-noted prompt must still decide — absence of a record is not a resolution")
	assert.True(t, sawPublishedKind(natsRec, channelevents.KindInteractionApplied), "Applied still publishes")
}

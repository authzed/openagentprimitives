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
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/parkedprompt"
)

// The category is what selects the standing check, and the click supplies it.
// So a decision that NAMES a weak category resolves whatever RequestRef it
// likes under that category's policy — unless something ties the RequestRef
// back to the category the request was actually raised under.
//
// Two durable witnesses record that tie, and each is written before the prompt
// can be seen: the parked prompt (every ResurfaceCached category) and the
// AgentSession's PendingInteractions entry (every parking category). The tests
// below drive the two ways the tie used to be lost.
const weakFixtureCategory = "fixture_weak_interaction"

// registerWeakFixtureCategory adds a second category alongside the fixture one:
// the weakest decider policy in the registry (any participant), leaving no
// durable witness of its own (Park "" + ResurfaceNone) — the exact shape of
// queued_messages and provider_error_retry, and therefore the shape an attacker
// names.
func registerWeakFixtureCategory(t *testing.T) {
	t.Helper()
	channelinteractions.Register(channelinteractions.Category{
		Name:      weakFixtureCategory,
		Tone:      channelinteractions.ToneRoutine,
		Deciders:  channelinteractions.DecideParticipant,
		Resurface: channelinteractions.ResurfaceNone,
	})
}

// An undecodable parked-prompt envelope used to skip the category check
// entirely: the integrity comparison read the category out of the DECODED
// request payload, so a record that existed but would not parse left `req` nil
// and the comparison was never reached. The click's own category then chose the
// standing check.
//
// The category is a top-level field on the stored record, right next to the
// envelope bytes. Reading it there is what makes an unparseable body a
// non-event for this decision rather than a downgrade of it.
func TestHandleInteractionDecision_UndecodableParkedEnvelope_StillBindsTheCategory(t *testing.T) {
	registerInteractionCategory(t, channelinteractions.DecideResourceOwners)
	registerWeakFixtureCategory(t)
	calls := 0
	channelinteractions.Bind(weakFixtureCategory, trackingHandler(&calls, channelinteractions.Outcome{
		Result: channelevents.OutcomeDenied,
	}, nil))

	cli := newFakeK8sClientWithExistingSession(t, "U_ALICE")
	sessKey := sessKeyForFixture()
	natsRec := &fakeNATS{}
	// checkResult true = every participant passes CheckInteract, which is the
	// whole point: the attacker HAS interact standing and still must not decide.
	p := newTestPipeline(t, cli, &fakeAuthz{checkResult: true}, natsRec)
	p.Mem = newTestMemory(t)

	// The request was raised under the strict category. Its parked prompt is
	// present but its envelope bytes are unreadable.
	require.NoError(t, parkedprompt.Note(context.Background(), p.Mem,
		promptScope(sessKey.Namespace, sessKey.Name), parkedprompt.Content{
			RequestRef: "req-1",
			Category:   fixtureInteractionCategory,
			Envelope:   []byte("{not json"),
		}), "note parked prompt with an unreadable envelope")

	mallory := channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_MALLORY", Email: "mallory@example.com"}
	require.NoError(t, p.HandleInteractionDecision(context.Background(),
		mustBuildInteractionDecision(t, sessKey, weakFixtureCategory, "req-1", "deny", mallory)),
		"the forged decision is handled, not errored")

	assert.Equal(t, 0, calls,
		"a decision naming a different category than the request was raised under must not reach a handler")
	rej := findInteractionDecisionRejected(t, natsRec)
	assert.Equal(t, "category_mismatch", rej.Class,
		"the clicker is told the claim does not match the pending request")
	assert.False(t, sawPublishedKind(natsRec, channelevents.KindInteractionApplied),
		"nothing is applied, so the runner never resumes on a forged resolution")
}

// The parked prompt is only written for ResurfaceCached categories, and a read
// of it can fail. The AgentSession's PendingInteractions entry is the second
// witness — written by HandleInteractionRequest for every PARKING category,
// before the prompt is delivered — and it records the category too.
//
// Without consulting it, a participant-policy click resolved a parked
// resource-owner request: standing was checked against the claimed category, the
// bound handler ran, and Applied published under the victim's RequestRef, which
// is what the waiting gate resumes on.
func TestHandleInteractionDecision_NoParkedPrompt_PendingEntryStillBindsTheCategory(t *testing.T) {
	registerInteractionCategory(t, channelinteractions.DecideResourceOwners)
	registerWeakFixtureCategory(t)
	calls := 0
	channelinteractions.Bind(weakFixtureCategory, trackingHandler(&calls, channelinteractions.Outcome{
		Result: channelevents.OutcomeDenied,
	}, nil))

	cli := newFakeK8sClientWithExistingSession(t, "U_ALICE")
	sessKey := sessKeyForFixture()
	natsRec := &fakeNATS{}
	p := newTestPipeline(t, cli, &fakeAuthz{checkResult: true}, natsRec)
	p.Mem = newTestMemory(t)

	// No parked prompt at all — the memory record is gone (or was never
	// readable). The K8s-witnessed entry is what remains.
	seedPendingInteraction(t, cli, sessKey, spiceboxv1alpha1.PendingInteraction{
		RequestID:   "req-1",
		Category:    fixtureInteractionCategory,
		RequestRef:  "req-1",
		RequestedAt: metav1.NewTime(time.Now()),
	})

	mallory := channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_MALLORY", Email: "mallory@example.com"}
	require.NoError(t, p.HandleInteractionDecision(context.Background(),
		mustBuildInteractionDecision(t, sessKey, weakFixtureCategory, "req-1", "deny", mallory)),
		"the forged decision is handled, not errored")

	assert.Equal(t, 0, calls,
		"the parked entry names the real category; a click claiming another one must not reach a handler")
	rej := findInteractionDecisionRejected(t, natsRec)
	assert.Equal(t, "category_mismatch", rej.Class)
	assert.False(t, sawPublishedKind(natsRec, channelevents.KindInteractionApplied))
}

// The counterweight, and the reason this is a binding rather than a blanket
// refusal: a category that legitimately leaves NO witness — no park, no cached
// prompt — is still decidable by a participant. queued_messages and
// provider_error_retry are that shape, they fire mid-turn against a live
// session, and refusing them would break the interrupt path outright.
//
// Substitution between two witness-less categories buys nothing on its own, and
// the veto it USED to buy -- poisoning resolvedCache under a victim's
// RequestRef -- is closed separately by keying that cache on the category too
// (see TestResolvedCache_AResolutionUnderOneCategoryDoesNotBlockAnother).
func TestHandleInteractionDecision_WitnessLessCategory_StillDecidableByParticipant(t *testing.T) {
	registerInteractionCategory(t, channelinteractions.DecideResourceOwners)
	registerWeakFixtureCategory(t)
	calls := 0
	channelinteractions.Bind(weakFixtureCategory, trackingHandler(&calls, channelinteractions.Outcome{
		Result: channelevents.OutcomeApproved,
	}, nil))

	cli := newFakeK8sClientWithExistingSession(t, "U_ALICE")
	sessKey := sessKeyForFixture()
	natsRec := &fakeNATS{}
	p := newTestPipeline(t, cli, &fakeAuthz{checkResult: true}, natsRec)
	p.Mem = newTestMemory(t)

	alice := channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_ALICE", Email: "alice@example.com"}
	require.NoError(t, p.HandleInteractionDecision(context.Background(),
		mustBuildInteractionDecision(t, sessKey, weakFixtureCategory, "req-weak", "go", alice)),
		"a witness-less participant category decides normally")

	assert.Equal(t, 1, calls, "the bound handler runs")
	assert.True(t, sawPublishedKind(natsRec, channelevents.KindInteractionApplied),
		"Applied is published so the surface and runner see the decision")
}

// Memory is what holds one of the two witnesses, so a pipeline built without it
// keeps working — this is the shape webd's read-only relay and much of the unit
// suite run in. The K8s witness still binds, which the first assertion pins so
// nil memory cannot be read as "no check runs".
func TestHandleInteractionDecision_NoMemory_PendingEntryStillBinds(t *testing.T) {
	registerInteractionCategory(t, channelinteractions.DecideResourceOwners)
	registerWeakFixtureCategory(t)
	calls := 0
	channelinteractions.Bind(weakFixtureCategory, trackingHandler(&calls, channelinteractions.Outcome{
		Result: channelevents.OutcomeDenied,
	}, nil))

	cli := newFakeK8sClientWithExistingSession(t, "U_ALICE")
	sessKey := sessKeyForFixture()
	natsRec := &fakeNATS{}
	p := newTestPipeline(t, cli, &fakeAuthz{checkResult: true}, natsRec)
	var noMem memory.Memory
	p.Mem = noMem

	seedPendingInteraction(t, cli, sessKey, spiceboxv1alpha1.PendingInteraction{
		RequestID:   "req-1",
		Category:    fixtureInteractionCategory,
		RequestRef:  "req-1",
		RequestedAt: metav1.NewTime(time.Now()),
	})

	mallory := channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_MALLORY", Email: "mallory@example.com"}
	require.NoError(t, p.HandleInteractionDecision(context.Background(),
		mustBuildInteractionDecision(t, sessKey, weakFixtureCategory, "req-1", "deny", mallory)))

	assert.Equal(t, 0, calls, "the K8s witness binds the category with no memory configured")
	rej := findInteractionDecisionRejected(t, natsRec)
	assert.Equal(t, "category_mismatch", rej.Class)
}

// The witness legs refuse a substitution for any category that leaves a durable
// record. This is the same hole for the categories that leave NONE — and it is
// closed without enumerating them, which matters because an earlier version of
// that enumeration was wrong: permission_request and session_release are both
// DecideOwner and both witness-less by park/resurface.
//
// The poison was never the handler. It was resolvedCache, keyed on RequestRef
// ALONE: a participant posts a participant-policy decision carrying an owner's
// card ref, resolves nothing of consequence under its own handler, and the
// owner's real click then comes back "already_resolved". A veto by anyone, on
// any pending interaction, without passing that interaction's standing check.
func TestResolvedCache_AResolutionUnderOneCategoryDoesNotBlockAnother(t *testing.T) {
	c := newResolvedDecisionCache()

	c.put(weakFixtureCategory, "req-1", resolvedDecision{
		Decision: channelevents.OutcomeDenied, ResolvedAt: time.Now(),
	})

	_, resolved := c.get(fixtureInteractionCategory, "req-1")
	assert.False(t, resolved,
		"the owner's card must still be decidable after a participant resolved its own")
}

// Idempotency is what the cache is FOR, and it must survive the re-keying: a
// second click on the same card carries the same category.
func TestResolvedCache_SameCategoryStillDedupes(t *testing.T) {
	c := newResolvedDecisionCache()

	c.put(fixtureInteractionCategory, "req-1", resolvedDecision{
		Decision: channelevents.OutcomeApproved, ResolvedAt: time.Now(),
	})

	got, resolved := c.get(fixtureInteractionCategory, "req-1")
	require.True(t, resolved, "a second click on the same card is still a spectator")
	assert.Equal(t, channelevents.OutcomeApproved, got.Decision)
}

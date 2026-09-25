// pkg/controllers/credentialupdaterequest/budget_test.go
//
// The ask budget is the only thing standing between a confused-or-compromised
// agent and an unbounded stream of credential-entry prompts at a human: at most
// two asks per (session, credential) for the life of a session. Collapsing --
// several sessions folding onto one card -- is precisely the mechanism that
// could quietly dissolve it, because a follower publishes nothing of its own
// and stays deliberately non-terminal while it waits.
//
// Every test here is about who pays for an ask when the card is shared:
//
//   - a follower charges its OWN session, because its ask is what rode on the
//     human's attention;
//   - it charges the canonical's session with NOTHING, or one shared dead
//     credential would exhaust the budget of the one session actually holding
//     the card;
//   - two sessions trading canonical and follower roles must still each run
//     out after two -- the anti-evasion property, and the reason an ask is
//     spent when it is DETERMINED rather than when it settles;
//   - a session already out of budget is REFUSED, never quietly attached to
//     somebody else's card (the budget check precedes the collapse check, and
//     must stay there).
//
// Built on collapse_test.go's shared-credential world (sharedWorld,
// agentModeSession, requestFor, rejectingProbe, reconcileNamed, getNamed,
// credentialKey) and propagate_test.go's expireCanonical, for the same reason
// those files use an AgentIdentity: it is the one identity kind several
// sessions genuinely share, so it is the only shape where collapsing -- and
// therefore any of this -- is possible at all.
package credentialupdaterequest_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/credentialupdaterequest"
)

// The sessions these tests need. Distinct from collapse_test.go's pair because
// several of them need more than one request per session, and a request name
// per session-and-round reads better than reusing two fixed ones.
const (
	budSessA = "demo-budget-session-a"
	budSessB = "demo-budget-session-b"
	budSessC = "demo-budget-session-c"
	budSessD = "demo-budget-session-d"
)

const (
	budUIDA types.UID = "demo-budget-session-a-uid"
	budUIDB types.UID = "demo-budget-session-b-uid"
	budUIDC types.UID = "demo-budget-session-c-uid"
	budUIDD types.UID = "demo-budget-session-d-uid"
)

// Request names are "demo-cur-<session letter><round>", so they sort in the
// order the tests raise them. That keeps the canonical election's lowest-name
// tie-break agreeing with arrival order, which is what lets these tests be
// about the BUDGET rather than accidentally about the election.
const (
	budReqA1, budReqA2, budReqA3 = "demo-cur-a1", "demo-cur-a2", "demo-cur-a3"
	budReqB1, budReqB2, budReqB3 = "demo-cur-b1", "demo-cur-b2", "demo-cur-b3"
	budReqC1                     = "demo-cur-c1"
	budReqD1                     = "demo-cur-d1"
)

// sharedCredentialRef is the credential every session in this file resolves:
// the one static credential on the one shared AgentIdentity sharedWorld builds.
// It is the budget KEY, so a seeded prior ask that does not carry exactly this
// is invisible to the budget -- which is why the helpers below stamp it and the
// controls below re-assert it off the live object.
func sharedCredentialRef() spiceboxv1alpha1.ResolvedCredentialRef {
	return spiceboxv1alpha1.ResolvedCredentialRef{
		IdentityKind: spiceboxv1alpha1.IdentityKindAgentIdentity,
		Namespace:    ns,
		Name:         sharedIdentityName,
		Credential:   credName,
	}
}

// spentAskFor builds a request that has ALREADY reached a terminal phase with
// the shared credential resolved -- one of this session's two asks, already
// used, from the budget's point of view.
func spentAskFor(name, session string, uid types.UID) *spiceboxv1alpha1.CredentialUpdateRequest {
	cr := requestFor(name, session, uid, mcpName)
	ref := sharedCredentialRef()
	cr.Status = spiceboxv1alpha1.CredentialUpdateRequestStatus{
		Phase:              spiceboxv1alpha1.CredentialUpdateRequestPhaseExpired,
		Determination:      spiceboxv1alpha1.CredentialUpdateDeterminationRejectedVerified,
		Reason:             "seeded prior ask",
		ResolvedCredential: &ref,
	}
	return cr
}

// budgetSession pairs a session name with the UID its requests' ownerReferences
// carry. Kept as a slice rather than a map so the fixture is built in a fixed
// order -- a map's iteration order would vary run to run, and a fixture that
// differs between runs is the last thing a budget test should have.
type budgetSession struct {
	name string
	uid  types.UID
}

// budgetWorld assembles the shared-credential world plus the named sessions and
// whatever extra objects (requests, seeded prior asks) a test needs.
func budgetWorld(sessions []budgetSession, extra ...client.Object) []client.Object {
	objs := sharedWorld()
	for _, s := range sessions {
		objs = append(objs, agentModeSession(s.name, s.uid))
	}
	return append(objs, extra...)
}

// requireCountableAsk is the positive control every claim in this file leans
// on. "Session X was not charged" and "session Y was" are both vacuous if the
// row in question was never a candidate for counting at all: the budget counts
// a peer only when it is charged to that session AND resolved the same
// credential AND has been determined. This asserts all three off the LIVE
// object, so a test that passes is passing about attribution rather than about
// a row the budget could never have seen.
//
// Every check here is `require`, and the name says so. These are not facts
// about the state under test -- they are the preconditions that make the facts
// asserted AFTER them mean anything. A row that turns out not to be countable
// makes the rest of its test vacuous, so there is nothing to be learned from
// collecting further failures past that point.
func requireCountableAsk(t *testing.T, cr *spiceboxv1alpha1.CredentialUpdateRequest, wantSession string) {
	t.Helper()
	require.Equal(t, wantSession, cr.Spec.SessionRef.Name,
		"control: %s must be charged to %s -- that is the field the budget filters on", cr.Name, wantSession)
	require.NotNil(t, cr.Status.ResolvedCredential,
		"control: %s must have RESOLVED a credential, or the budget cannot see it at all; status=%+v",
		cr.Name, cr.Status)
	require.Equal(t, sharedCredentialRef(), credentialKey(cr.Status.ResolvedCredential),
		"control: %s must name the SHARED credential -- the budget is keyed on it", cr.Name)
	require.NotEmpty(t, cr.Status.Phase, "control: %s must have been determined", cr.Name)
	require.NotEqual(t, spiceboxv1alpha1.CredentialUpdateRequestPhasePending, cr.Status.Phase,
		"control: %s must have been determined; an undetermined ask costs nothing", cr.Name)
}

// assertRefusedForBudget pins the whole shape of an over-budget refusal,
// including the two things that distinguish it from every other way a request
// can fail to get a card: the determination the agent reads verbatim, and the
// absence of a collapse pointer. The latter is the ORDERING claim -- an
// exhausted session is turned away, not quietly attached to another session's
// card.
func assertRefusedForBudget(t *testing.T, cr *spiceboxv1alpha1.CredentialUpdateRequest) {
	t.Helper()
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseRefused, cr.Status.Phase,
		"%s must be refused outright; status=%+v", cr.Name, cr.Status)
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateDeterminationBudgetExhausted, cr.Status.Determination,
		"%s must say WHY it was refused -- the agent reads this verbatim", cr.Name)
	assert.NotEmpty(t, cr.Status.Reason, "no-silent-errors: a refusal always carries a reason")
	// This is the line that carries the ORDERING claim. The phase assertion
	// above already rules out Collapsed; what a nil pointer adds is that the
	// request was never attached to anybody's card in the first place.
	assert.Nil(t, cr.Status.CollapsedInto,
		"%s must not have collapsed: the budget is checked BEFORE the collapse, so an exhausted session is "+
			"turned away rather than allowed to ride somebody else's card", cr.Name)
}

// TestBudget_ATerminalFollowerChargesItsOwnSession is the first half of "who
// pays for a shared card".
//
// A follower gets no card of its own, runs no probe, and asks nobody anything
// directly -- so it is tempting to treat it as free. It is not: it consumed a
// human's attention through the card it attached to, and it collected that
// human's answer as its own. If it did not charge its session, a session could
// ride an unlimited number of other sessions' cards and never run out.
//
// The sequence is the whole proof: the follower is session B's FIRST ask, B
// then raises a second ask that must still open (so the follower counted once,
// not twice, and the world can still produce cards), and B's third is refused.
// Delete the follower's contribution and that third ask opens a THIRD card for
// a session that already had two answers.
func TestBudget_ATerminalFollowerChargesItsOwnSession(t *testing.T) {
	objs := budgetWorld(
		[]budgetSession{{budSessA, budUIDA}, {budSessB, budUIDB}, {budSessD, budUIDD}},
		requestFor(budReqA1, budSessA, budUIDA, mcpName),
		requestFor(budReqB1, budSessB, budUIDB, mcpName),
		requestFor(budReqB2, budSessB, budUIDB, mcpName),
		requestFor(budReqB3, budSessB, budUIDB, mcpName),
		requestFor(budReqD1, budSessD, budUIDD, mcpName),
	)
	probes := rejectingProbe(t)
	c, r, _ := newReconciler(t, objs...)

	// Session A raises the card. Session B's first ask folds onto it.
	reconcileNamed(t, r, budReqA1)
	require.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen, getNamed(t, c, budReqA1).Status.Phase,
		"positive control: the fixture must really produce a card")

	reconcileNamed(t, r, budReqB1)
	follower := getNamed(t, c, budReqB1)
	require.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseCollapsed, follower.Status.Phase,
		"positive control: B's first ask must really be a follower; status=%+v", follower.Status)
	require.NotNil(t, follower.Status.CollapsedInto)
	require.Equal(t, budReqA1, follower.Status.CollapsedInto.Name)
	require.Equal(t, int32(1), probes.Load(),
		"positive control: one card, one determination -- the follower never ran its own")

	// The card expires; the follower settles with it and becomes terminal.
	expireCanonical(t, c, r, budReqA1)
	reconcileNamed(t, r, budReqB1)
	follower = getNamed(t, c, budReqB1)
	require.True(t, spiceboxv1alpha1.IsCredentialUpdateRequestTerminal(follower.Status.Phase),
		"positive control: the follower must really have settled -- an ask still in flight proves nothing "+
			"about a TERMINAL follower; status=%+v", follower.Status)
	requireCountableAsk(t, follower, budSessB)

	// B's SECOND ask must still open: one settled follower is exactly one
	// charge. This is the control that stops the test passing for the wrong
	// reason -- if the follower somehow charged twice, or the budget were
	// simply broken-closed, this would already be refused.
	reconcileNamed(t, r, budReqB2)
	second := getNamed(t, c, budReqB2)
	require.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen, second.Status.Phase,
		"control: B's second ask must still open -- a follower is ONE ask, not two; status=%+v", second.Status)
	require.Equal(t, int32(2), probes.Load(),
		"control: B's own card really did run its own full determination")
	expireCanonical(t, c, r, budReqB2)

	// The claim. B has now had two answers about this credential -- one through
	// a card it shared, one through a card of its own -- and gets no third.
	reconcileNamed(t, r, budReqB3)
	assertRefusedForBudget(t, getNamed(t, c, budReqB3))
	assert.Equal(t, int32(2), probes.Load(),
		"an over-budget refusal must never reach the live probe: the budget precedes Determine")

	// Differential control: a session that has spent NOTHING, reconciled at the
	// very same moment against the very same credential, still gets its card.
	// Without this, "refused" could be a fact about the world having run out of
	// cards rather than about B's own budget.
	reconcileNamed(t, r, budReqD1)
	fresh := getNamed(t, c, budReqD1)
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen, fresh.Status.Phase,
		"a session with an unspent budget still gets a card for the same credential; status=%+v", fresh.Status)
	assert.Equal(t, int32(3), probes.Load(), "and it ran its own determination")
}

// TestBudget_AFollowerChargesNothingToTheCanonicalsSession is the other half.
//
// The budget is charged to the session that RAISED the ask, never to whoever's
// card it ended up riding on. Getting this backwards is not a small error: the
// session holding the card is the one being most useful, and charging it for
// every other session that piled on would exhaust exactly the wrong budget --
// five sessions on one dead bot token would silence the one session that
// actually got a human asked.
//
// The negative claim ("A was not charged") is only worth anything if the
// followers were genuinely countable rows, so both are asserted to be terminal,
// attributed to their own sessions, and resolved to the identical credential --
// three followers plus A's own ask is three, well past the limit of two, so a
// misattribution would be unmissable. And because "A is not refused" could
// equally mean "the budget never refuses anything", A's own third ask is driven
// to a refusal in the same test.
func TestBudget_AFollowerChargesNothingToTheCanonicalsSession(t *testing.T) {
	objs := budgetWorld(
		[]budgetSession{{budSessA, budUIDA}, {budSessB, budUIDB}, {budSessC, budUIDC}},
		requestFor(budReqA1, budSessA, budUIDA, mcpName),
		requestFor(budReqA2, budSessA, budUIDA, mcpName),
		requestFor(budReqA3, budSessA, budUIDA, mcpName),
		requestFor(budReqB1, budSessB, budUIDB, mcpName),
		requestFor(budReqC1, budSessC, budUIDC, mcpName),
	)
	probes := rejectingProbe(t)
	c, r, _ := newReconciler(t, objs...)

	// One card, two other sessions riding on it.
	reconcileNamed(t, r, budReqA1)
	require.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen, getNamed(t, c, budReqA1).Status.Phase,
		"positive control: session A really holds the card")
	for _, name := range []string{budReqB1, budReqC1} {
		reconcileNamed(t, r, name)
		require.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseCollapsed, getNamed(t, c, name).Status.Phase,
			"positive control: %s must really be a follower of A's card", name)
	}
	require.Equal(t, int32(1), probes.Load(), "positive control: three sessions, one determination")

	// The card expires and every follower settles with it.
	expireCanonical(t, c, r, budReqA1)
	for _, f := range []struct{ request, session string }{
		{budReqB1, budSessB}, {budReqC1, budSessC},
	} {
		reconcileNamed(t, r, f.request)
		settled := getNamed(t, c, f.request)
		require.True(t, spiceboxv1alpha1.IsCredentialUpdateRequestTerminal(settled.Status.Phase),
			"positive control: %s must really have settled terminal; status=%+v", f.request, settled.Status)
		requireCountableAsk(t, settled, f.session)
	}
	// Stated explicitly, because it is the arithmetic the claim rests on: two
	// followers plus session A's own spent ask is three. If followers charged
	// the canonical's session, A would already be over the limit of two.
	require.Equal(t, 2, credentialupdaterequest.BudgetLimitForTest,
		"this test's arithmetic assumes a limit of two asks per (session, credential)")

	// The claim: A's own budget saw one ask, not three.
	reconcileNamed(t, r, budReqA2)
	secondForA := getNamed(t, c, budReqA2)
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen, secondForA.Status.Phase,
		"the canonical's session must be charged for its OWN ask only -- not for every session that rode its "+
			"card; status=%+v", secondForA.Status)
	assert.Nil(t, secondForA.Status.CollapsedInto)
	assert.Equal(t, int32(2), probes.Load(), "A's second ask ran its own determination")

	// Control: A's budget is not simply unbounded. Two of its OWN asks exhaust
	// it, exactly as they would for any other session.
	expireCanonical(t, c, r, budReqA2)
	reconcileNamed(t, r, budReqA3)
	assertRefusedForBudget(t, getNamed(t, c, budReqA3))
	assert.Equal(t, int32(2), probes.Load(), "the refusal precedes the probe")
}

// TestBudget_AlternatingCanonicalAndFollowerRolesStillExhaustBothSessions is
// the anti-evasion property, and the reason an ask is spent when it is
// DETERMINED rather than when it settles.
//
// Two sessions on one shared credential can take turns: whoever reconciles
// first holds the card, the other follows; next round they swap. Each session
// therefore accumulates a mix of its own cards and rides on the other's. The
// budget has to survive that mix, because if either kind of ask is free the two
// agents can trade roles indefinitely and neither ever runs out -- an unbounded
// stream of credential prompts assembled entirely out of individually-legal
// steps.
//
// The trap this pins is specifically the IN-FLIGHT ask. Round 2 is deliberately
// left unsettled: session A's second ask is sitting in Collapsed and session
// B's is sitting in Open, neither terminal. Counting only terminal asks makes
// both of those free, so both sessions' third ask sails through -- and so would
// the fourth, and the fifth, for as long as the two keep trading. Every round
// here is driven through the real reconciler; nothing is patched into place.
func TestBudget_AlternatingCanonicalAndFollowerRolesStillExhaustBothSessions(t *testing.T) {
	objs := budgetWorld(
		[]budgetSession{{budSessA, budUIDA}, {budSessB, budUIDB}, {budSessC, budUIDC}},
		requestFor(budReqA1, budSessA, budUIDA, mcpName),
		requestFor(budReqA2, budSessA, budUIDA, mcpName),
		requestFor(budReqA3, budSessA, budUIDA, mcpName),
		requestFor(budReqB1, budSessB, budUIDB, mcpName),
		requestFor(budReqB2, budSessB, budUIDB, mcpName),
		requestFor(budReqB3, budSessB, budUIDB, mcpName),
		requestFor(budReqC1, budSessC, budUIDC, mcpName),
	)
	probes := rejectingProbe(t)
	c, r, _ := newReconciler(t, objs...)

	// --- Round 1: A holds the card, B follows. --------------------------------
	reconcileNamed(t, r, budReqA1)
	reconcileNamed(t, r, budReqB1)
	roundOneCard, roundOneFollower := getNamed(t, c, budReqA1), getNamed(t, c, budReqB1)
	require.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen, roundOneCard.Status.Phase,
		"positive control: A holds round 1's card; status=%+v", roundOneCard.Status)
	require.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseCollapsed, roundOneFollower.Status.Phase,
		"positive control: B follows in round 1; status=%+v", roundOneFollower.Status)
	require.NotNil(t, roundOneFollower.Status.CollapsedInto)
	require.Equal(t, budReqA1, roundOneFollower.Status.CollapsedInto.Name)
	require.Equal(t, int32(1), probes.Load(), "positive control: round 1 is ONE card at ONE human")

	// Round 1 settles: the card expires and the follower settles with it, so
	// each session now carries exactly one terminal ask.
	expireCanonical(t, c, r, budReqA1)
	reconcileNamed(t, r, budReqB1)
	for _, f := range []struct{ request, session string }{
		{budReqA1, budSessA}, {budReqB1, budSessB},
	} {
		settled := getNamed(t, c, f.request)
		require.True(t, spiceboxv1alpha1.IsCredentialUpdateRequestTerminal(settled.Status.Phase),
			"positive control: round 1's %s must really have settled; status=%+v", f.request, settled.Status)
		requireCountableAsk(t, settled, f.session)
	}

	// --- Round 2: the roles TRADE. B holds the card, A follows. ---------------
	// Deliberately NOT settled. This is the state the evasion lives in.
	reconcileNamed(t, r, budReqB2)
	reconcileNamed(t, r, budReqA2)
	roundTwoCard, roundTwoFollower := getNamed(t, c, budReqB2), getNamed(t, c, budReqA2)
	require.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen, roundTwoCard.Status.Phase,
		"positive control: the roles must really have traded -- B holds round 2's card; status=%+v",
		roundTwoCard.Status)
	require.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseCollapsed, roundTwoFollower.Status.Phase,
		"positive control: A follows in round 2; status=%+v", roundTwoFollower.Status)
	require.NotNil(t, roundTwoFollower.Status.CollapsedInto)
	require.Equal(t, budReqB2, roundTwoFollower.Status.CollapsedInto.Name,
		"positive control: A really is riding B's card now")
	require.Equal(t, int32(2), probes.Load(), "positive control: round 2 is a second real card")
	// The trap, stated: neither of round 2's asks is terminal.
	require.False(t, spiceboxv1alpha1.IsCredentialUpdateRequestTerminal(roundTwoCard.Status.Phase),
		"the property under test is about IN-FLIGHT asks: B's round-2 card must still be unsettled")
	require.False(t, spiceboxv1alpha1.IsCredentialUpdateRequestTerminal(roundTwoFollower.Status.Phase),
		"the property under test is about IN-FLIGHT asks: A's round-2 ride must still be unsettled")

	// --- Round 3: both are out. ----------------------------------------------
	// A has spent one settled ask and one in-flight ride; B has spent one
	// settled ask and one live card. Two each, so neither gets a third.
	reconcileNamed(t, r, budReqA3)
	assertRefusedForBudget(t, getNamed(t, c, budReqA3))
	reconcileNamed(t, r, budReqB3)
	assertRefusedForBudget(t, getNamed(t, c, budReqB3))
	assert.Equal(t, int32(2), probes.Load(),
		"neither refusal ran a determination: the budget is checked before Determine")

	// Positive control, and the one that stops both refusals above from being
	// vacuous: a session that has spent nothing, asking about the same
	// credential at the same instant, is still accepted -- it folds onto B's
	// live card. So the collapse path is demonstrably still working right here,
	// and A's and B's refusals are about their own exhausted budgets rather
	// than about the world having stopped accepting asks.
	reconcileNamed(t, r, budReqC1)
	fresh := getNamed(t, c, budReqC1)
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseCollapsed, fresh.Status.Phase,
		"a session with an unspent budget is still accepted onto the live card; status=%+v", fresh.Status)
	require.NotNil(t, fresh.Status.CollapsedInto)
	assert.Equal(t, budReqB2, fresh.Status.CollapsedInto.Name)
	assert.Equal(t, int32(2), probes.Load(), "and it collapsed rather than opening a third card")
}

// TestBudget_AnExhaustedSessionIsRefusedNotCollapsed pins the ORDERING: the
// budget check runs before the collapse check, and must stay there.
//
// Swapped, a session that has already had its two answers would be allowed to
// attach to whatever card happens to be live instead of being turned away --
// which is how a session with no budget left keeps receiving credential answers
// indefinitely, one borrowed card at a time. The refusal is also the only thing
// that ever tells such an agent to stop; a collapse tells it to keep waiting.
//
// The positive control is a third session reconciled at the same moment against
// the same live card: it collapses. So the card really is absorbing asks right
// then, and the exhausted session's refusal is the ordering rather than a
// collapse path that happened to be unavailable.
func TestBudget_AnExhaustedSessionIsRefusedNotCollapsed(t *testing.T) {
	objs := budgetWorld(
		[]budgetSession{{budSessA, budUIDA}, {budSessB, budUIDB}, {budSessC, budUIDC}},
		requestFor(budReqA1, budSessA, budUIDA, mcpName),
		// Session B has already had both of its answers about this credential.
		spentAskFor("demo-cur-b-prior-1", budSessB, budUIDB),
		spentAskFor("demo-cur-b-prior-2", budSessB, budUIDB),
		requestFor(budReqB1, budSessB, budUIDB, mcpName),
		requestFor(budReqC1, budSessC, budUIDC, mcpName),
	)
	probes := rejectingProbe(t)
	c, r, _ := newReconciler(t, objs...)

	reconcileNamed(t, r, budReqA1)
	card := getNamed(t, c, budReqA1)
	require.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen, card.Status.Phase,
		"positive control: a live card for this credential must exist, or 'refused rather than collapsed' "+
			"proves nothing; status=%+v", card.Status)
	require.NotNil(t, card.Status.ResolvedCredential)
	require.Equal(t, sharedCredentialRef(), credentialKey(card.Status.ResolvedCredential),
		"positive control: the live card is for the very credential B is exhausted on")
	probesAfterCard := probes.Load()
	require.Equal(t, int32(1), probesAfterCard)

	// Both of B's priors must be countable, or B is not actually exhausted and
	// the refusal below would be measuring nothing.
	for _, name := range []string{"demo-cur-b-prior-1", "demo-cur-b-prior-2"} {
		requireCountableAsk(t, getNamed(t, c, name), budSessB)
	}

	reconcileNamed(t, r, budReqB1)
	assertRefusedForBudget(t, getNamed(t, c, budReqB1))
	assert.Equal(t, probesAfterCard, probes.Load(),
		"an over-budget refusal must not probe: the budget is checked before Determine")

	// Positive control: the live card is absorbing asks at this exact moment.
	reconcileNamed(t, r, budReqC1)
	rider := getNamed(t, c, budReqC1)
	require.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseCollapsed, rider.Status.Phase,
		"control: a session with budget left DOES collapse onto this card, so B's refusal is the ordering "+
			"and not an unavailable collapse; status=%+v", rider.Status)
	require.NotNil(t, rider.Status.CollapsedInto)
	assert.Equal(t, budReqA1, rider.Status.CollapsedInto.Name)
	assert.Equal(t, probesAfterCard, probes.Load(), "a collapse never probes either")
}

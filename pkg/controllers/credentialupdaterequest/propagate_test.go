// pkg/controllers/credentialupdaterequest/propagate_test.go
//
// Collapsing is only half a feature. A follower publishes nothing, probes
// nothing, and carries no deadline of its own, so if the canonical's outcome
// never reaches it, NOTHING ever moves it: its agent blocks its whole wait
// window and then reports a timeout for a credential a human really did
// replace. That is this slice's failure mode 1, and every test in this file
// exists to make it unrepresentable.
//
// Built on collapse_test.go's fixtures (sharedWorld, agentModeSession,
// requestFor, rejectingProbe, reconcileNamed, getNamed) -- same package, same
// file-scoped helpers, and the same reason for an AgentIdentity rather than a
// SessionUserIdentity: a shared identity is the only shape where several
// sessions genuinely resolve ONE credential.
package credentialupdaterequest_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/credentialupdaterequest"
)

// canonicalExpiryReason is the wording slice 1 gives a DELIVERED request whose
// window ran out. Restated here (rather than imported) on purpose: this file's
// honesty assertions are about a follower never emitting this sentence, so the
// test must fail if the reconciler's copy drifts away from it -- a shared
// constant would silently keep both sides in agreement while the guard stopped
// guarding anything.
const canonicalExpiryReason = "Nobody updated the credential before the wait window elapsed."

// credUpdateFixture is one session in the shared-credential world: its
// AgentSession, its CredentialUpdateRequest, and the UID that ties the two
// together through the request's ownerReference (resolveIdentity's
// ownedBySession check compares exactly that UID).
type credUpdateFixture struct {
	session string
	request string
	uid     types.UID
}

// fixtureSessions names count sessions in reconcile order. The names are
// ordered so that the first one is also lexicographically smallest, which
// makes the canonical election's tie-break agree with arrival order -- the
// fixture then proves propagation rather than accidentally testing the
// tie-break.
func fixtureSessions(t *testing.T, count int) []credUpdateFixture {
	t.Helper()
	letters := []string{"a", "b", "c", "d"}
	require.LessOrEqual(t, count, len(letters), "fixtureSessions: add more letters")
	out := make([]credUpdateFixture, 0, count)
	for i := 0; i < count; i++ {
		out = append(out, credUpdateFixture{
			session: "demo-session-" + letters[i],
			request: "demo-cur-" + letters[i],
			uid:     types.UID("demo-session-" + letters[i] + "-uid"),
		})
	}
	return out
}

// oneCardManyFollowers builds count sessions sharing ONE dead credential and
// reconciles them in order, so fixtures[0] holds the card and the rest are its
// followers. extra objects are seeded into the world before any reconcile, for
// tests that need a session to arrive carrying history.
//
// The returned probe counter is the positive control every test here leans on.
// It must read exactly 1 on return: one full determination ran, which means a
// follower that later reaches a terminal phase got there from the CANONICAL'S
// answer and not by quietly re-deciding for itself. A follower that re-decides
// is the mutation this file is built to catch, and it always shows up here as
// the counter climbing.
func oneCardManyFollowers(t *testing.T, count int, extra ...client.Object) (
	client.Client, *credentialupdaterequest.Reconciler, *atomic.Int32, []credUpdateFixture) {
	t.Helper()
	require.GreaterOrEqual(t, count, 2, "oneCardManyFollowers needs a canonical and at least one follower")

	probes := rejectingProbe(t)
	fixtures := fixtureSessions(t, count)

	objs := sharedWorld()
	for _, f := range fixtures {
		objs = append(objs, agentModeSession(f.session, f.uid), requestFor(f.request, f.session, f.uid, mcpName))
	}
	objs = append(objs, extra...)
	c, r, _ := newReconciler(t, objs...)

	for _, f := range fixtures {
		reconcileNamed(t, r, f.request)
	}

	canonical := getNamed(t, c, fixtures[0].request)
	require.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen, canonical.Status.Phase,
		"positive control: the fixture must really produce ONE card; status=%+v", canonical.Status)
	for _, f := range fixtures[1:] {
		follower := getNamed(t, c, f.request)
		require.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseCollapsed, follower.Status.Phase,
			"positive control: %s must be a follower; status=%+v", f.request, follower.Status)
		require.NotNil(t, follower.Status.CollapsedInto)
		require.Equal(t, fixtures[0].request, follower.Status.CollapsedInto.Name)
		require.False(t, spiceboxv1alpha1.IsCredentialUpdateRequestTerminal(follower.Status.Phase),
			"positive control: a follower starts NON-terminal, so reaching terminal later is a real transition")
	}
	require.Equal(t, int32(1), probes.Load(),
		"positive control: exactly one full determination ran -- one card, one probe, %d sessions", count)

	return c, r, probes, fixtures
}

// replaceSharedCredential simulates the human the card went to pasting a new
// value into the shared Secret.
func replaceSharedCredential(t *testing.T, c client.Client) {
	t.Helper()
	var sec corev1.Secret
	key := types.NamespacedName{Namespace: ns, Name: sharedSecretName}
	require.NoError(t, c.Get(context.Background(), key, &sec), "Get shared Secret")
	sec.Data[credName] = []byte("tok-pasted-by-a-human")
	require.NoError(t, c.Update(context.Background(), &sec), "Update shared Secret")
}

// patchCanonicalStatus applies mutate to the named request's status. Used for
// the outcomes this reconciler does not itself produce from Open (a refusal at
// click time is slice 4's later task), so the propagation contract is pinned
// for whatever eventually writes them rather than left untested until then.
func patchCanonicalStatus(t *testing.T, c client.Client, name string,
	mutate func(cr *spiceboxv1alpha1.CredentialUpdateRequest)) {
	t.Helper()
	cur := getNamed(t, c, name)
	prior := cur.DeepCopy()
	mutate(cur)
	require.NoError(t, c.Status().Patch(context.Background(), cur, client.MergeFrom(prior)), "patch %s status", name)
}

// expireCanonical drives the canonical to Expired through the REAL path --
// reconcileOpen finding the idle deadline elapsed -- rather than by patching
// the phase in, so the reason a follower must not copy is the reason production
// would actually write.
//
// The clock skew is RESTORED on return, and that is load-bearing rather than
// tidiness. Leaving `r.Now` two hours ahead with a one-minute TTL puts every
// LATER Open request in the same test permanently past its deadline, so the
// next reconcile of any of them silently expires it. Nothing catches that today
// only because each Open phase in these tests is read immediately after the
// write that set it; inserting a single re-reconcile between the two -- the
// most ordinary edit imaginable -- would flip several assertions from Open to
// Expired for reasons having nothing to do with the property under test.
// Deferred rather than t.Cleanup'd on purpose: t.Cleanup restores at test END,
// which is exactly too late to help the rest of the test body.
func expireCanonical(t *testing.T, c client.Client, r *credentialupdaterequest.Reconciler, name string) {
	t.Helper()
	priorTTL, priorNow := r.IdleTTL, r.Now
	defer func() { r.IdleTTL, r.Now = priorTTL, priorNow }()
	r.IdleTTL = time.Minute
	r.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	reconcileNamed(t, r, name)
	got := getNamed(t, c, name)
	require.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseExpired, got.Status.Phase,
		"positive control: the canonical must really expire; status=%+v", got.Status)
}

// TestReconcile_CanonicalFulfilledSettlesEveryFollower is the headline claim:
// one human pastes one credential once, and every session riding on that card
// is released.
//
// It also pins CONVERGENCE, by settling the followers one at a time: the
// follower nothing has touched yet must still be sitting in Collapsed with its
// pointer intact, not corrupted or half-written, so a propagation pass that
// reaches only some followers finishes on the next pass rather than stranding
// the rest.
func TestReconcile_CanonicalFulfilledSettlesEveryFollower(t *testing.T) {
	c, r, probes, fx := oneCardManyFollowers(t, 3)
	probesAfterCard := probes.Load()

	replaceSharedCredential(t, c)
	reconcileNamed(t, r, fx[0].request)
	canonical := getNamed(t, c, fx[0].request)
	require.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseFulfilled, canonical.Status.Phase,
		"positive control: a real Secret edit must really fulfil the canonical; status=%+v", canonical.Status)

	// First follower only. The second is deliberately left alone.
	reconcileNamed(t, r, fx[1].request)
	first := getNamed(t, c, fx[1].request)
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseFulfilled, first.Status.Phase,
		"a fulfilled card must fulfil its followers; status=%+v", first.Status)

	untouched := getNamed(t, c, fx[2].request)
	require.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseCollapsed, untouched.Status.Phase,
		"convergence: a follower nothing has reconciled yet stays cleanly Collapsed, ready to settle on its next pass")
	require.NotNil(t, untouched.Status.CollapsedInto,
		"convergence: its pointer must survive the other follower's settlement -- one follower's write must not "+
			"disturb another's state")

	reconcileNamed(t, r, fx[2].request)
	second := getNamed(t, c, fx[2].request)
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseFulfilled, second.Status.Phase,
		"the second pass settles the rest; status=%+v", second.Status)

	for _, follower := range []*spiceboxv1alpha1.CredentialUpdateRequest{first, second} {
		assert.True(t, spiceboxv1alpha1.IsCredentialUpdateRequestTerminal(follower.Status.Phase),
			"%s must reach a phase the blocked meta tool may stop on", follower.Name)
		assert.NotEqual(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen, follower.Status.Phase,
			"%s must never open a card of its own after the shared ask was already answered -- that asks a "+
				"second human and blocks this agent for a second full window", follower.Name)
		assert.Equal(t, canonical.Status.Determination, follower.Status.Determination,
			"%s must carry the canonical's determination, not a stand-in", follower.Name)
		assert.NotEmpty(t, follower.Status.Reason,
			"no-silent-errors: %s must say why it settled", follower.Name)
		assert.Contains(t, follower.Status.Reason, "Retry your call",
			"%s's agent must be told the credential is usable again", follower.Name)
		assert.Empty(t, follower.Status.InteractionRef,
			"%s still has no card of its own; settling must not retro-stamp delivery", follower.Name)
		require.NotNil(t, follower.Status.CollapsedInto,
			"%s must keep the record of whose answer it took", follower.Name)
		assert.Equal(t, fx[0].request, follower.Status.CollapsedInto.Name)
	}

	assert.Equal(t, probesAfterCard, probes.Load(),
		"a follower settles from the canonical's answer -- it must not re-run the determination, which would "+
			"mean N probes and (with the credential still looking dead to a stale probe) N new cards")
}

// TestReconcile_CanonicalRefusedSettlesFollowersWithTheSameDetermination pins
// that a follower learns the REAL reason. A refusal is the outcome where a
// generic hand-off costs the most: "the request you were waiting on finished"
// tells an agent nothing, while the canonical's own determination tells it
// whether to retry, escalate, or stop asking.
func TestReconcile_CanonicalRefusedSettlesFollowersWithTheSameDetermination(t *testing.T) {
	c, r, probes, fx := oneCardManyFollowers(t, 2)
	probesAfterCard := probes.Load()

	before := getNamed(t, c, fx[1].request)
	require.Equal(t, spiceboxv1alpha1.CredentialUpdateDeterminationCollapsed, before.Status.Determination,
		"positive control: a follower starts on the Collapsed determination, so matching the canonical's later "+
			"is a real change rather than a value both happened to share")

	const refusalReason = "This credential is managed by the platform and cannot be replaced by hand."
	patchCanonicalStatus(t, c, fx[0].request, func(cr *spiceboxv1alpha1.CredentialUpdateRequest) {
		cr.Status.Phase = spiceboxv1alpha1.CredentialUpdateRequestPhaseRefused
		cr.Status.Determination = spiceboxv1alpha1.CredentialUpdateDeterminationNotUpdatable
		cr.Status.Reason = refusalReason
	})
	canonical := getNamed(t, c, fx[0].request)
	require.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseRefused, canonical.Status.Phase,
		"positive control: the canonical really is Refused before the follower reconciles")

	reconcileNamed(t, r, fx[1].request)
	follower := getNamed(t, c, fx[1].request)

	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseRefused, follower.Status.Phase,
		"a refused card refuses its followers; status=%+v", follower.Status)
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateDeterminationNotUpdatable, follower.Status.Determination,
		"the follower must carry the canonical's determination verbatim -- the meta tool surfaces it, and a "+
			"generic one would hide whether this is worth retrying")
	assert.Contains(t, follower.Status.Reason, refusalReason,
		"the canonical's platform-authored reason is the real 'why' and must reach this agent intact")
	assert.Contains(t, follower.Status.Reason, "No card was ever shown for this request",
		"the follower must still say the decision was not made on a card shown to IT")
	assert.Empty(t, follower.Status.InteractionRef, "settling must not retro-stamp delivery")
	assert.Equal(t, probesAfterCard, probes.Load(), "a follower must not re-probe to learn an answer it was given")
}

// TestReconcile_CanonicalExpiredExpiresFollowersWithoutBlamingAnybody is the
// honesty invariant, in the place it is easiest to break: expiry is the only
// outcome whose natural wording is an accusation.
//
// A follower was never shown a card. Copying the canonical's "Nobody updated
// the credential before the wait window elapsed" onto it produces exactly the
// defect this project has already shipped twice -- the system reporting that a
// human ignored something they were never given. Both rows therefore assert on
// what the reason must NOT contain as well as what it must.
func TestReconcile_CanonicalExpiredExpiresFollowersWithoutBlamingAnybody(t *testing.T) {
	cases := []struct {
		name string
		// deliverCanonical stamps the canonical's interactionRef, which is what
		// channelsd does when it really publishes -- so the canonical's own
		// expiry takes the "a human WAS asked" wording.
		deliverCanonical bool
		wantContains     []string
	}{
		{
			name:             "canonical's card WAS shown to a human: the follower still says no card was shown for IT",
			deliverCanonical: true,
			wantContains: []string{
				"No card was ever shown for this request",
				"wait window elapsed without the credential being replaced",
			},
		},
		{
			name:             "canonical was never delivered either: the follower says nobody was ever asked at all",
			deliverCanonical: false,
			wantContains: []string{
				"No card was ever shown for this request",
				"nobody was ever asked",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, r, probes, fx := oneCardManyFollowers(t, 2)
			probesAfterCard := probes.Load()

			if tc.deliverCanonical {
				patchCanonicalStatus(t, c, fx[0].request, func(cr *spiceboxv1alpha1.CredentialUpdateRequest) {
					cr.Status.InteractionRef = "req-fake-delivered"
				})
			}
			expireCanonical(t, c, r, fx[0].request)
			canonical := getNamed(t, c, fx[0].request)

			// Positive control, and the one that makes the NotContains assertion
			// below mean something: the sentence a follower must never emit is a
			// sentence this fixture demonstrably produces (or demonstrably does
			// not, when nobody was ever asked).
			if tc.deliverCanonical {
				require.Equal(t, canonicalExpiryReason, canonical.Status.Reason,
					"control: a DELIVERED canonical really does expire with the blame wording, so finding it "+
						"absent from the follower is a fact about the follower")
			} else {
				require.Contains(t, canonical.Status.Reason, "never delivered",
					"control: an undelivered canonical expires with slice 1's never-delivered wording")
			}

			reconcileNamed(t, r, fx[1].request)
			follower := getNamed(t, c, fx[1].request)

			assert.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseExpired, follower.Status.Phase,
				"an expired card expires its followers; status=%+v", follower.Status)
			assert.NotEqual(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen, follower.Status.Phase,
				"a follower must NEVER answer an expired shared ask by opening its own card: that asks a second "+
					"human about a credential the first was already asked about, and blocks this agent for a "+
					"second full wait window")
			assert.Equal(t, canonical.Status.Determination, follower.Status.Determination,
				"the follower carries the canonical's determination -- the verdict that justified the card is "+
					"unchanged by nobody having acted on it")

			for _, want := range tc.wantContains {
				assert.Contains(t, follower.Status.Reason, want,
					"the follower's expiry reason must state ITS OWN situation; reason=%q", follower.Status.Reason)
			}
			assert.NotEqual(t, canonical.Status.Reason, follower.Status.Reason,
				"the canonical's reason is written from the point of view of a request that HAD a card; "+
					"copying it verbatim is the honesty bug this asserts against")
			assert.NotContains(t, follower.Status.Reason, canonicalExpiryReason,
				"a follower may never say a human failed to update the credential -- no human was ever shown "+
					"anything on this request's behalf; reason=%q", follower.Status.Reason)
			assert.Empty(t, follower.Status.InteractionRef,
				"settling must not retro-stamp delivery onto a request that was never delivered")
			assert.Equal(t, probesAfterCard, probes.Load(), "a follower must not re-probe to learn it expired")
		})
	}
}

// TestFollowerReason_ScopesEveryOutcomeToARequestThatHadNoCard asserts the
// honesty invariant directly against the renderer, over the whole surface --
// including inputs no fixture in this file reaches (a canonical whose refusal
// reason is empty, a terminal phase added in some later slice).
//
// The end-to-end tests above prove the wording on the paths production takes
// today; this proves the property holds everywhere, so a branch added to
// followerReason later cannot reintroduce the accusation through a combination
// nothing happens to exercise.
//
// Note what is NOT asserted: that a follower never echoes the canonical's
// reason. For a REFUSAL that echo is the requirement -- the canonical's reason
// is the real, platform-authored "why", and replacing it with a stand-in would
// report to the agent something no human ever decided. The invariant is about
// the sentence a follower AUTHORS, and about never claiming a human ignored a
// card this request never had.
func TestFollowerReason_ScopesEveryOutcomeToARequestThatHadNoCard(t *testing.T) {
	const noCardClause = "No card was ever shown for this request"
	const refusalReason = "This credential is managed by the platform and cannot be replaced by hand."
	const neverDeliveredReason = "The credential-update request was never delivered to a human " +
		"(no card was ever shown) before the wait window elapsed."

	cases := []struct {
		name            string
		phase           string
		canonicalReason string
		delivered       bool
		wantContains    []string
	}{
		{
			name:  "Fulfilled: tells the agent to retry, and claims nothing about who was asked",
			phase: spiceboxv1alpha1.CredentialUpdateRequestPhaseFulfilled,
			// Deliberately NOT the canonical's own production wording ("The
			// credential was updated. Retry your call." -- reconcileOpen). That
			// sentence contains this row's whole assertion verbatim, so a
			// followerReason that simply RETURNED the canonical's reason on this
			// branch satisfied the row and the file's headline invariant went
			// unpinned on the commonest settlement path. Saying the same thing in
			// different words makes finding "Retry your call" on the follower a
			// fact about what followerReason AUTHORS.
			canonicalReason: "The credential has been replaced; the blocked call may go again.",
			wantContains:    []string{"Retry your call"},
		},
		{
			name:            "Refused: carries the real why, scoped to a request that had no card",
			phase:           spiceboxv1alpha1.CredentialUpdateRequestPhaseRefused,
			canonicalReason: refusalReason,
			delivered:       true,
			wantContains:    []string{refusalReason, noCardClause},
		},
		{
			name:  "Refused with an empty canonical reason: still says something rather than nothing",
			phase: spiceboxv1alpha1.CredentialUpdateRequestPhaseRefused,
			// no-silent-errors: an agent told only "refused" with no text at all
			// has nothing to act on and nothing to report.
			canonicalReason: "",
			wantContains:    []string{noCardClause, "was refused"},
		},
		{
			name:            "Expired, canonical WAS delivered: the blame sentence is right there to copy, and must not be",
			phase:           spiceboxv1alpha1.CredentialUpdateRequestPhaseExpired,
			canonicalReason: canonicalExpiryReason,
			delivered:       true,
			wantContains:    []string{noCardClause, "without the credential being replaced"},
		},
		{
			name:            "Expired, canonical never delivered either: says plainly that nobody was ever asked",
			phase:           spiceboxv1alpha1.CredentialUpdateRequestPhaseExpired,
			canonicalReason: neverDeliveredReason,
			wantContains:    []string{noCardClause, "nobody was ever asked"},
		},
		{
			name:  "an unrecognized terminal phase degrades to a vague TRUE sentence, never an accusation",
			phase: "SomeFutureTerminalPhase",
			// Worst case on purpose: the blame wording sitting in reach of a
			// future branch that reaches for the canonical's text by default.
			canonicalReason: canonicalExpiryReason,
			wantContains:    []string{noCardClause},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			canonical := &spiceboxv1alpha1.CredentialUpdateRequest{}
			canonical.Status.Phase = tc.phase
			canonical.Status.Reason = tc.canonicalReason
			if tc.delivered {
				canonical.Status.InteractionRef = "req-fake-delivered"
			}

			got := credentialupdaterequest.FollowerReasonForTest(canonical)

			require.NotEmpty(t, got, "no-silent-errors: every phase must produce a reason the agent can read")
			for _, want := range tc.wantContains {
				assert.Contains(t, got, want, "got=%q", got)
			}
			assert.NotContains(t, got, canonicalExpiryReason,
				"no follower may assert a human failed to act on a card it never had; got=%q", got)
			// The file's headline invariant, asserted on EVERY row: propagation
			// copies phase and determination verbatim, but the REASON is the one
			// thing it may not. A branch that returned the canonical's reason
			// unchanged reddens here even when the row's wantContains happens to
			// be satisfiable from the canonical's own text. Refused is no
			// exception -- it CARRIES the canonical's reason, but appends the
			// no-card scoping, so it is never byte-identical either.
			assert.NotEqual(t, tc.canonicalReason, got,
				"a follower's reason is authored for a request that had no card, never the canonical's verbatim; got=%q", got)
		})
	}
}

// TestMapCanonicalToFollowers pins the enqueue itself -- the half of
// propagation no end-to-end assertion can see. Every test above reconciles the
// follower by hand, which proves the Collapsed arm settles it but says nothing
// about whether production would ever have woken it. Gutting the map func
// leaves all of them green and this one red.
func TestMapCanonicalToFollowers(t *testing.T) {
	canonicalRef := &spiceboxv1alpha1.NamespacedRef{Namespace: ns, Name: curAName}

	// A collapsed follower of the canonical -- the thing that must be enqueued.
	follower := requestFor(curBName, sessionBName, sessionBUID, mcpName)
	follower.Status = spiceboxv1alpha1.CredentialUpdateRequestStatus{
		Phase:         spiceboxv1alpha1.CredentialUpdateRequestPhaseCollapsed,
		Determination: spiceboxv1alpha1.CredentialUpdateDeterminationCollapsed,
		CollapsedInto: canonicalRef.DeepCopy(),
	}
	// A follower of a DIFFERENT canonical: waking it on this canonical's change
	// would be wasted work at best, and at worst hides a missing enqueue for the
	// canonical it actually waits on.
	otherFollower := requestFor("demo-cur-other-follower", sessionBName, sessionBUID, mcpName)
	otherFollower.Status = spiceboxv1alpha1.CredentialUpdateRequestStatus{
		Phase:         spiceboxv1alpha1.CredentialUpdateRequestPhaseCollapsed,
		CollapsedInto: &spiceboxv1alpha1.NamespacedRef{Namespace: ns, Name: "demo-cur-somebody-else"},
	}
	// An already-settled follower: it keeps collapsedInto as the durable record
	// of whose answer it took, so matching on that pointer alone would re-enqueue
	// it forever for a reconcile that can only no-op.
	settledFollower := requestFor("demo-cur-settled-follower", sessionBName, sessionBUID, mcpName)
	settledFollower.Status = spiceboxv1alpha1.CredentialUpdateRequestStatus{
		Phase:         spiceboxv1alpha1.CredentialUpdateRequestPhaseFulfilled,
		CollapsedInto: canonicalRef.DeepCopy(),
	}
	// An unrelated Open request: not a follower of anything.
	unrelated := requestFor("demo-cur-unrelated", sessionBName, sessionBUID, mcpName)
	unrelated.Status = spiceboxv1alpha1.CredentialUpdateRequestStatus{
		Phase: spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen,
	}

	canonical := requestFor(curAName, sessionAName, sessionAUID, mcpName)
	canonical.Status = spiceboxv1alpha1.CredentialUpdateRequestStatus{
		Phase: spiceboxv1alpha1.CredentialUpdateRequestPhaseFulfilled,
	}

	objs := sharedWorld()
	objs = append(objs, canonical, follower, otherFollower, settledFollower, unrelated)
	_, r, _ := newReconciler(t, objs...)

	got := r.MapCanonicalToFollowersForTest(context.Background(), canonical)

	require.Len(t, got, 1, "exactly the still-waiting followers of THIS canonical must be enqueued; got=%v", got)
	assert.Equal(t, types.NamespacedName{Namespace: ns, Name: curBName}, got[0].NamespacedName)
}

// TestMapCanonicalToFollowers_WrongObjectTypeEnqueuesNothing keeps the map func
// fail-closed on an object it was never meant to receive, rather than panicking
// inside a shared informer handler.
func TestMapCanonicalToFollowers_WrongObjectTypeEnqueuesNothing(t *testing.T) {
	_, r, _ := newReconciler(t)
	assert.Nil(t, r.MapCanonicalToFollowersForTest(context.Background(), &spiceboxv1alpha1.AgentSession{}))
}

// TestMapCanonicalToFollowers_DeletedCanonicalStillWakesItsFollowers covers the
// case a phase filter on the canonical would have missed. When a canonical is
// GC'd with its session, the handler is invoked with the object as it last was
// -- and its followers are precisely the requests that MUST be woken, because
// they are now pointing at a name that no longer resolves and have to
// re-determine their own asks.
func TestMapCanonicalToFollowers_DeletedCanonicalStillWakesItsFollowers(t *testing.T) {
	canonical := requestFor(curAName, sessionAName, sessionAUID, mcpName)
	canonical.Status.Phase = spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen

	follower := requestFor(curBName, sessionBName, sessionBUID, mcpName)
	follower.Status = spiceboxv1alpha1.CredentialUpdateRequestStatus{
		Phase:         spiceboxv1alpha1.CredentialUpdateRequestPhaseCollapsed,
		CollapsedInto: &spiceboxv1alpha1.NamespacedRef{Namespace: ns, Name: curAName},
	}

	objs := sharedWorld()
	// The canonical is deliberately NOT in the client: it has already been
	// deleted, exactly as the informer's delete event describes it.
	objs = append(objs, follower)
	_, r, _ := newReconciler(t, objs...)

	got := r.MapCanonicalToFollowersForTest(context.Background(), canonical)

	require.Len(t, got, 1, "a deleted canonical's followers must still be woken; got=%v", got)
	assert.Equal(t, curBName, got[0].Name)
}

// TestReconcile_AFollowerLostMidFlightDoesNotBlockTheOthers pins that
// propagation is per-follower and depends on nothing outside the two requests
// involved. Sessions end at arbitrary moments -- a user closes a thread, an
// idle reaper fires -- and one session disappearing while a card is settling
// must not leave its peers waiting.
//
// Two ways to lose a follower are covered at once, because they fail
// differently: session gone but request still present (owner-ref GC has not run
// yet) would break an implementation that read the follower's session, and both
// gone would break the enqueue if it assumed every listed name still resolves.
func TestReconcile_AFollowerLostMidFlightDoesNotBlockTheOthers(t *testing.T) {
	c, r, _, fx := oneCardManyFollowers(t, 4)
	ctx := context.Background()
	sessionGone, bothGone, survivor := fx[1], fx[2], fx[3]

	require.NoError(t, c.Delete(ctx, &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1Object(sessionGone.session)}), "delete the first follower's session")
	require.NoError(t, c.Delete(ctx, &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1Object(bothGone.session)}), "delete the second follower's session")
	require.NoError(t, c.Delete(ctx, &spiceboxv1alpha1.CredentialUpdateRequest{
		ObjectMeta: metav1Object(bothGone.request)}), "owner-ref GC takes its request too")

	expireCanonical(t, c, r, fx[0].request)

	// The enqueue skips the request that is gone and still names both survivors.
	enqueued := r.MapCanonicalToFollowersForTest(ctx, getNamed(t, c, fx[0].request))
	names := make([]string, 0, len(enqueued))
	for _, e := range enqueued {
		names = append(names, e.Name)
	}
	assert.ElementsMatch(t, []string{sessionGone.request, survivor.request}, names,
		"a vanished follower must be skipped, not abort the walk; enqueued=%v", names)

	// Reconciling the vanished one is a clean no-op: no error, nothing to
	// propagate to, and -- critically -- no reason for the caller to stop.
	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: ns, Name: bothGone.request}})
	require.NoError(t, err, "a follower that no longer exists must reconcile away quietly")

	for _, f := range []credUpdateFixture{sessionGone, survivor} {
		reconcileNamed(t, r, f.request)
		got := getNamed(t, c, f.request)
		assert.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseExpired, got.Status.Phase,
			"%s must settle with the canonical regardless of what happened to any OTHER follower "+
				"(or to its own session); status=%+v", f.request, got.Status)
		assert.True(t, spiceboxv1alpha1.IsCredentialUpdateRequestTerminal(got.Status.Phase),
			"%s must reach a phase its blocked meta tool may stop on", f.request)
	}
}

// TestReconcile_PropagationIsIdempotentAndSelfConverging covers the two
// properties a propagation step has to have to be trustworthy under retry.
//
// Idempotent: settling twice must be byte-identical, so a duplicate enqueue (a
// canonical write and the recheck timer landing together) cannot churn a
// settled follower's status or re-log a decision.
//
// Self-converging: while its canonical is still live, a follower asks to be
// re-reconciled on its own. That requeue is what makes propagation converge
// when an enqueue never arrives at all -- the map func's List failing, an
// informer restart mid-handover -- so no follower's release hangs on a single
// event being delivered.
func TestReconcile_PropagationIsIdempotentAndSelfConverging(t *testing.T) {
	c, r, probes, fx := oneCardManyFollowers(t, 2)
	ctx := context.Background()
	followerKey := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: fx[1].request}}

	res, err := r.Reconcile(ctx, followerKey)
	require.NoError(t, err)
	assert.Positive(t, res.RequeueAfter,
		"a follower waiting on a live card must schedule its own re-check, so a dropped enqueue delays "+
			"propagation rather than losing it")

	replaceSharedCredential(t, c)
	reconcileNamed(t, r, fx[0].request)
	require.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseFulfilled,
		getNamed(t, c, fx[0].request).Status.Phase, "positive control: the canonical settled")

	res, err = r.Reconcile(ctx, followerKey)
	require.NoError(t, err)
	settled := getNamed(t, c, fx[1].request)
	require.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseFulfilled, settled.Status.Phase,
		"positive control: the follower really did settle, so the idempotency check below is about a "+
			"settled request rather than an untouched one")
	assert.Zero(t, res.RequeueAfter, "a settled follower has nothing left to re-check")
	probesAfterSettle := probes.Load()

	_, err = r.Reconcile(ctx, followerKey)
	require.NoError(t, err)
	again := getNamed(t, c, fx[1].request)
	assert.Equal(t, settled.Status, again.Status, "re-reconciling a settled follower must be byte-identical")
	assert.Equal(t, settled.ResourceVersion, again.ResourceVersion,
		"and must not write at all. There is no changed-only-write filter under this -- a settled follower is "+
			"terminal, and Reconcile's phase gate returns on a terminal phase BEFORE reaching any status write. "+
			"That gate is the whole guarantee, so relaxing it to let a settled request fall through would make "+
			"every canonical event PUT every follower")
	assert.Equal(t, probesAfterSettle, probes.Load(), "and must not re-probe")
}

// TestReconcile_CollapsedRequestWhoseCanonicalVanishedRedeterminesItsOwnAsk
// pins the branch propagation must NOT swallow: when the canonical is gone
// rather than settled, there is no answer to inherit, and the follower has to
// go and get its own card. Settling it "with" a canonical that does not exist
// would tell an agent its ask was decided when nobody ever looked at it.
//
// The follower's session arrives carrying ONE prior spent ask, and that is not
// scenery. Re-determination re-runs the ask budget, and the budget counts peers
// -- so it is the one path where a request could be counted against ITSELF.
// With a fresh session (zero priors) the off-by-one is invisible: 0 self-counted
// +1 is still under the limit of two, and the request opens either way. With one
// prior, a peer list that failed to exclude this request reaches two and refuses
// it, wedging an agent whose budget was never actually spent.
func TestReconcile_CollapsedRequestWhoseCanonicalVanishedRedeterminesItsOwnAsk(t *testing.T) {
	const priorAsk = "demo-cur-b-prior"
	sessions := fixtureSessions(t, 2)
	followerSession := sessions[1]

	c, r, probes, fx := oneCardManyFollowers(t, 2,
		spentAskFor(priorAsk, followerSession.session, followerSession.uid))
	ctx := context.Background()

	// Positive control: the seeded prior really is a countable ask charged to
	// the follower's own session. Without this the budget arithmetic below is
	// 0+1 rather than 1+1, and the self-exclusion claim goes back to being
	// unobservable.
	requireCountableAsk(t, getNamed(t, c, priorAsk), followerSession.session)
	require.Equal(t, 2, credentialupdaterequest.BudgetLimitForTest,
		"this test's arithmetic assumes a limit of two asks per (session, credential)")

	require.NoError(t, c.Delete(ctx, &spiceboxv1alpha1.CredentialUpdateRequest{
		ObjectMeta: metav1Object(fx[0].request)}), "the canonical's session ended, taking its request with it")

	reconcileNamed(t, r, fx[1].request)
	got := getNamed(t, c, fx[1].request)

	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen, got.Status.Phase,
		"with nothing left to wait on, the follower must raise its OWN card -- and must not have been counted "+
			"against its own budget on the way; status=%+v", got.Status)
	assert.Nil(t, got.Status.CollapsedInto, "the dead pointer must be dropped")
	assert.Greater(t, probes.Load(), int32(1),
		"re-determining means a real determination ran, probe included -- otherwise this card was opened "+
			"on the strength of a decision nobody made")
}

// metav1Object builds the minimal ObjectMeta needed to Delete a namespaced
// object by name. Kept local and tiny: the alternative is Get-then-Delete
// round-trips that add nothing to what these tests are proving.
func metav1Object(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: ns}
}

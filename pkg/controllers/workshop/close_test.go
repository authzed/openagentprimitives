package workshop_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
)

// closeTarget builds a target Workshop that looks provisioned — status.namespace
// anchored AND TupleWritten stamped — plus the builder AgentSession it belongs
// to. status.namespace is the id the close check keys on: it is what
// EnsureWorkshopSubjects wrote the TARGET's tuples under (Reconcile step 7's
// nsName), so a check against anything else would ask SpiceDB about a workshop
// object nobody ever wrote. The condition is what says those tuples are
// actually standing — the anchor alone is persisted three steps earlier.
func closeTarget(name, uid string) (*spiceboxv1alpha1.Workshop, *spiceboxv1alpha1.AgentSession) {
	sess := builderSession(name, uid)
	ws := sanctionedWorkshop(sess)
	ws.Status.Namespace = spiceboxv1alpha1.WorkshopNamespaceName(sess.UID)
	ws.Status.Phase = spiceboxv1alpha1.WorkshopPhaseReady
	return tuplesWritten(ws), sess
}

// closeTargetIn builds a provisioned target in ANOTHER namespace — the shape a
// cluster with a second sanctioned builder class produces, and the one the
// start route's cluster-wide count can name.
func closeTargetIn(ns, name, uid string) (*spiceboxv1alpha1.Workshop, *spiceboxv1alpha1.AgentSession) {
	ws, sess := closeTarget(name, uid)
	sess.Namespace = ns
	ws.Namespace = ns
	ws.Spec.Session.Namespace = ns
	return ws, sess
}

// tuplesWritten stamps the condition Reconcile's step 7 sets once a workshop's
// SpiceDB tuples are standing — the signal a close decision waits for.
func tuplesWritten(ws *spiceboxv1alpha1.Workshop) *spiceboxv1alpha1.Workshop {
	apimeta.SetStatusCondition(&ws.Status.Conditions, metav1.Condition{
		Type:   spiceboxv1alpha1.WorkshopConditionTupleWritten,
		Status: metav1.ConditionTrue,
		Reason: spiceboxv1alpha1.ReasonWorkshopProvisioned,
	})
	return ws
}

// stillProvisioning rolls a target back to the state Reconcile leaves it in
// before step 7: no anchor and no tuples, so nothing a close check reads exists.
func stillProvisioning(ws *spiceboxv1alpha1.Workshop) *spiceboxv1alpha1.Workshop {
	ws.Status.Namespace = ""
	ws.Status.Phase = spiceboxv1alpha1.WorkshopPhaseProvisioning
	apimeta.RemoveStatusCondition(&ws.Status.Conditions, spiceboxv1alpha1.WorkshopConditionTupleWritten)
	return ws
}

// withCloseRequest returns ws with one spec.closeRequests entry for target,
// asked just now — far from the decision deadline, the way the tool's own
// writes arrive.
func withCloseRequest(ws *spiceboxv1alpha1.Workshop, target string) *spiceboxv1alpha1.Workshop {
	return withCloseRequestAt(ws, target, metav1.NewTime(time.Now()))
}

// withCloseRequestAt is withCloseRequest with the asking time spelled out. The
// decision deadline is measured from it, so a test about that deadline has to
// own it; a zero metav1.Time is the hand-written request that carried no time
// at all.
func withCloseRequestAt(ws *spiceboxv1alpha1.Workshop, target string, at metav1.Time) *spiceboxv1alpha1.Workshop {
	ws.Spec.CloseRequests = append(ws.Spec.CloseRequests, spiceboxv1alpha1.WorkshopCloseRequest{
		Target:      target,
		RequestedAt: at,
	})
	return ws
}

func closeStatusFor(ws *spiceboxv1alpha1.Workshop, target string) *spiceboxv1alpha1.WorkshopCloseStatus {
	for i := range ws.Status.CloseRequests {
		if ws.Status.CloseRequests[i].Target == target {
			return &ws.Status.CloseRequests[i]
		}
	}
	return nil
}

func sessionExists(t *testing.T, c client.Client, ns, name string) bool {
	t.Helper()
	var got spiceboxv1alpha1.AgentSession
	err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &got)
	if err == nil {
		return true
	}
	require.True(t, apierrors.IsNotFound(err), "unexpected error reading session %s/%s: %v", ns, name, err)
	return false
}

// TestReconcile_CloseRequestsAreDecidedOncePerTarget is the close pass's
// decision table: every spec.closeRequests entry the controller has not yet
// decided gets exactly one status entry, and only an ALLOWED check may end
// anyone's workshop.
func TestReconcile_CloseRequestsAreDecidedOncePerTarget(t *testing.T) {
	cases := []struct {
		name string
		// selfTarget names the REQUESTING workshop as the target.
		selfTarget bool
		// createTarget materializes the target Workshop + its builder session.
		createTarget bool
		// noStarter empties spec.starterCanonical on the requester.
		noStarter       bool
		allow           bool
		wantPhase       string
		wantMessage     string
		wantChecks      int
		wantSessionGone bool
	}{
		{
			name:            "target does not exist: NotFound, nothing checked, nothing deleted",
			createTarget:    false,
			wantPhase:       spiceboxv1alpha1.WorkshopClosePhaseNotFound,
			wantChecks:      0,
			wantSessionGone: false,
		},
		{
			name:            "check denies: Refused 'not yours to close', the target's session is untouched",
			createTarget:    true,
			allow:           false,
			wantPhase:       spiceboxv1alpha1.WorkshopClosePhaseRefused,
			wantMessage:     "not yours to close",
			wantChecks:      1,
			wantSessionGone: false,
		},
		{
			name:            "check allows: the target's builder session is deleted and the decision is Closed",
			createTarget:    true,
			allow:           true,
			wantPhase:       spiceboxv1alpha1.WorkshopClosePhaseClosed,
			wantChecks:      1,
			wantSessionGone: true,
		},
		{
			name:        "the requester's own workshop: Refused 'the workshop you are in', nothing is checked",
			selfTarget:  true,
			wantPhase:   spiceboxv1alpha1.WorkshopClosePhaseRefused,
			wantMessage: "the workshop you are in",
			wantChecks:  0,
		},
		{
			name:            "no starter recorded: Refused without a check — nobody to ask as",
			createTarget:    true,
			noStarter:       true,
			allow:           true,
			wantPhase:       spiceboxv1alpha1.WorkshopClosePhaseRefused,
			wantMessage:     "not yours to close",
			wantChecks:      0,
			wantSessionGone: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := builderSession("builder-close", "a1b2c3d4-e5f6-7890-abcd-ef1234567890")
			ws := sanctionedWorkshop(sess)
			if tc.noStarter {
				ws.Spec.StarterCanonical = ""
			}
			targetWS, targetSess := closeTarget("builder-target", "f1e2d3c4-b5a6-7890-abcd-ef1234567890")

			target := targetWS.Name
			if tc.selfTarget {
				target = ws.Name
			}
			withCloseRequest(ws, target)

			objs := []client.Object{sess, ws}
			if tc.createTarget {
				objs = append(objs, targetWS, targetSess)
			}
			ft := &fakeTuples{closeAllow: map[string]bool{targetWS.Status.Namespace: tc.allow}}
			c, r := newFakeReconciler(t, ft, tokens.NewRegistry(), objs...)

			primeFinalizer(t, r, ws)
			_, err := reconcileWorkshop(t, r, ws)
			require.NoError(t, err, "a decided close request must never fail the reconcile")

			got := getWorkshop(t, c, ws)
			require.Len(t, got.Status.CloseRequests, 1, "exactly one decision per request")
			decision := closeStatusFor(got, target)
			require.NotNil(t, decision, "the decision must be keyed by its target")
			assert.Equal(t, tc.wantPhase, decision.Phase)
			if tc.wantMessage != "" {
				assert.Equal(t, tc.wantMessage, decision.Message)
			}
			assert.NotNil(t, decision.DecidedAt, "every decision records when it was made")
			assert.Len(t, ft.closeChecks, tc.wantChecks, "close checks asked: %v", ft.closeChecks)
			if tc.wantChecks == 1 {
				assert.Equal(t, targetWS.Status.Namespace+"|"+ws.Spec.StarterCanonical, ft.closeChecks[0],
					"the check names the target's workshop id and asks as the REQUESTER's starter")
			}
			if tc.createTarget {
				assert.Equal(t, !tc.wantSessionGone, sessionExists(t, c, targetSess.Namespace, targetSess.Name),
					"only an allowed check may end the target's builder session")
			}
			// The requester is never the thing that gets deleted.
			assert.True(t, sessionExists(t, c, sess.Namespace, sess.Name), "the requesting builder's own session must survive")
		})
	}
}

// A decision is SET ONCE: the second reconcile must neither re-check nor
// re-delete. A target's session recreated between passes (a person starting a
// new build) must not be killed by a request that was already answered.
func TestReconcile_ADecidedCloseRequestIsNeverRedecided(t *testing.T) {
	sess := builderSession("builder-close2", "a1b2c3d4-e5f6-7890-abcd-ef1234567891")
	ws := sanctionedWorkshop(sess)
	targetWS, targetSess := closeTarget("builder-target2", "f1e2d3c4-b5a6-7890-abcd-ef1234567891")
	withCloseRequest(ws, targetWS.Name)

	ft := &fakeTuples{closeAllow: map[string]bool{targetWS.Status.Namespace: true}}
	c, r := newFakeReconciler(t, ft, tokens.NewRegistry(), sess, ws, targetWS, targetSess)
	ctx := context.Background()

	primeFinalizer(t, r, ws)
	_, err := reconcileWorkshop(t, r, ws)
	require.NoError(t, err)
	got := getWorkshop(t, c, ws)
	require.Len(t, got.Status.CloseRequests, 1)
	require.Equal(t, spiceboxv1alpha1.WorkshopClosePhaseClosed, got.Status.CloseRequests[0].Phase)
	firstDecidedAt := got.Status.CloseRequests[0].DecidedAt
	require.False(t, sessionExists(t, c, targetSess.Namespace, targetSess.Name), "the allowed close deletes the target's session")

	// A NEW session of the same name — the person started another build.
	replacement := builderSession(targetSess.Name, "99999999-b5a6-7890-abcd-ef1234567891")
	require.NoError(t, c.Create(ctx, replacement))

	_, err = reconcileWorkshop(t, r, got)
	require.NoError(t, err)
	got = getWorkshop(t, c, ws)
	assert.Len(t, got.Status.CloseRequests, 1, "a decided request must not gain a second entry")
	assert.Equal(t, firstDecidedAt, got.Status.CloseRequests[0].DecidedAt, "a decision is written once and never restamped")
	assert.Len(t, ft.closeChecks, 1, "a decided request must not be re-checked")
	assert.True(t, sessionExists(t, c, replacement.Namespace, replacement.Name),
		"a request answered once must never reach through to a later session of the same name")
}

// A check that ERRORS is not a refusal: nothing is decided and the reconcile
// returns the error so the pass retries. Recording Refused here would make an
// unreachable SpiceDB permanently answer "not yours to close".
func TestReconcile_CloseCheckErrorRetriesRatherThanRefusing(t *testing.T) {
	sess := builderSession("builder-close3", "a1b2c3d4-e5f6-7890-abcd-ef1234567892")
	ws := sanctionedWorkshop(sess)
	targetWS, targetSess := closeTarget("builder-target3", "f1e2d3c4-b5a6-7890-abcd-ef1234567892")
	withCloseRequest(ws, targetWS.Name)

	ft := &fakeTuples{closeErr: assert.AnError}
	c, r := newFakeReconciler(t, ft, tokens.NewRegistry(), sess, ws, targetWS, targetSess)

	primeFinalizer(t, r, ws)
	_, err := reconcileWorkshop(t, r, ws)
	require.Error(t, err, "an unanswered close check must fail the reconcile, not decide")

	got := getWorkshop(t, c, ws)
	assert.Empty(t, got.Status.CloseRequests, "no decision may be recorded from a check that never answered")
	assert.True(t, sessionExists(t, c, targetSess.Namespace, targetSess.Name), "nothing is deleted on an unanswered check")
}

// A target that has not finished provisioning has no tuples yet, so no check
// could answer truthfully. It is LEFT UNDECIDED — never refused — and the
// requester still provisions to Ready: one person's half-built workshop must
// not wedge another's.
func TestReconcile_UnprovisionedCloseTargetIsLeftUndecided(t *testing.T) {
	sess := builderSession("builder-close4", "a1b2c3d4-e5f6-7890-abcd-ef1234567893")
	ws := sanctionedWorkshop(sess)
	targetWS, targetSess := closeTarget("builder-target4", "f1e2d3c4-b5a6-7890-abcd-ef1234567893")
	stillProvisioning(targetWS)
	withCloseRequest(ws, targetWS.Name)

	ft := &fakeTuples{}
	c, r := newFakeReconciler(t, ft, tokens.NewRegistry(), sess, ws, targetWS, targetSess)

	primeFinalizer(t, r, ws)
	_, err := reconcileWorkshop(t, r, ws)
	require.NoError(t, err)

	got := getWorkshop(t, c, ws)
	assert.Empty(t, got.Status.CloseRequests, "an undecidable target gets no fabricated decision")
	assert.Empty(t, ft.closeChecks, "no check is asked about a workshop whose tuples cannot exist yet")
	assert.Equal(t, spiceboxv1alpha1.WorkshopPhaseReady, got.Status.Phase, "the requester still provisions")
	assert.True(t, sessionExists(t, c, targetSess.Namespace, targetSess.Name))
}

// A target that HAS a namespace anchor is still not decidable until its tuples
// are written: Reconcile persists the anchor at step 3 and writes the tuples at
// step 7, so in that window a check would answer "no" about a person's own
// workshop — and the set-once Refused it produced could never be revisited.
// TupleWritten is the signal; the anchor is not.
func TestReconcile_CloseTargetIsUndecidedUntilItsTuplesAreWritten(t *testing.T) {
	sess := builderSession("builder-close5", "a1b2c3d4-e5f6-7890-abcd-ef1234567894")
	ws := sanctionedWorkshop(sess)
	targetWS, targetSess := closeTarget("builder-target5", "f1e2d3c4-b5a6-7890-abcd-ef1234567894")
	// Past step 3 (anchored, so a namespace IS recorded), before step 7.
	apimeta.RemoveStatusCondition(&targetWS.Status.Conditions, spiceboxv1alpha1.WorkshopConditionTupleWritten)
	targetWS.Status.Phase = spiceboxv1alpha1.WorkshopPhaseProvisioning
	withCloseRequest(ws, targetWS.Name)

	ft := &fakeTuples{closeAllow: map[string]bool{targetWS.Status.Namespace: true}}
	c, r := newFakeReconciler(t, ft, tokens.NewRegistry(), sess, ws, targetWS, targetSess)
	ctx := context.Background()

	primeFinalizer(t, r, ws)
	res, err := reconcileWorkshop(t, r, ws)
	require.NoError(t, err)

	got := getWorkshop(t, c, ws)
	assert.Empty(t, got.Status.CloseRequests, "an anchored-but-untupled target gets no set-once decision")
	assert.Empty(t, ft.closeChecks, "nothing is asked of SpiceDB about tuples that are not standing yet")
	assert.True(t, sessionExists(t, c, targetSess.Namespace, targetSess.Name), "nothing is ended on an undecidable target")
	assert.Equal(t, 5*time.Second, res.RequeueAfter, "the undecided request brings the pass back for it")

	// Step 7 lands on the target: now the tuples a check reads exist.
	var target spiceboxv1alpha1.Workshop
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: targetWS.Namespace, Name: targetWS.Name}, &target))
	target.Status.Phase = spiceboxv1alpha1.WorkshopPhaseReady
	require.NoError(t, c.Status().Update(ctx, tuplesWritten(&target)))

	_, err = reconcileWorkshop(t, r, got)
	require.NoError(t, err)

	got = getWorkshop(t, c, ws)
	decision := closeStatusFor(got, targetWS.Name)
	require.NotNil(t, decision, "once the tuples are standing the request is decided")
	assert.Equal(t, spiceboxv1alpha1.WorkshopClosePhaseClosed, decision.Phase)
	assert.Len(t, ft.closeChecks, 1, "the check is asked once, on the pass that could answer it")
	assert.False(t, sessionExists(t, c, targetSess.Namespace, targetSess.Name))
}

// A target stuck before its tuple write gets a BOUNDED wait, not an endless
// one: ten minutes from the moment the tool asked, after which the request is
// refused in plain words rather than holding the requester's five-second
// requeue open for the life of the workshop. Both sides of that boundary are
// here, because the near side is what makes the far side safe — a target that
// finishes provisioning in time is still decided the ordinary way, and a
// target that finished late but DID finish is still decided by SpiceDB.
func TestReconcile_ACloseTargetStuckBeforeItsTuplesIsRefusedAtTheDeadline(t *testing.T) {
	const (
		pendingRequeue = 5 * time.Second
		deadlineWhy    = "not closed: it was still being set up when the time ran out"
	)
	// A fixed clock, so "eleven minutes ago" means the same thing to the
	// reconciler as it does to the test.
	clock := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		// requestedAgo is how long before clock the tool asked;
		// noRequestedAt is the hand-written request that carried no time.
		requestedAgo  time.Duration
		noRequestedAt bool
		// tupled leaves the target decidable — its close tuples are standing.
		tupled bool
		allow  bool
		// wantPhase "" means the request must be left undecided, which is also
		// what makes the pass ask for its five-second re-check.
		wantPhase       string
		wantMessage     string
		wantChecks      int
		wantSessionGone bool
	}{
		{
			name:         "a minute in, tuples not standing yet: left undecided, and the pass comes back in five seconds",
			requestedAgo: time.Minute,
		},
		{
			name:         "eleven minutes in, tuples still not standing: Refused in plain words, nothing deleted, no five-second loop",
			requestedAgo: 11 * time.Minute,
			wantPhase:    spiceboxv1alpha1.WorkshopClosePhaseRefused,
			wantMessage:  deadlineWhy,
		},
		{
			name:          "the request carries no requestedAt: Refused on the first pass rather than waited on forever",
			noRequestedAt: true,
			wantPhase:     spiceboxv1alpha1.WorkshopClosePhaseRefused,
			wantMessage:   deadlineWhy,
		},
		{
			name:            "past the deadline but the tuples ARE standing: SpiceDB decides it, not the clock",
			requestedAgo:    11 * time.Minute,
			tupled:          true,
			allow:           true,
			wantPhase:       spiceboxv1alpha1.WorkshopClosePhaseClosed,
			wantChecks:      1,
			wantSessionGone: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := builderSession("builder-deadline", "a1b2c3d4-e5f6-7890-abcd-ef1234567920")
			ws := sanctionedWorkshop(sess)
			targetWS, targetSess := closeTarget("builder-stuck", "f1e2d3c4-b5a6-7890-abcd-ef1234567920")
			// Read before stillProvisioning clears it: it is the id the check
			// would name if this target ever became decidable.
			targetNS := targetWS.Status.Namespace
			if !tc.tupled {
				stillProvisioning(targetWS)
			}
			at := metav1.NewTime(clock.Add(-tc.requestedAgo))
			if tc.noRequestedAt {
				at = metav1.Time{}
			}
			withCloseRequestAt(ws, targetWS.Name, at)

			ft := &fakeTuples{closeAllow: map[string]bool{targetNS: tc.allow}}
			c, r := newFakeReconciler(t, ft, tokens.NewRegistry(), sess, ws, targetWS, targetSess)
			r.Now = func() time.Time { return clock }

			primeFinalizer(t, r, ws)
			res, err := reconcileWorkshop(t, r, ws)
			require.NoError(t, err, "a deadline is a decision, never a failed reconcile")

			got := getWorkshop(t, c, ws)
			decision := closeStatusFor(got, targetWS.Name)
			if tc.wantPhase == "" {
				assert.Nil(t, decision, "a target still inside its deadline gets no decision")
				assert.Equal(t, pendingRequeue, res.RequeueAfter,
					"an undecided request is re-checked for as long as its deadline runs")
			} else {
				require.NotNil(t, decision, "the request must be decided")
				assert.Equal(t, tc.wantPhase, decision.Phase)
				assert.Equal(t, tc.wantMessage, decision.Message)
				assert.NotNil(t, decision.DecidedAt, "every decision records when it was made")
				assert.Greater(t, res.RequeueAfter, pendingRequeue,
					"nothing is left undecided, so the five-second loop must end")
			}
			assert.Len(t, ft.closeChecks, tc.wantChecks, "close checks asked: %v", ft.closeChecks)
			assert.Equal(t, !tc.wantSessionGone, sessionExists(t, c, targetSess.Namespace, targetSess.Name),
				"a deadline refuses a target; only an allowed check may end one")
			assert.True(t, sessionExists(t, c, sess.Namespace, sess.Name), "the requesting builder's own session must survive")
		})
	}
}

// closeSpec describes one workshop to stand up beside the requester in the
// wildcard table below: whose it is, whether it finished provisioning, and how
// the close check answers for it.
type closeSpec struct {
	name          string
	starter       string // "" means the requester's own starter
	unprovisioned bool
	deleting      bool
	allow         bool
}

// TestReconcile_WildcardCloseRequestIsExpandedToTheStartersOtherWorkshops is
// the wildcard pass: the sidecar's Role is name-restricted to its own Workshop,
// so it cannot list the person's workshops and asks for "*" instead. The
// controller is the only party that can resolve that, and it resolves it to the
// SAME decision a named target would get, one status entry each, plus a summary
// entry for "*" itself.
func TestReconcile_WildcardCloseRequestIsExpandedToTheStartersOtherWorkshops(t *testing.T) {
	cases := []struct {
		name string
		// others are the workshops standing beside the requester.
		others []closeSpec
		// wantPerTarget maps a target's workshop name to its decided phase.
		wantPerTarget      map[string]string
		wantSummaryPhase   string
		wantSummaryMessage string
		// wantSessionsGone names the builder SESSIONS the pass must have ended.
		wantSessionsGone []string
	}{
		{
			name:               "nobody else's workshop is open: the summary is NotFound and nothing is checked",
			others:             nil,
			wantPerTarget:      map[string]string{},
			wantSummaryPhase:   spiceboxv1alpha1.WorkshopClosePhaseNotFound,
			wantSummaryMessage: "no other workshops of yours",
		},
		{
			name:               "one other workshop of the same person: closed, and the summary counts it",
			others:             []closeSpec{{name: "builder-w1", allow: true}},
			wantPerTarget:      map[string]string{spiceboxv1alpha1.WorkshopName("builder-w1"): spiceboxv1alpha1.WorkshopClosePhaseClosed},
			wantSummaryPhase:   spiceboxv1alpha1.WorkshopClosePhaseClosed,
			wantSummaryMessage: "1 closed, 0 refused",
			wantSessionsGone:   []string{"builder-w1"},
		},
		{
			name:   "a refused target is counted as refused, not closed, and its session survives",
			others: []closeSpec{{name: "builder-w2", allow: true}, {name: "builder-w3", allow: false}},
			wantPerTarget: map[string]string{
				spiceboxv1alpha1.WorkshopName("builder-w2"): spiceboxv1alpha1.WorkshopClosePhaseClosed,
				spiceboxv1alpha1.WorkshopName("builder-w3"): spiceboxv1alpha1.WorkshopClosePhaseRefused,
			},
			wantSummaryPhase:   spiceboxv1alpha1.WorkshopClosePhaseClosed,
			wantSummaryMessage: "1 closed, 1 refused",
			wantSessionsGone:   []string{"builder-w2"},
		},
		{
			name:               "another person's workshop is never expanded into, so the wildcard finds nothing",
			others:             []closeSpec{{name: "builder-w4", starter: "someone-else", allow: true}},
			wantPerTarget:      map[string]string{},
			wantSummaryPhase:   spiceboxv1alpha1.WorkshopClosePhaseNotFound,
			wantSummaryMessage: "no other workshops of yours",
		},
		{
			name:               "a workshop already being torn down is not expanded into",
			others:             []closeSpec{{name: "builder-w5", deleting: true, allow: true}},
			wantPerTarget:      map[string]string{},
			wantSummaryPhase:   spiceboxv1alpha1.WorkshopClosePhaseNotFound,
			wantSummaryMessage: "no other workshops of yours",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := builderSession("builder-wild", "a1b2c3d4-e5f6-7890-abcd-ef1234567900")
			ws := sanctionedWorkshop(sess)
			withCloseRequest(ws, spiceboxv1alpha1.WorkshopCloseTargetAll)

			objs := []client.Object{sess, ws}
			allow := map[string]bool{}
			for i, spec := range tc.others {
				otherWS, otherSess := closeTarget(spec.name, otherUID(i))
				if spec.starter != "" {
					otherWS.Spec.StarterCanonical = spec.starter
				}
				if spec.unprovisioned {
					stillProvisioning(otherWS)
				}
				if spec.deleting {
					otherWS.Finalizers = []string{spiceboxv1alpha1.FinalizerWorkshop}
					now := metav1.NewTime(time.Now())
					otherWS.DeletionTimestamp = &now
				}
				allow[otherWS.Status.Namespace] = spec.allow
				objs = append(objs, otherWS, otherSess)
			}

			ft := &fakeTuples{closeAllow: allow}
			c, r := newFakeReconciler(t, ft, tokens.NewRegistry(), objs...)

			primeFinalizer(t, r, ws)
			_, err := reconcileWorkshop(t, r, ws)
			require.NoError(t, err, "an expanded wildcard must never fail the reconcile")

			got := getWorkshop(t, c, ws)
			for target, wantPhase := range tc.wantPerTarget {
				decision := closeStatusFor(got, target)
				require.NotNil(t, decision, "the expansion must record a decision for %s", target)
				assert.Equal(t, wantPhase, decision.Phase, "phase for %s", target)
			}
			summary := closeStatusFor(got, spiceboxv1alpha1.WorkshopCloseTargetAll)
			require.NotNil(t, summary, "the wildcard itself must be answered, or the tool waits forever")
			assert.Equal(t, tc.wantSummaryPhase, summary.Phase)
			assert.Equal(t, tc.wantSummaryMessage, summary.Message)
			assert.Len(t, got.Status.CloseRequests, len(tc.wantPerTarget)+1,
				"one entry per resolved target plus the wildcard summary, and nothing else: %+v", got.Status.CloseRequests)

			gone := map[string]bool{}
			for _, n := range tc.wantSessionsGone {
				gone[n] = true
			}
			for _, spec := range tc.others {
				assert.Equal(t, !gone[spec.name], sessionExists(t, c, "default", spec.name),
					"only an allowed, expanded target may lose its builder session: %s", spec.name)
			}
			assert.True(t, sessionExists(t, c, sess.Namespace, sess.Name), "the requesting builder's own session must survive")
		})
	}
}

// otherUID makes a distinct, well-formed UID per extra workshop in the table
// above; the value only has to differ, since WorkshopNamespaceName hashes it.
func otherUID(i int) string {
	// The first twelve hex digits are the whole of it: WorkshopNamespaceName
	// truncates there, so two UIDs that differ only near the end would hash to
	// ONE workshop id and the per-target close checks would answer as one.
	return fmt.Sprintf("f1e2d3c4-b5%02d-7890-abcd-ef1234567900", i)
}

// The wildcard summary is SET ONCE, so recording it while one expanded target
// is still undecidable would freeze a count that was never true and strand that
// target forever. It is held back instead, and the decisions already made are
// not re-made when the held pass runs again.
func TestReconcile_WildcardSummaryIsHeldUntilEveryExpandedTargetIsDecided(t *testing.T) {
	sess := builderSession("builder-wild2", "a1b2c3d4-e5f6-7890-abcd-ef1234567901")
	ws := sanctionedWorkshop(sess)
	withCloseRequest(ws, spiceboxv1alpha1.WorkshopCloseTargetAll)

	readyWS, readySess := closeTarget("builder-ready", "f1e2d3c4-b5a6-7890-abcd-ef1234567901")
	slowWS, slowSess := closeTarget("builder-slow", "a9b8c7d6-e5f4-7890-abcd-ef1234567902")
	slowNS := slowWS.Status.Namespace
	stillProvisioning(slowWS) // its close tuples cannot exist yet

	ft := &fakeTuples{closeAllow: map[string]bool{readyWS.Status.Namespace: true, slowNS: true}}
	c, r := newFakeReconciler(t, ft, tokens.NewRegistry(), sess, ws, readyWS, readySess, slowWS, slowSess)
	ctx := context.Background()

	primeFinalizer(t, r, ws)
	_, err := reconcileWorkshop(t, r, ws)
	require.NoError(t, err)

	got := getWorkshop(t, c, ws)
	assert.Nil(t, closeStatusFor(got, spiceboxv1alpha1.WorkshopCloseTargetAll),
		"the summary must not be written while a target is still undecidable")
	ready := closeStatusFor(got, readyWS.Name)
	require.NotNil(t, ready, "a target that CAN be decided is decided on this pass")
	assert.Equal(t, spiceboxv1alpha1.WorkshopClosePhaseClosed, ready.Phase)
	require.False(t, sessionExists(t, c, "default", readySess.Name))

	// The slow workshop finishes provisioning; the next pass decides it and
	// only then summarizes.
	var slow spiceboxv1alpha1.Workshop
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: slowWS.Namespace, Name: slowWS.Name}, &slow))
	slow.Status.Namespace = slowNS
	slow.Status.Phase = spiceboxv1alpha1.WorkshopPhaseReady
	require.NoError(t, c.Status().Update(ctx, tuplesWritten(&slow)))

	_, err = reconcileWorkshop(t, r, got)
	require.NoError(t, err)

	got = getWorkshop(t, c, ws)
	summary := closeStatusFor(got, spiceboxv1alpha1.WorkshopCloseTargetAll)
	require.NotNil(t, summary, "once every target is decided the wildcard is answered")
	assert.Equal(t, spiceboxv1alpha1.WorkshopClosePhaseClosed, summary.Phase)
	assert.Equal(t, "2 closed, 0 refused", summary.Message)
	assert.Len(t, ft.closeChecks, 2, "a target decided on an earlier pass must never be re-checked: %v", ft.closeChecks)
	assert.False(t, sessionExists(t, c, "default", slowSess.Name))
}

// Every target the wildcard expands to inherits the "*" entry's own asking
// time: the person asked once, so one deadline covers everything that ask
// resolved to. A workshop of theirs stuck before its tuple write is refused at
// that deadline, and the summary the expansion HELD for it is then written,
// counting it as refused — without which one half-built workshop would hold a
// person's "close everything" open forever.
func TestReconcile_WildcardCloseRefusesAnExpandedTargetThatRanOutOfTime(t *testing.T) {
	const deadlineWhy = "not closed: it was still being set up when the time ran out"
	clock := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

	sess := builderSession("builder-wild5", "a1b2c3d4-e5f6-7890-abcd-ef1234567905")
	ws := sanctionedWorkshop(sess)
	withCloseRequestAt(ws, spiceboxv1alpha1.WorkshopCloseTargetAll, metav1.NewTime(clock.Add(-11*time.Minute)))

	readyWS, readySess := closeTarget("builder-ready2", "f1e2d3c4-b5a6-7890-abcd-ef1234567905")
	stuckWS, stuckSess := closeTarget("builder-stuck2", "a9b8c7d6-e5f4-7890-abcd-ef1234567906")
	stuckNS := stuckWS.Status.Namespace
	stillProvisioning(stuckWS) // its close tuples cannot exist yet, and never will

	ft := &fakeTuples{closeAllow: map[string]bool{readyWS.Status.Namespace: true, stuckNS: true}}
	c, r := newFakeReconciler(t, ft, tokens.NewRegistry(), sess, ws, readyWS, readySess, stuckWS, stuckSess)
	r.Now = func() time.Time { return clock }

	primeFinalizer(t, r, ws)
	_, err := reconcileWorkshop(t, r, ws)
	require.NoError(t, err)

	got := getWorkshop(t, c, ws)
	ready := closeStatusFor(got, readyWS.Name)
	require.NotNil(t, ready, "a target that can be decided is decided on this pass")
	assert.Equal(t, spiceboxv1alpha1.WorkshopClosePhaseClosed, ready.Phase)
	stuck := closeStatusFor(got, stuckWS.Name)
	require.NotNil(t, stuck, "a target that ran out of time is decided by the deadline")
	assert.Equal(t, spiceboxv1alpha1.WorkshopClosePhaseRefused, stuck.Phase)
	assert.Equal(t, deadlineWhy, stuck.Message)

	summary := closeStatusFor(got, spiceboxv1alpha1.WorkshopCloseTargetAll)
	require.NotNil(t, summary, "the held summary is answerable once the deadline decided the last target")
	assert.Equal(t, spiceboxv1alpha1.WorkshopClosePhaseClosed, summary.Phase)
	assert.Equal(t, "1 closed, 1 refused", summary.Message)
	assert.Len(t, ft.closeChecks, 1, "the stuck target is refused without a check: %v", ft.closeChecks)
	assert.False(t, sessionExists(t, c, "default", readySess.Name), "the allowed target is ended")
	assert.True(t, sessionExists(t, c, "default", stuckSess.Name), "a deadline refusal ends nothing")
}

// The start route counts a person's open workshops across EVERY namespace, so
// the ones it names in its refusal can sit outside the requester's own — a
// cluster with two sanctioned builder classes produces exactly that. The
// wildcard has to reach them, or a person is told to close workshops the only
// tool they have cannot see.
func TestReconcile_WildcardCloseReachesTheStartersWorkshopsInEveryNamespace(t *testing.T) {
	sess := builderSession("builder-wild4", "a1b2c3d4-e5f6-7890-abcd-ef1234567904")
	ws := sanctionedWorkshop(sess)
	withCloseRequest(ws, spiceboxv1alpha1.WorkshopCloseTargetAll)

	farWS, farSess := closeTargetIn("builder-b", "builder-far", "f1e2d3c4-b5a6-7890-abcd-ef1234567904")

	ft := &fakeTuples{closeAllow: map[string]bool{farWS.Status.Namespace: true}}
	c, r := newFakeReconciler(t, ft, tokens.NewRegistry(), sess, ws, farWS, farSess)

	primeFinalizer(t, r, ws)
	_, err := reconcileWorkshop(t, r, ws)
	require.NoError(t, err)

	got := getWorkshop(t, c, ws)
	// Recorded with the namespace in front, because that is the spelling the
	// person could say back to the tool for a workshop outside their own
	// builder namespace — a bare name there would name nothing.
	decision := closeStatusFor(got, farWS.Namespace+"/"+farWS.Name)
	require.NotNil(t, decision, "a live workshop of the same person is theirs to close wherever it lives: %+v", got.Status.CloseRequests)
	assert.Equal(t, spiceboxv1alpha1.WorkshopClosePhaseClosed, decision.Phase)
	summary := closeStatusFor(got, spiceboxv1alpha1.WorkshopCloseTargetAll)
	require.NotNil(t, summary, "the wildcard itself is answered once its expansion is decided")
	assert.Equal(t, "1 closed, 0 refused", summary.Message)
	assert.False(t, sessionExists(t, c, farSess.Namespace, farSess.Name),
		"the target's builder session is ended in the TARGET's own namespace")
	assert.True(t, sessionExists(t, c, sess.Namespace, sess.Name), "the requesting builder's own session must survive")
}

// The requester's own workshop is never in the expansion, and an unattributed
// requester (no starter recorded) cannot ask on anyone's behalf — the wildcard
// is refused rather than expanded, so "all of mine" from a session with no
// person behind it closes nothing.
func TestReconcile_WildcardFromAnUnattributedWorkshopIsRefusedNotExpanded(t *testing.T) {
	sess := builderSession("builder-wild3", "a1b2c3d4-e5f6-7890-abcd-ef1234567903")
	ws := sanctionedWorkshop(sess)
	ws.Spec.StarterCanonical = ""
	withCloseRequest(ws, spiceboxv1alpha1.WorkshopCloseTargetAll)

	otherWS, otherSess := closeTarget("builder-other", "f1e2d3c4-b5a6-7890-abcd-ef1234567903")
	otherWS.Spec.StarterCanonical = ""

	ft := &fakeTuples{closeAllow: map[string]bool{otherWS.Status.Namespace: true}}
	c, r := newFakeReconciler(t, ft, tokens.NewRegistry(), sess, ws, otherWS, otherSess)

	primeFinalizer(t, r, ws)
	_, err := reconcileWorkshop(t, r, ws)
	require.NoError(t, err)

	got := getWorkshop(t, c, ws)
	require.Len(t, got.Status.CloseRequests, 1, "only the wildcard itself is answered")
	summary := closeStatusFor(got, spiceboxv1alpha1.WorkshopCloseTargetAll)
	require.NotNil(t, summary)
	assert.Equal(t, spiceboxv1alpha1.WorkshopClosePhaseRefused, summary.Phase)
	assert.Equal(t, "not yours to close", summary.Message)
	assert.Empty(t, ft.closeChecks, "nothing is checked on behalf of nobody")
	assert.True(t, sessionExists(t, c, "default", otherSess.Name))
}

// A decision the pass could not make has no trigger of its own: the requester's
// Workshop is otherwise only reconciled by its generic requeue, up to an hour
// out, while the tool that asked waits 30 seconds. So an undecided request
// shortens the requeue — and a pass that decided everything must not, or every
// workshop in the cluster re-reconciles every five seconds forever.
func TestReconcile_APendingCloseRequestIsRecheckedSoon(t *testing.T) {
	const pendingRequeue = 5 * time.Second

	t.Run("a wildcard held on a still-provisioning target: back in five seconds", func(t *testing.T) {
		sess := builderSession("builder-pending", "a1b2c3d4-e5f6-7890-abcd-ef1234567910")
		ws := sanctionedWorkshop(sess)
		withCloseRequest(ws, spiceboxv1alpha1.WorkshopCloseTargetAll)

		slowWS, slowSess := closeTarget("builder-slow2", "c7d6e5f4-a3b2-7890-abcd-ef1234567910")
		slowNS := slowWS.Status.Namespace
		stillProvisioning(slowWS) // its close tuples cannot exist yet

		ft := &fakeTuples{closeAllow: map[string]bool{slowNS: true}}
		c, r := newFakeReconciler(t, ft, tokens.NewRegistry(), sess, ws, slowWS, slowSess)

		primeFinalizer(t, r, ws)
		res, err := reconcileWorkshop(t, r, ws)
		require.NoError(t, err)

		got := getWorkshop(t, c, ws)
		require.Nil(t, closeStatusFor(got, spiceboxv1alpha1.WorkshopCloseTargetAll), "precondition: the wildcard is held")
		assert.Equal(t, pendingRequeue, res.RequeueAfter,
			"an undecided request must be re-checked while the tool that asked is still waiting")
	})

	t.Run("an undecided named target: back in five seconds", func(t *testing.T) {
		sess := builderSession("builder-pending2", "a1b2c3d4-e5f6-7890-abcd-ef1234567911")
		ws := sanctionedWorkshop(sess)
		targetWS, targetSess := closeTarget("builder-slow3", "d8c7b6a5-f4e3-7890-abcd-ef1234567911")
		stillProvisioning(targetWS)
		withCloseRequest(ws, targetWS.Name)

		ft := &fakeTuples{}
		c, r := newFakeReconciler(t, ft, tokens.NewRegistry(), sess, ws, targetWS, targetSess)

		primeFinalizer(t, r, ws)
		res, err := reconcileWorkshop(t, r, ws)
		require.NoError(t, err)

		got := getWorkshop(t, c, ws)
		require.Empty(t, got.Status.CloseRequests, "precondition: the target is undecidable")
		assert.Equal(t, pendingRequeue, res.RequeueAfter)
	})

	t.Run("every request decided: the ordinary requeue stands, no five-second loop", func(t *testing.T) {
		sess := builderSession("builder-pending3", "a1b2c3d4-e5f6-7890-abcd-ef1234567912")
		ws := sanctionedWorkshop(sess)
		targetWS, targetSess := closeTarget("builder-done", "e9d8c7b6-a5f4-7890-abcd-ef1234567912")
		withCloseRequest(ws, targetWS.Name)

		ft := &fakeTuples{closeAllow: map[string]bool{targetWS.Status.Namespace: true}}
		c, r := newFakeReconciler(t, ft, tokens.NewRegistry(), sess, ws, targetWS, targetSess)

		primeFinalizer(t, r, ws)
		res, err := reconcileWorkshop(t, r, ws)
		require.NoError(t, err)

		got := getWorkshop(t, c, ws)
		require.Len(t, got.Status.CloseRequests, 1, "precondition: the request was decided")
		assert.Greater(t, res.RequeueAfter, pendingRequeue,
			"a decided pass must leave the ordinary requeue alone, or every workshop re-reconciles every five seconds forever")
	})
}

// twinTargets stands up two provisioned workshops of the SAME name in two
// namespaces — the shape a cluster with a second sanctioned builder class
// produces, and the one a bare target cannot tell apart. near lives in the
// requester's own namespace ("default"); far lives in closeFarNS.
func twinTargets(t *testing.T, name string) (nearWS, farWS *spiceboxv1alpha1.Workshop, nearSess, farSess *spiceboxv1alpha1.AgentSession) {
	t.Helper()
	nearWS, nearSess = closeTarget(name, "b1b2c3d4-e5f6-7890-abcd-ef1234567930")
	farWS, farSess = closeTargetIn(closeFarNS, name, "c1c2c3d4-e5f6-7890-abcd-ef1234567931")
	return nearWS, farWS, nearSess, farSess
}

// closeFarNS is a builder namespace that is not the requester's own.
const closeFarNS = "other-ns"

// A NAMED target may now carry the namespace its workshop lives in, and a bare
// one still means the requester's own. Both spellings are exercised against the
// SAME pair of same-named workshops, because that is the only fixture where
// resolving to the wrong one is visible: the request must end exactly one of
// them, and which one is the whole question.
func TestReconcile_ANamedCloseTargetMayNameAnotherBuilderNamespace(t *testing.T) {
	t.Run("a bare name: the twin in the requester's own namespace is the one closed", func(t *testing.T) {
		sess := builderSession("builder-nsname", "a1b2c3d4-e5f6-7890-abcd-ef1234567930")
		ws := sanctionedWorkshop(sess)
		nearWS, farWS, nearSess, farSess := twinTargets(t, "builder-twin")
		withCloseRequest(ws, nearWS.Name)

		ft := &fakeTuples{closeAllow: map[string]bool{nearWS.Status.Namespace: true, farWS.Status.Namespace: true}}
		c, r := newFakeReconciler(t, ft, tokens.NewRegistry(), sess, ws, nearWS, nearSess, farWS, farSess)

		primeFinalizer(t, r, ws)
		_, err := reconcileWorkshop(t, r, ws)
		require.NoError(t, err)

		got := getWorkshop(t, c, ws)
		decision := closeStatusFor(got, nearWS.Name)
		require.NotNil(t, decision, "a bare target is the requester's own namespace")
		assert.Equal(t, spiceboxv1alpha1.WorkshopClosePhaseClosed, decision.Phase)
		require.Len(t, ft.closeChecks, 1, "exactly one workshop is asked about: %v", ft.closeChecks)
		assert.Equal(t, nearWS.Status.Namespace+"|"+ws.Spec.StarterCanonical, ft.closeChecks[0],
			"the check names the NEAR twin's workshop id")
		assert.False(t, sessionExists(t, c, nearSess.Namespace, nearSess.Name), "the near twin is ended")
		assert.True(t, sessionExists(t, c, farSess.Namespace, farSess.Name),
			"a bare target must never reach a same-named workshop in another namespace")
	})

	t.Run("namespace/name: the twin in THAT namespace is the one closed", func(t *testing.T) {
		sess := builderSession("builder-nsname2", "a1b2c3d4-e5f6-7890-abcd-ef1234567932")
		ws := sanctionedWorkshop(sess)
		nearWS, farWS, nearSess, farSess := twinTargets(t, "builder-twin")
		withCloseRequest(ws, closeFarNS+"/"+farWS.Name)

		ft := &fakeTuples{closeAllow: map[string]bool{nearWS.Status.Namespace: true, farWS.Status.Namespace: true}}
		c, r := newFakeReconciler(t, ft, tokens.NewRegistry(), sess, ws, nearWS, nearSess, farWS, farSess)

		primeFinalizer(t, r, ws)
		_, err := reconcileWorkshop(t, r, ws)
		require.NoError(t, err)

		got := getWorkshop(t, c, ws)
		decision := closeStatusFor(got, closeFarNS+"/"+farWS.Name)
		require.NotNil(t, decision, "the decision is keyed by the target exactly as it was asked")
		assert.Equal(t, spiceboxv1alpha1.WorkshopClosePhaseClosed, decision.Phase)
		require.Len(t, ft.closeChecks, 1, "exactly one workshop is asked about: %v", ft.closeChecks)
		assert.Equal(t, farWS.Status.Namespace+"|"+ws.Spec.StarterCanonical, ft.closeChecks[0],
			"the check names the FAR twin's workshop id")
		assert.False(t, sessionExists(t, c, farSess.Namespace, farSess.Name), "the far twin is ended")
		assert.True(t, sessionExists(t, c, nearSess.Namespace, nearSess.Name),
			"naming another namespace must not reach the same-named workshop at home")
	})
}

// Two spellings of ONE workshop are one target: the second is answered from the
// decision the first produced — same phase, same words — and never re-checked
// or re-deleted. Without that, "x" and "<my ns>/x" would each delete a session,
// and the second would reach through to whatever build the person started next.
func TestReconcile_ASecondSpellingOfADecidedTargetCopiesTheDecision(t *testing.T) {
	sess := builderSession("builder-spelling", "a1b2c3d4-e5f6-7890-abcd-ef1234567933")
	ws := sanctionedWorkshop(sess)
	targetWS, targetSess := closeTarget("builder-spelled", "f1e2d3c4-b5a6-7890-abcd-ef1234567933")
	withCloseRequest(ws, targetWS.Name)

	ft := &fakeTuples{closeAllow: map[string]bool{targetWS.Status.Namespace: true}}
	c, r := newFakeReconciler(t, ft, tokens.NewRegistry(), sess, ws, targetWS, targetSess)
	ctx := context.Background()

	primeFinalizer(t, r, ws)
	_, err := reconcileWorkshop(t, r, ws)
	require.NoError(t, err)
	got := getWorkshop(t, c, ws)
	require.Len(t, got.Status.CloseRequests, 1, "precondition: the bare spelling was decided")
	require.Equal(t, spiceboxv1alpha1.WorkshopClosePhaseClosed, got.Status.CloseRequests[0].Phase)
	require.False(t, sessionExists(t, c, targetSess.Namespace, targetSess.Name))

	// The person starts another build of the same name, and the tool asks
	// again — this time spelling the target with its namespace.
	replacement := builderSession(targetSess.Name, "99999999-b5a6-7890-abcd-ef1234567933")
	require.NoError(t, c.Create(ctx, replacement))
	got = withCloseRequest(got, "default/"+targetWS.Name)
	require.NoError(t, c.Update(ctx, got))

	_, err = reconcileWorkshop(t, r, got)
	require.NoError(t, err)

	got = getWorkshop(t, c, ws)
	require.Len(t, got.Status.CloseRequests, 2, "the new spelling is answered in its own entry: %+v", got.Status.CloseRequests)
	copied := closeStatusFor(got, "default/"+targetWS.Name)
	require.NotNil(t, copied, "the answer is recorded under the spelling that was asked")
	assert.Equal(t, spiceboxv1alpha1.WorkshopClosePhaseClosed, copied.Phase, "the same decision, not a fresh one")
	assert.Equal(t, got.Status.CloseRequests[0].Message, copied.Message, "the same words")
	assert.Len(t, ft.closeChecks, 1, "a target decided under another spelling must never be re-checked: %v", ft.closeChecks)
	assert.True(t, sessionExists(t, c, replacement.Namespace, replacement.Name),
		"a second spelling must never reach through to a later session of the same name")
}

// The wildcard's expansion keys on namespace AND name, so two same-named
// workshops of one person in two namespaces are two targets, not one seen
// twice. Each is recorded under the spelling a person could say back: bare at
// home, namespace/name away.
func TestReconcile_WildcardCloseSeparatesSameNamedWorkshopsInTwoNamespaces(t *testing.T) {
	sess := builderSession("builder-wild6", "a1b2c3d4-e5f6-7890-abcd-ef1234567934")
	ws := sanctionedWorkshop(sess)
	withCloseRequest(ws, spiceboxv1alpha1.WorkshopCloseTargetAll)

	nearWS, farWS, nearSess, farSess := twinTargets(t, "builder-twin")

	ft := &fakeTuples{closeAllow: map[string]bool{nearWS.Status.Namespace: true, farWS.Status.Namespace: true}}
	c, r := newFakeReconciler(t, ft, tokens.NewRegistry(), sess, ws, nearWS, nearSess, farWS, farSess)

	primeFinalizer(t, r, ws)
	_, err := reconcileWorkshop(t, r, ws)
	require.NoError(t, err)

	got := getWorkshop(t, c, ws)
	near := closeStatusFor(got, nearWS.Name)
	require.NotNil(t, near, "the twin at home is recorded under its bare name: %+v", got.Status.CloseRequests)
	assert.Equal(t, spiceboxv1alpha1.WorkshopClosePhaseClosed, near.Phase)
	far := closeStatusFor(got, closeFarNS+"/"+farWS.Name)
	require.NotNil(t, far, "the twin elsewhere is recorded with its namespace: %+v", got.Status.CloseRequests)
	assert.Equal(t, spiceboxv1alpha1.WorkshopClosePhaseClosed, far.Phase)

	summary := closeStatusFor(got, spiceboxv1alpha1.WorkshopCloseTargetAll)
	require.NotNil(t, summary, "the wildcard itself is answered once its expansion is decided")
	assert.Equal(t, "2 closed, 0 refused", summary.Message, "two same-named workshops are two closes, not one")
	assert.Len(t, ft.closeChecks, 2, "each twin is asked about on its own: %v", ft.closeChecks)
	assert.False(t, sessionExists(t, c, nearSess.Namespace, nearSess.Name))
	assert.False(t, sessionExists(t, c, farSess.Namespace, farSess.Name))
}

// A sequenced wildcard ask ("*:2") is a wildcard too, and it is answered in an
// entry of its OWN. status.closeRequests is keyed by target, so a summary
// written under "*" for a second ask would be a duplicate key the apiserver
// refuses — and the person who asked again would be told nothing at all.
//
// This second ask has nothing NEW to decide: the person's one other workshop
// was decided by the first ask, and a decision is made once per target and
// never revised. So it is answered by saying that — not by the words a person
// with no other workshops at all reads — and nothing is re-checked or
// re-closed.
func TestReconcile_ASequencedWildcardAskIsAnsweredInItsOwnEntry(t *testing.T) {
	sess := builderSession("builder-wild7", "a1b2c3d4-e5f6-7890-abcd-ef1234567935")
	ws := sanctionedWorkshop(sess)
	withCloseRequest(ws, spiceboxv1alpha1.WorkshopCloseTargetAll)
	otherWS, otherSess := closeTarget("builder-seq", "f1e2d3c4-b5a6-7890-abcd-ef1234567935")

	ft := &fakeTuples{closeAllow: map[string]bool{otherWS.Status.Namespace: true}}
	c, r := newFakeReconciler(t, ft, tokens.NewRegistry(), sess, ws, otherWS, otherSess)
	ctx := context.Background()

	primeFinalizer(t, r, ws)
	_, err := reconcileWorkshop(t, r, ws)
	require.NoError(t, err)
	got := getWorkshop(t, c, ws)
	first := closeStatusFor(got, spiceboxv1alpha1.WorkshopCloseTargetAll)
	require.NotNil(t, first, "precondition: the first ask is answered")
	require.Equal(t, "1 closed, 0 refused", first.Message, "precondition: the first ask closed the one other workshop")
	expanded := closeStatusFor(got, otherWS.Name)
	require.NotNil(t, expanded, "precondition: the first ask decided the person's other workshop")
	assert.Equal(t, spiceboxv1alpha1.WorkshopCloseTargetAll, expanded.Ask,
		"a decision a wildcard ask made carries the ask that made it, or no later ask can tell it from its own")
	assert.Empty(t, first.Ask,
		"the summary IS its ask, and its target says so; stamping it would list the ask itself as a workshop")

	got = withCloseRequest(got, spiceboxv1alpha1.WorkshopCloseTargetAll+":2")
	require.NoError(t, c.Update(ctx, got))

	_, err = reconcileWorkshop(t, r, got)
	require.NoError(t, err)

	got = getWorkshop(t, c, ws)
	second := closeStatusFor(got, spiceboxv1alpha1.WorkshopCloseTargetAll+":2")
	require.NotNil(t, second, "the second ask is answered under its own target: %+v", got.Status.CloseRequests)
	assert.Equal(t, spiceboxv1alpha1.WorkshopClosePhaseNotFound, second.Phase,
		"an ask whose expansion holds nothing it may decide found nothing of its own")
	assert.Equal(t, "nothing new to close: the rest were answered by an earlier ask", second.Message,
		"the words a person reads when a fresh ask has nothing left that has not already been answered")
	stillFirst := closeStatusFor(got, otherWS.Name)
	require.NotNil(t, stillFirst)
	assert.Equal(t, spiceboxv1alpha1.WorkshopCloseTargetAll, stillFirst.Ask,
		"the first ask's decision is left exactly as it was, ask and all")

	perTarget := map[string]int{}
	for _, entry := range got.Status.CloseRequests {
		perTarget[entry.Target]++
	}
	for target, n := range perTarget {
		assert.Equal(t, 1, n, "one entry per target, or the status list is not a map: %q has %d", target, n)
	}
	assert.Len(t, ft.closeChecks, 1, "a workshop the first ask decided must not be re-checked: %v", ft.closeChecks)
}

// createCloseTarget materializes a provisioned target part-way through a test —
// a workshop the person opened AFTER an earlier ask was answered. Status is
// written on its own because the fake client serves Workshop's status as a
// subresource, and Create drops it.
func createCloseTarget(t *testing.T, c client.Client, ws *spiceboxv1alpha1.Workshop, sess *spiceboxv1alpha1.AgentSession) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, c.Create(ctx, sess), "create builder session %s/%s", sess.Namespace, sess.Name)
	status := ws.Status
	require.NoError(t, c.Create(ctx, ws), "create Workshop %s/%s", ws.Namespace, ws.Name)
	ws.Status = status
	require.NoError(t, c.Status().Update(ctx, ws), "stamp status on Workshop %s/%s", ws.Namespace, ws.Name)
}

// A second wildcard ask decides what the first one could not have: the
// workshops the person opened since. Everything the first ask already settled
// is SKIPPED — not re-checked, not re-closed, and not counted into the second
// ask's summary, which is the whole of what that summary claims.
//
// The refused target is the case that makes skipping load-bearing rather than
// tidy. A decision is written once per target and never revised, so a workshop
// refused by the first ask stays refused; counting it again would tell the
// person their fresh ask refused something it never looked at, and re-deciding
// it would reach through to whatever session carries that name now.
func TestReconcile_ASequencedWildcardAskDecidesOnlyWhatIsNewSinceTheFirst(t *testing.T) {
	sess := builderSession("builder-wild8", "a1b2c3d4-e5f6-7890-abcd-ef1234567936")
	ws := sanctionedWorkshop(sess)
	withCloseRequest(ws, spiceboxv1alpha1.WorkshopCloseTargetAll)

	mineWS, mineSess := closeTarget("builder-mine", "f1e2d3c4-b5a6-7890-abcd-ef1234567936")
	notMineWS, notMineSess := closeTarget("builder-notmine", "a9b8c7d6-e5f4-7890-abcd-ef1234567937")

	ft := &fakeTuples{closeAllow: map[string]bool{mineWS.Status.Namespace: true, notMineWS.Status.Namespace: false}}
	c, r := newFakeReconciler(t, ft, tokens.NewRegistry(), sess, ws, mineWS, mineSess, notMineWS, notMineSess)
	ctx := context.Background()

	primeFinalizer(t, r, ws)
	_, err := reconcileWorkshop(t, r, ws)
	require.NoError(t, err)

	got := getWorkshop(t, c, ws)
	firstSummary := closeStatusFor(got, spiceboxv1alpha1.WorkshopCloseTargetAll)
	require.NotNil(t, firstSummary, "precondition: the first ask is answered")
	require.Equal(t, "1 closed, 1 refused", firstSummary.Message, "precondition: one closed, one refused as not theirs")
	require.False(t, sessionExists(t, c, mineSess.Namespace, mineSess.Name))
	require.True(t, sessionExists(t, c, notMineSess.Namespace, notMineSess.Name))

	// The person opens another workshop, then asks again.
	freshWS, freshSess := closeTarget("builder-fresh", "c3d4e5f6-a7b8-7890-abcd-ef1234567938")
	ft.closeAllow[freshWS.Status.Namespace] = true
	createCloseTarget(t, c, freshWS, freshSess)

	got = withCloseRequest(got, spiceboxv1alpha1.WorkshopCloseTargetAll+":2")
	require.NoError(t, c.Update(ctx, got))

	_, err = reconcileWorkshop(t, r, got)
	require.NoError(t, err)

	got = getWorkshop(t, c, ws)
	fresh := closeStatusFor(got, freshWS.Name)
	require.NotNil(t, fresh, "the second ask reaches the workshop opened since: %+v", got.Status.CloseRequests)
	assert.Equal(t, spiceboxv1alpha1.WorkshopClosePhaseClosed, fresh.Phase)
	assert.Equal(t, spiceboxv1alpha1.WorkshopCloseTargetAll+":2", fresh.Ask,
		"a decision carries the ask that made it, which is how the tool reads back its own answer")
	assert.False(t, sessionExists(t, c, freshSess.Namespace, freshSess.Name), "the new workshop is ended")

	second := closeStatusFor(got, spiceboxv1alpha1.WorkshopCloseTargetAll+":2")
	require.NotNil(t, second, "the second ask is answered under its own target")
	assert.Equal(t, spiceboxv1alpha1.WorkshopClosePhaseClosed, second.Phase)
	assert.Equal(t, "1 closed, 0 refused", second.Message,
		"only what THIS ask decided is counted; the first ask's close and refusal are not re-reported")

	refused := closeStatusFor(got, notMineWS.Name)
	require.NotNil(t, refused, "the first ask's refusal is still recorded")
	assert.Equal(t, spiceboxv1alpha1.WorkshopClosePhaseRefused, refused.Phase, "and is never revised")
	assert.Equal(t, spiceboxv1alpha1.WorkshopCloseTargetAll, refused.Ask, "under the ask that made it")
	assert.True(t, sessionExists(t, c, notMineSess.Namespace, notMineSess.Name),
		"a refused workshop is not re-decided by a later ask, so nothing of it is ended")

	perTarget := map[string]int{}
	for _, entry := range got.Status.CloseRequests {
		perTarget[entry.Target]++
	}
	for target, n := range perTarget {
		assert.Equal(t, 1, n, "one entry per target, or the status list is not a map: %q has %d", target, n)
	}
	assert.Len(t, ft.closeChecks, 3,
		"three checks in all: the first ask's two targets, and the one workshop the second ask found: %v", ft.closeChecks)
}

// A held ask's summary counts what THAT ASK DECIDED, which is not the same as
// what its expansion still holds. Closing a workshop deletes its builder
// session, and its Workshop carries that session's owner reference, so the
// Workshop is garbage-collected moments later and the next pass no longer sees
// it. A summary recomputed from the current expansion would tell the person
// "1 closed" beside a list of two.
//
// The fake client runs no garbage collector, so the cascade a real cluster
// performs is done here by hand: the test deletes the closed target's Workshop
// between passes, which is exactly the state the next pass would find.
func TestReconcile_AHeldWildcardSummaryCountsTheTargetsItClosedOnEarlierPasses(t *testing.T) {
	sess := builderSession("builder-wild9", "a1b2c3d4-e5f6-7890-abcd-ef1234567940")
	ws := sanctionedWorkshop(sess)
	withCloseRequest(ws, spiceboxv1alpha1.WorkshopCloseTargetAll)

	readyWS, readySess := closeTarget("builder-gone", "f1e2d3c4-b5a6-7890-abcd-ef1234567940")
	slowWS, slowSess := closeTarget("builder-late", "a9b8c7d6-e5f4-7890-abcd-ef1234567941")
	slowNS := slowWS.Status.Namespace
	stillProvisioning(slowWS) // its close tuples cannot exist yet

	ft := &fakeTuples{closeAllow: map[string]bool{readyWS.Status.Namespace: true, slowNS: true}}
	c, r := newFakeReconciler(t, ft, tokens.NewRegistry(), sess, ws, readyWS, readySess, slowWS, slowSess)
	ctx := context.Background()

	primeFinalizer(t, r, ws)
	_, err := reconcileWorkshop(t, r, ws)
	require.NoError(t, err)

	got := getWorkshop(t, c, ws)
	require.Nil(t, closeStatusFor(got, spiceboxv1alpha1.WorkshopCloseTargetAll),
		"precondition: the summary is held while one target is still provisioning")
	require.NotNil(t, closeStatusFor(got, readyWS.Name), "precondition: the ready target was closed on this pass")
	require.False(t, sessionExists(t, c, readySess.Namespace, readySess.Name))

	// The cascade a real cluster runs: the closed target's builder session is
	// gone, so its Workshop goes too and leaves the expansion.
	require.NoError(t, c.Delete(ctx, readyWS), "delete the closed target's Workshop the way owner-ref GC would")

	_, err = reconcileWorkshop(t, r, got)
	require.NoError(t, err)
	got = getWorkshop(t, c, ws)
	require.Nil(t, closeStatusFor(got, spiceboxv1alpha1.WorkshopCloseTargetAll),
		"the summary is still held: the slow target has not become decidable")

	// The slow workshop finishes provisioning; the next pass decides it and
	// summarizes what the whole ask did.
	var slow spiceboxv1alpha1.Workshop
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: slowWS.Namespace, Name: slowWS.Name}, &slow))
	slow.Status.Namespace = slowNS
	slow.Status.Phase = spiceboxv1alpha1.WorkshopPhaseReady
	require.NoError(t, c.Status().Update(ctx, tuplesWritten(&slow)))

	_, err = reconcileWorkshop(t, r, got)
	require.NoError(t, err)

	got = getWorkshop(t, c, ws)
	summary := closeStatusFor(got, spiceboxv1alpha1.WorkshopCloseTargetAll)
	require.NotNil(t, summary, "once every target is decided the ask is answered")
	assert.Equal(t, spiceboxv1alpha1.WorkshopClosePhaseClosed, summary.Phase)
	assert.Equal(t, "2 closed, 0 refused", summary.Message,
		"a workshop this ask closed before it was GC'd is still one of this ask's closes")
	for _, target := range []string{readyWS.Name, slowWS.Name} {
		entry := closeStatusFor(got, target)
		require.NotNil(t, entry, "the ask's decision for %s stands: %+v", target, got.Status.CloseRequests)
		assert.Equal(t, spiceboxv1alpha1.WorkshopCloseTargetAll, entry.Ask,
			"every workshop this ask decided carries the ask, which is what the summary counts")
	}
	assert.False(t, sessionExists(t, c, slowSess.Namespace, slowSess.Name))
}

// A named request for a workshop a HELD ask already closed is answered by
// copying that decision under its own spelling — and copying must not take the
// target away from the ask that decided it. The copy carries no ask (it is a
// named request's own entry), so a decided map that let it overwrite the ask's
// entry would make the held ask skip its own decision and undercount it.
func TestReconcile_ASecondSpellingDoesNotTakeATargetFromTheAskThatDecidedIt(t *testing.T) {
	sess := builderSession("builder-wild10", "a1b2c3d4-e5f6-7890-abcd-ef1234567942")
	ws := sanctionedWorkshop(sess)
	readyWS, readySess := closeTarget("builder-both", "f1e2d3c4-b5a6-7890-abcd-ef1234567942")
	slowWS, slowSess := closeTarget("builder-later", "a9b8c7d6-e5f4-7890-abcd-ef1234567943")
	slowNS := slowWS.Status.Namespace
	stillProvisioning(slowWS)

	// The ask first, then the same workshop named in the other spelling: the
	// person asked to close everything and then named one of them by hand.
	withCloseRequest(ws, spiceboxv1alpha1.WorkshopCloseTargetAll)
	withCloseRequest(ws, "default/"+readyWS.Name)

	ft := &fakeTuples{closeAllow: map[string]bool{readyWS.Status.Namespace: true, slowNS: true}}
	c, r := newFakeReconciler(t, ft, tokens.NewRegistry(), sess, ws, readyWS, readySess, slowWS, slowSess)
	ctx := context.Background()

	primeFinalizer(t, r, ws)
	_, err := reconcileWorkshop(t, r, ws)
	require.NoError(t, err)

	got := getWorkshop(t, c, ws)
	require.Nil(t, closeStatusFor(got, spiceboxv1alpha1.WorkshopCloseTargetAll), "precondition: the summary is held")
	copied := closeStatusFor(got, "default/"+readyWS.Name)
	require.NotNil(t, copied, "precondition: the named spelling is answered from the ask's decision")
	assert.Empty(t, copied.Ask, "a named request's entry is its own, whichever request decided the target")

	var slow spiceboxv1alpha1.Workshop
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: slowWS.Namespace, Name: slowWS.Name}, &slow))
	slow.Status.Namespace = slowNS
	slow.Status.Phase = spiceboxv1alpha1.WorkshopPhaseReady
	require.NoError(t, c.Status().Update(ctx, tuplesWritten(&slow)))

	_, err = reconcileWorkshop(t, r, got)
	require.NoError(t, err)

	got = getWorkshop(t, c, ws)
	summary := closeStatusFor(got, spiceboxv1alpha1.WorkshopCloseTargetAll)
	require.NotNil(t, summary, "the held ask is answered once its last target is decided")
	assert.Equal(t, "2 closed, 0 refused", summary.Message,
		"the ask decided both workshops; a copy made for a second spelling changes neither count")
	assert.Len(t, ft.closeChecks, 2, "neither workshop is checked twice: %v", ft.closeChecks)
	assert.False(t, sessionExists(t, c, slowSess.Namespace, slowSess.Name))
	assert.False(t, sessionExists(t, c, readySess.Namespace, readySess.Name))
}

// Two kinds of nothing, and a person needs to be able to tell them apart: an
// ask that found no other workshop at all, and an ask whose every target had
// already been answered — by an earlier ask or by a named request. The first
// means they are done; the second means the answer they want is the one they
// already have.
func TestReconcile_AWildcardAskWithNothingToDecideSaysWhichKindOfNothing(t *testing.T) {
	t.Run("every other workshop was already answered: NotFound, and says an earlier ask answered them", func(t *testing.T) {
		sess := builderSession("builder-wild11", "a1b2c3d4-e5f6-7890-abcd-ef1234567944")
		ws := sanctionedWorkshop(sess)
		withCloseRequest(ws, spiceboxv1alpha1.WorkshopCloseTargetAll)
		otherWS, otherSess := closeTarget("builder-answered", "f1e2d3c4-b5a6-7890-abcd-ef1234567944")

		ft := &fakeTuples{closeAllow: map[string]bool{otherWS.Status.Namespace: false}}
		c, r := newFakeReconciler(t, ft, tokens.NewRegistry(), sess, ws, otherWS, otherSess)
		ctx := context.Background()

		primeFinalizer(t, r, ws)
		_, err := reconcileWorkshop(t, r, ws)
		require.NoError(t, err)
		got := getWorkshop(t, c, ws)
		require.NotNil(t, closeStatusFor(got, otherWS.Name), "precondition: the first ask answered the one other workshop")

		got = withCloseRequest(got, spiceboxv1alpha1.WorkshopCloseTargetAll+":2")
		require.NoError(t, c.Update(ctx, got))
		_, err = reconcileWorkshop(t, r, got)
		require.NoError(t, err)

		got = getWorkshop(t, c, ws)
		second := closeStatusFor(got, spiceboxv1alpha1.WorkshopCloseTargetAll+":2")
		require.NotNil(t, second, "the second ask is answered under its own target: %+v", got.Status.CloseRequests)
		assert.Equal(t, spiceboxv1alpha1.WorkshopClosePhaseNotFound, second.Phase)
		assert.Equal(t, "nothing new to close: the rest were answered by an earlier ask", second.Message,
			"the person still HAS a workshop; what they do not have is a new answer about it")
		assert.True(t, sessionExists(t, c, otherSess.Namespace, otherSess.Name), "nothing is re-decided, so nothing is ended")
	})

	t.Run("no other workshop is open at all: NotFound, 'no other workshops of yours'", func(t *testing.T) {
		sess := builderSession("builder-wild12", "a1b2c3d4-e5f6-7890-abcd-ef1234567945")
		ws := sanctionedWorkshop(sess)
		withCloseRequest(ws, spiceboxv1alpha1.WorkshopCloseTargetAll)

		ft := &fakeTuples{}
		c, r := newFakeReconciler(t, ft, tokens.NewRegistry(), sess, ws)

		primeFinalizer(t, r, ws)
		_, err := reconcileWorkshop(t, r, ws)
		require.NoError(t, err)

		got := getWorkshop(t, c, ws)
		summary := closeStatusFor(got, spiceboxv1alpha1.WorkshopCloseTargetAll)
		require.NotNil(t, summary)
		assert.Equal(t, spiceboxv1alpha1.WorkshopClosePhaseNotFound, summary.Phase)
		assert.Equal(t, "no other workshops of yours", summary.Message,
			"an ask that found nothing at all is not an ask whose targets were already answered")
	})
}

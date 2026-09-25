package agentsession

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
)

// TestMapCredentialUpdateRequestToSession pins "the watch actually enqueuing":
// a CredentialUpdateRequest change must re-enqueue exactly the AgentSession
// its spec.sessionRef names. Reverting mapCredentialUpdateRequestToSession to
// return nil unconditionally (or to read the wrong field) makes this fail.
func TestMapCredentialUpdateRequestToSession(t *testing.T) {
	cur := &spiceboxv1alpha1.CredentialUpdateRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-cur", Namespace: "demo-ns"},
		Spec: spiceboxv1alpha1.CredentialUpdateRequestSpec{
			SessionRef: spiceboxv1alpha1.NamespacedRef{Namespace: "demo-ns", Name: "demo-session"},
		},
	}

	got := mapCredentialUpdateRequestToSession(context.Background(), cur)

	require.Len(t, got, 1, "exactly one AgentSession must be enqueued")
	assert.Equal(t, "demo-ns", got[0].Namespace)
	assert.Equal(t, "demo-session", got[0].Name)
}

func TestMapCredentialUpdateRequestToSession_NoSessionRefEnqueuesNothing(t *testing.T) {
	cur := &spiceboxv1alpha1.CredentialUpdateRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-cur", Namespace: "demo-ns"},
	}
	got := mapCredentialUpdateRequestToSession(context.Background(), cur)
	assert.Nil(t, got, "a request naming no session has nothing to enqueue")
}

func TestMapCredentialUpdateRequestToSession_WrongObjectTypeEnqueuesNothing(t *testing.T) {
	got := mapCredentialUpdateRequestToSession(context.Background(), &spiceboxv1alpha1.AgentSession{})
	assert.Nil(t, got)
}

// newParkTestReconciler builds a fake-client-backed Reconciler with just
// enough wiring for reconcileCredentialUpdatePark / reconcileHold +
// applyStatus to run. SessionHold carries its own status subresource
// (reconcileHold stamps TrippedAt via Client.Status().Update) alongside
// AgentSession's. RunnerFactory is wired to a PodRunnerFactory over the same
// fake client -- reconcileHold's reap step (via reapSessionPods) calls
// RunnerFactory.Stop, and PodRunnerFactory.Stop needs only a client (see
// pod_runner_factory.go), the same minimal wiring sleepFixture/reapFixture
// use for the identical "park the session, drop its compute" reap call.
func newParkTestReconciler(t *testing.T, objs ...client.Object) (client.Client, *Reconciler) {
	t.Helper()
	scheme := testfixtures.NewScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}, &spiceboxv1alpha1.SessionHold{}).
		Build()
	r := &Reconciler{Client: c}
	r.RunnerFactory = &PodRunnerFactory{Client: c}
	return c, r
}

func openCUR(name, sessNS, sessName string) *spiceboxv1alpha1.CredentialUpdateRequest {
	return curInPhase(name, sessNS, sessName, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen)
}

func curInPhase(name, sessNS, sessName, phase string) *spiceboxv1alpha1.CredentialUpdateRequest {
	return &spiceboxv1alpha1.CredentialUpdateRequest{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: sessNS},
		Spec:       spiceboxv1alpha1.CredentialUpdateRequestSpec{SessionRef: spiceboxv1alpha1.NamespacedRef{Namespace: sessNS, Name: sessName}},
		Status:     spiceboxv1alpha1.CredentialUpdateRequestStatus{Phase: phase},
	}
}

// TestReconcileCredentialUpdatePark_OpenRequestForcesAwaitingCredentials pins
// the park half: an Open CredentialUpdateRequest for this session must force
// phase=AwaitingCredentials and short-circuit (proceed=false) so the caller
// never reaches derivePhase, which would otherwise overwrite it back to
// Running on this exact same reconcile pass.
func TestReconcileCredentialUpdatePark_OpenRequestForcesAwaitingCredentials(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-session", Namespace: "demo-ns"},
		Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
	}
	cur := openCUR("demo-cur", "demo-ns", "demo-session")
	c, r := newParkTestReconciler(t, sess, cur)

	_, proceed, err := r.reconcileCredentialUpdatePark(context.Background(), sess)
	require.NoError(t, err)
	assert.False(t, proceed, "an Open request must short-circuit the rest of Reconcile")
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials, sess.Status.Phase)
	assert.True(t, conditions.IsTrue(sess.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionCredentialUpdatePending))

	var persisted spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(sess), &persisted))
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials, persisted.Status.Phase, "the park must be persisted, not only held in memory")
}

// TestReconcileCredentialUpdatePark_NoOpenRequestLeavesSessionAlone proves the
// converse: a session with no Open request and no prior park marker is
// untouched (proceed=true, phase unchanged) -- reconcileCredentialUpdatePark
// must not interfere with a session it has no business in.
func TestReconcileCredentialUpdatePark_NoOpenRequestLeavesSessionAlone(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-session", Namespace: "demo-ns"},
		Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
	}
	_, r := newParkTestReconciler(t, sess)

	_, proceed, err := r.reconcileCredentialUpdatePark(context.Background(), sess)
	require.NoError(t, err)
	assert.True(t, proceed)
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseRunning, sess.Status.Phase, "phase must not be touched when nothing is Open")
}

// TestReconcileCredentialUpdatePark_UnparkClearsMarkerButNotPhase covers the
// unpark leg: once the request has resolved (no longer returned by
// awaitingCredentialUpdateRequestFor), the CredentialUpdatePending marker must
// clear -- but Phase is deliberately left for derivePhase to compute, so this
// function must NOT set it to Running (or anything else) itself.
func TestReconcileCredentialUpdatePark_UnparkClearsMarkerButNotPhase(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-session", Namespace: "demo-ns"},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase: spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials,
		},
	}
	conditions.SetTrue(sess, &sess.Status.Conditions,
		spiceboxv1alpha1.AgentSessionConditionCredentialUpdatePending, spiceboxv1alpha1.ReasonCredentialUpdateRequested)
	_, r := newParkTestReconciler(t, sess)
	// No CredentialUpdateRequest objects at all: the one that parked this
	// session already resolved (Fulfilled/Expired) or was deleted.

	_, proceed, err := r.reconcileCredentialUpdatePark(context.Background(), sess)
	require.NoError(t, err)
	assert.True(t, proceed, "unpark must fall through so derivePhase runs")
	assert.False(t, conditions.IsTrue(sess.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionCredentialUpdatePending),
		"the marker must clear once no Open request remains")
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials, sess.Status.Phase,
		"phase is left for derivePhase to compute -- this function must not guess Running")
}

// TestReconcileCredentialUpdatePark_TerminalSessionIsNoOp proves a
// Succeeded/Failed session is never parked or unparked -- isTerminalPhase
// short-circuits before any List or status write.
func TestReconcileCredentialUpdatePark_TerminalSessionIsNoOp(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-session", Namespace: "demo-ns"},
		Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseSucceeded},
	}
	cur := openCUR("demo-cur", "demo-ns", "demo-session")
	_, r := newParkTestReconciler(t, sess, cur)

	_, proceed, err := r.reconcileCredentialUpdatePark(context.Background(), sess)
	require.NoError(t, err)
	assert.True(t, proceed)
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseSucceeded, sess.Status.Phase, "a terminal session must never be parked")
}

func TestAwaitingCredentialUpdateRequestFor(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "demo-session", Namespace: "demo-ns"}}
	other := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "other-session", Namespace: "demo-ns"}}

	cases := []struct {
		name    string
		objs    []client.Object
		wantNil bool
	}{
		{
			name:    "an Open request for this session is found",
			objs:    []client.Object{sess, openCUR("cur-1", "demo-ns", "demo-session")},
			wantNil: false,
		},
		{
			// A collapsed request has no card of its OWN, but its agent is blocked
			// in the identical meta-tool call on the identical credential -- the
			// tool waits on non-terminal, not on Open. Missing this leaves the
			// session reading Running while its runner is stuck.
			name:    "a Collapsed request for this session is found: its agent is blocked just as hard",
			objs:    []client.Object{sess, curInPhase("cur-1", "demo-ns", "demo-session", spiceboxv1alpha1.CredentialUpdateRequestPhaseCollapsed)},
			wantNil: false,
		},
		{
			name:    "a Pending request for this session is ignored: nothing has been determined yet",
			objs:    []client.Object{sess, curInPhase("cur-1", "demo-ns", "demo-session", spiceboxv1alpha1.CredentialUpdateRequestPhasePending)},
			wantNil: true,
		},
		{
			name:    "a Refused request for this session is ignored",
			objs:    []client.Object{sess, curInPhase("cur-1", "demo-ns", "demo-session", spiceboxv1alpha1.CredentialUpdateRequestPhaseRefused)},
			wantNil: true,
		},
		{
			name:    "a Fulfilled request for this session is ignored",
			objs:    []client.Object{sess, curInPhase("cur-1", "demo-ns", "demo-session", spiceboxv1alpha1.CredentialUpdateRequestPhaseFulfilled)},
			wantNil: true,
		},
		{
			name:    "an Expired request for this session is ignored",
			objs:    []client.Object{sess, curInPhase("cur-1", "demo-ns", "demo-session", spiceboxv1alpha1.CredentialUpdateRequestPhaseExpired)},
			wantNil: true,
		},
		{
			name:    "an unrecognized phase is ignored: an allowlist must never park a session forever on a typo",
			objs:    []client.Object{sess, curInPhase("cur-1", "demo-ns", "demo-session", "SomeFuturePhase")},
			wantNil: true,
		},
		{
			name:    "an Open request for a DIFFERENT session in the same namespace is ignored",
			objs:    []client.Object{sess, other, openCUR("cur-1", "demo-ns", "other-session")},
			wantNil: true,
		},
		{
			name:    "a Collapsed request for a DIFFERENT session in the same namespace is ignored",
			objs:    []client.Object{sess, other, curInPhase("cur-1", "demo-ns", "other-session", spiceboxv1alpha1.CredentialUpdateRequestPhaseCollapsed)},
			wantNil: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, r := newParkTestReconciler(t, tc.objs...)
			got, err := r.awaitingCredentialUpdateRequestFor(context.Background(), sess)
			require.NoError(t, err)
			if tc.wantNil {
				assert.Nil(t, got)
			} else {
				require.NotNil(t, got)
			}
		})
	}
}

// TestReconcileCredentialUpdatePark_CollapsedParksAndSettlingUnparks is the
// session half of "when the canonical settles, every follower settles with it".
//
// A follower session is parked by a request that has no card at all -- somebody
// else's card is the one in front of a human -- and it must be released the
// moment that follower reaches a terminal phase. The two halves are asserted in
// ONE test, against the SAME session object, so a park that never happens can
// never make the unpark leg pass vacuously: the unpark is only meaningful if
// the session was demonstrably parked first.
func TestReconcileCredentialUpdatePark_CollapsedParksAndSettlingUnparks(t *testing.T) {
	settledPhases := []string{
		spiceboxv1alpha1.CredentialUpdateRequestPhaseFulfilled,
		spiceboxv1alpha1.CredentialUpdateRequestPhaseRefused,
		spiceboxv1alpha1.CredentialUpdateRequestPhaseExpired,
	}
	for _, settled := range settledPhases {
		t.Run("Collapsed parks, then "+settled+" unparks", func(t *testing.T) {
			sess := &spiceboxv1alpha1.AgentSession{
				ObjectMeta: metav1.ObjectMeta{Name: "demo-session", Namespace: "demo-ns"},
				Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
			}
			follower := curInPhase("demo-cur", "demo-ns", "demo-session",
				spiceboxv1alpha1.CredentialUpdateRequestPhaseCollapsed)
			follower.Status.CollapsedInto = &spiceboxv1alpha1.NamespacedRef{Namespace: "demo-ns", Name: "demo-cur-canonical"}
			c, r := newParkTestReconciler(t, sess, follower)

			_, proceed, err := r.reconcileCredentialUpdatePark(context.Background(), sess)
			require.NoError(t, err)
			require.False(t, proceed,
				"a collapsed request must short-circuit the rest of Reconcile exactly like an Open one")
			require.Equal(t, spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials, sess.Status.Phase,
				"a session waiting on somebody else's card is still waiting on credentials, not Running")
			require.True(t, conditions.IsTrue(sess.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionCredentialUpdatePending))

			// The follower settles with its canonical.
			var settledFollower spiceboxv1alpha1.CredentialUpdateRequest
			require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(follower), &settledFollower))
			settledFollower.Status.Phase = settled
			require.NoError(t, c.Update(context.Background(), &settledFollower))

			_, proceed, err = r.reconcileCredentialUpdatePark(context.Background(), sess)
			require.NoError(t, err)
			assert.True(t, proceed, "unpark must fall through so derivePhase recomputes the real phase")
			assert.False(t, conditions.IsTrue(sess.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionCredentialUpdatePending),
				"a follower reaching %s releases its session -- otherwise the agent is unblocked but the "+
					"session still reads AwaitingCredentials forever", settled)
		})
	}
}

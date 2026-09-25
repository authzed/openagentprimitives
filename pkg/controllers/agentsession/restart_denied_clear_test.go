package agentsession_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	memorypkg "github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
)

// restart_denied_clear_test.go pins RestartDenied as CURRENT STATE rather than a
// latch recording "a denial happened here once".
//
// The condition was written True by the fork gate and cleared by nothing — no
// reconcile path set it False or removed it. Two consequences, both wrong:
//
//  1. A session denied once reads as denied forever, including after a LATER
//     continuation of it succeeds. The monitoring Targets table is one `Rule`
//     row away from that becoming a channel-flooding bug — the same shape that
//     made AgentSession/Failed need `Terminal: true`.
//
//  2. The user-visible half: meta.SetStatusCondition moves LastTransitionTime
//     ONLY when the Status changes. A second denial is True→True, so the stamp
//     stays frozen at the first denial indefinitely — and channelsd's relay
//     dedups on exactly that stamp. The user's second attempt was acked
//     ("picking it up in a new session") and then silently swallowed, which is
//     the precise failure the relay was built to prevent.
//
// Modeling it honestly fixes both: arming a restart means the verdict is pending
// again, so the operator clears any prior verdict when it picks up a new
// PendingRestart. A denial is then always a real False→True transition.

// armedSession returns a parked session carrying a fresh PendingRestart — the
// shape channelsd writes when a user replies in the thread.
func armedSession(target string) *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "demo"},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase: spiceboxv1alpha1.AgentSessionPhaseIdle,
			PendingRestart: &spiceboxv1alpha1.PendingRestart{
				CutTurnIndex:      1,
				NewUserText:       "please continue",
				TriggeredBy:       "user:bob",
				TargetSessionName: target,
			},
		},
	}
}

// priorDenial is the verdict left by an EARLIER attempt. The timestamp is
// deliberately old: the fix's False→True transition must re-stamp it, and
// seeding it in the past makes that assertion deterministic rather than racing
// a one-second-resolution clock inside the test.
func priorDenial() metav1.Condition {
	return metav1.Condition{
		Type:               spiceboxv1alpha1.AgentSessionConditionRestartDenied,
		Status:             metav1.ConditionTrue,
		Reason:             spiceboxv1alpha1.ReasonForkNotAuthorized,
		Message:            "denied an hour ago",
		LastTransitionTime: metav1.NewTime(time.Now().Add(-time.Hour).UTC()),
	}
}

// restartReconciler builds a Reconciler over a fake client holding sess, with
// the fork gate set to allow or deny.
//
// It signs sess's PendingRestart as the connector would: ReconcileRestart
// refuses a marker it cannot attribute to channelsd, so an unsigned fixture
// would never reach the fork gate these tests are about.
func restartReconciler(t *testing.T, sess *spiceboxv1alpha1.AgentSession, allow bool) (*agentsession.Reconciler, client.Client) {
	t.Helper()
	armSignedRestart(t, sess)
	c := fake.NewClientBuilder().
		WithScheme(restartScheme(t)).
		WithObjects(sess, ungatedClass("ns", "demo")).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		Build()
	return &agentsession.Reconciler{
		Client:            c,
		RestartMemory:     memorypkg.NewLocal(inmem.NewBackend()),
		AuthzGranter:      &fakeGranter{},
		Snapshotter:       &fakeSnapshotter{},
		ForkChecker:       fakeForkChecker{allow: allow},
		DeniedLister:      fakeDeniedLister{},
		ForkNoticePublish: func(_ context.Context, _, _, _, _ string) error { return nil },
		PublisherKeys:     testMarkerKeys,
	}, c
}

// Named for this file rather than the generic loadSession: an envtest-tagged
// file in this same package already owns that name, and mage test:unit does not
// compile it, so the collision only surfaces under mage test:integration.
func loadDeniedSession(t *testing.T, ctx context.Context, c client.Client) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "p"}, &got))
	return &got
}

func restartDeniedCond(t *testing.T, sess *spiceboxv1alpha1.AgentSession) *metav1.Condition {
	t.Helper()
	cond := meta.FindStatusCondition(sess.Status.Conditions,
		spiceboxv1alpha1.AgentSessionConditionRestartDenied)
	require.NotNil(t, cond, "RestartDenied condition must be present")
	return cond
}

// A session whose later continuation SUCCEEDS must not still read as denied.
func TestReconcileRestart_ArmingANewAttemptClearsAPriorDenial(t *testing.T) {
	ctx := memorypkg.WithSystemApproval(context.Background(), "test")
	sess := armedSession("p-fk2")
	sess.Status.Conditions = []metav1.Condition{priorDenial()}
	r, c := restartReconciler(t, sess, true) // this attempt is ALLOWED

	_, _, err := r.ReconcileRestart(ctx, sess)
	require.NoError(t, err)

	cond := restartDeniedCond(t, loadDeniedSession(t, ctx, c))
	assert.Equal(t, metav1.ConditionFalse, cond.Status,
		"arming a new attempt clears the prior verdict — the condition reports current "+
			"state, and this session's continuation was not denied")
}

// The silent-swallow bug: a second denial must re-stamp LastTransitionTime, or
// channelsd's relay (which dedups on it) never tells the user their retry was
// refused too.
func TestReconcileRestart_SecondDenialRestampsTheCondition(t *testing.T) {
	ctx := memorypkg.WithSystemApproval(context.Background(), "test")
	sess := armedSession("p-fk2")
	stale := priorDenial()
	sess.Status.Conditions = []metav1.Condition{stale}
	r, c := restartReconciler(t, sess, false) // denied again

	_, _, err := r.ReconcileRestart(ctx, sess)
	require.NoError(t, err)

	cond := restartDeniedCond(t, loadDeniedSession(t, ctx, c))
	assert.Equal(t, metav1.ConditionTrue, cond.Status, "this attempt was denied too")
	assert.True(t, cond.LastTransitionTime.After(stale.LastTransitionTime.Time),
		"a second denial must be a fresh False→True transition — left as True→True the "+
			"stamp never moves, and the relay swallows the user's retry")
}

// The gate still records a first-ever denial normally.
func TestReconcileRestart_FirstDenialSetsTheCondition(t *testing.T) {
	ctx := memorypkg.WithSystemApproval(context.Background(), "test")
	sess := armedSession("p-fk1")
	r, c := restartReconciler(t, sess, false)

	_, _, err := r.ReconcileRestart(ctx, sess)
	require.NoError(t, err)

	cond := restartDeniedCond(t, loadDeniedSession(t, ctx, c))
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonForkNotAuthorized, cond.Reason)
}

// A REPEAT unverified marker must re-stamp the condition, not sit at True→True.
//
// The marker-verification gate ends in the same RestartDenied verdict the fork
// gate does, so it needs the same prior-verdict clearing: meta.SetStatusCondition
// only moves LastTransitionTime when the Status changes, and channelsd's relay
// dedups on that stamp. Left as True→True, a user whose second attempt is also
// refused is acked and then silently ignored — the exact failure the relay
// exists to prevent, reached by a different route.
func TestReconcileRestart_SecondUnverifiedMarkerRestampsTheCondition(t *testing.T) {
	ctx := memorypkg.WithSystemApproval(context.Background(), "test")
	sess := armedSession("p-fk2")
	stale := priorDenial()
	sess.Status.Conditions = []metav1.Condition{stale}
	// Deliberately unsigned: this attempt is refused by the author gate, not the
	// fork gate, so allow=true proves the refusal came from verification.
	c := fake.NewClientBuilder().
		WithScheme(restartScheme(t)).
		WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		Build()
	r := &agentsession.Reconciler{
		Client:            c,
		RestartMemory:     memorypkg.NewLocal(inmem.NewBackend()),
		AuthzGranter:      &fakeGranter{},
		Snapshotter:       &fakeSnapshotter{},
		ForkChecker:       fakeForkChecker{allow: true},
		DeniedLister:      fakeDeniedLister{},
		ForkNoticePublish: func(_ context.Context, _, _, _, _ string) error { return nil },
		PublisherKeys:     testMarkerKeys,
	}

	_, _, err := r.ReconcileRestart(ctx, sess)
	require.NoError(t, err)

	cond := restartDeniedCond(t, loadDeniedSession(t, ctx, c))
	assert.Equal(t, metav1.ConditionTrue, cond.Status, "this attempt was refused too")
	// Unsigned rather than Unverified: this marker carries no attestation, the
	// upgrade-window shape. Re-stamping matters identically for both classes —
	// a rollout refusal the relay swallows is as silent as a forgery one.
	assert.Equal(t, agentsession.ReasonRestartMarkerUnsigned, cond.Reason)
	assert.True(t, cond.LastTransitionTime.After(stale.LastTransitionTime.Time),
		"a repeat refusal must be a fresh False→True transition, or the relay swallows the user's retry")
}

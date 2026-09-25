package agentsession

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/revocation"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/platform/workspace"
)

// TestReconcileHold_trippingPublishesSessionHoldRevoke proves the fast path
// wiring: the FIRST reconcile of a newly-active hold (the one that stamps
// TrippedAt) must publish a session-hold KindRevoked envelope naming the
// session, so a live runner's ap.revocation subscriber can halt its turn loop
// immediately instead of waiting for reapSessionPods below (which still runs
// regardless -- this is the latency path, not the enforcement).
func TestReconcileHold_trippingPublishesSessionHoldRevoke(t *testing.T) {
	ctx := context.Background()
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "demo"},
		Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
	}
	hold := &spiceboxv1alpha1.SessionHold{
		ObjectMeta: metav1.ObjectMeta{Name: "h1", Namespace: "demo"},
		Spec: spiceboxv1alpha1.SessionHoldSpec{
			SessionRef: spiceboxv1alpha1.NamespacedRef{Namespace: "demo", Name: "s1"},
			Reason:     "denial streak",
			Source:     "tripper/plangate-denial-streak",
		},
	}
	_, r := newParkTestReconciler(t, sess, hold)
	fb := &fakeBus{}
	r.RevokePublisher = revocation.NewPublisher(fb)

	_, proceed, err := r.reconcileHold(ctx, sess)
	require.NoError(t, err)
	assert.False(t, proceed)

	require.Len(t, fb.envelopes, 1, "tripping must publish exactly one session-hold revoke")
	var pl channelevents.RevokedPayload
	require.NoError(t, json.Unmarshal(fb.envelopes[0].Payload, &pl))
	assert.Equal(t, "session-hold", pl.Kind)
	assert.Equal(t, "demo/s1", pl.Key, "key must match sessionhold.Kind's <namespace>/<name>")
	assert.Equal(t, "demo", pl.Scope)
}

// TestReconcileHold_alreadyTrippedDoesNotRepublish proves the trip-once guard:
// a hold whose TrippedAt is already stamped (a later reconcile of an
// already-parked session) must not publish again. The publish sits inside the
// same "stamp the observation ONCE" block as TrippedAt for exactly this
// reason -- see reconcileHold's comment on that guard.
func TestReconcileHold_alreadyTrippedDoesNotRepublish(t *testing.T) {
	ctx := context.Background()
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "demo", UID: "uid-1"},
		Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseHeld},
	}
	trippedAt := metav1.NewTime(time.Now())
	hold := &spiceboxv1alpha1.SessionHold{
		ObjectMeta: metav1.ObjectMeta{Name: "h1", Namespace: "demo"},
		Spec: spiceboxv1alpha1.SessionHoldSpec{
			SessionRef: spiceboxv1alpha1.NamespacedRef{Namespace: "demo", Name: "s1"},
			Reason:     "denial streak",
		},
		Status: spiceboxv1alpha1.SessionHoldStatus{
			Phase:     spiceboxv1alpha1.SessionHoldPhaseActive,
			TrippedAt: &trippedAt,
		},
	}
	// Derived from the same code path production uses (mirrors
	// TestReconcileHold_secondReconcileWithHandleSet_doesNotRelaunchSnapshot),
	// so reconcileHold's ensureHoldSnapshot leg is already satisfied and this
	// test isolates the publish-guard behavior alone.
	hold.Status.SnapshotHandle = workspace.SnapshotJobName((&Reconciler{}).holdSnapshotHandle(sess, hold))

	_, r := newParkTestReconciler(t, sess, hold)
	fb := &fakeBus{}
	r.RevokePublisher = revocation.NewPublisher(fb)

	_, proceed, err := r.reconcileHold(ctx, sess)
	require.NoError(t, err)
	assert.False(t, proceed)
	assert.Zero(t, fb.published, "an already-tripped hold must not re-publish on a later reconcile")
}

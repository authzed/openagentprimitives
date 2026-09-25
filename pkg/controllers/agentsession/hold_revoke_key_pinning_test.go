package agentsession

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/revocation"
	"github.com/authzed/openagentprimitives/pkg/authz/revocation/kinds/sessionhold"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// TestSessionHoldRevokeKey_PublisherAndInvalidatorAgree pins the one thing
// neither side's own unit tests can see: that the KEY STRING this reconciler
// publishes on trip (hold.go's sess.Namespace+"/"+sess.Name) is exactly the
// string the runner's sessionhold.Kind is constructed with
// (internal/cmd/runner/main.go's sessionhold.New(ns+"/"+name, cancel)).
//
// Both sides independently format "<namespace>/<name>" today, and nothing in
// the type system forces them to agree. sessionhold's own unit test
// (TestInvalidate_ignoresAnotherSessionsKey, pkg/authz/revocation/kinds/
// sessionhold) hand-writes the identical literal on both sides of its own
// assertion, and TestReconcileHold_trippingPublishesSessionHoldRevoke (this
// package, hold_revoke_test.go) separately hand-writes "demo/s1" as the
// expected published key. Reformat EITHER side the same way (say, swap to
// "<name>.<namespace>", or reverse the order) and both of those tests keep
// passing — each only checks its own side against a literal it typed, not
// against what the other side actually does. The failure this produces is
// silent and total: the invalidator's key check rejects every envelope the
// reconciler publishes, so the fast path (rootCtx cancellation) stops firing
// for any session, everywhere, with nothing in the unit suites to catch it —
// reapSessionPods still contains the session eventually, so the SessionHold
// integration tests would stay green too.
//
// This test closes the gap by taking the key the RECONCILER actually
// published (captured off a fake bus, not hand-typed here) and feeding it,
// unmodified, into sessionhold.New(...).Invalidate — the real runner-side
// constructor. If either side's formatting ever drifts from the other, this
// fails; it never assumes agreement, it observes and cross-checks it.
func TestSessionHoldRevokeKey_PublisherAndInvalidatorAgree(t *testing.T) {
	ctx := context.Background()
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-session", Namespace: "demo-ns"},
		Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
	}
	hold := &spiceboxv1alpha1.SessionHold{
		ObjectMeta: metav1.ObjectMeta{Name: "h1", Namespace: "demo-ns"},
		Spec: spiceboxv1alpha1.SessionHoldSpec{
			SessionRef: spiceboxv1alpha1.NamespacedRef{Namespace: "demo-ns", Name: "demo-session"},
			Reason:     "pinning test",
			Source:     "manual",
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
	require.Equal(t, "session-hold", pl.Kind)

	// pl.Key is the RECONCILER's own output, not a literal this test wrote.
	// Hand it, unmodified, to the exact constructor the runner calls
	// (internal/cmd/runner/main.go: sessionhold.New(ns+"/"+name, cancel)).
	stopped := 0
	inv := sessionhold.New(pl.Key, func() { stopped++ })
	require.NoError(t, inv.Invalidate(pl.Key))
	assert.Equal(t, 1, stopped,
		"the runner's own invalidator, given the key the reconciler actually published, must recognize its own session")

	// The negative direction matters too: if the two sides' formats had
	// drifted apart, an invalidator built from a DIFFERENTLY-formatted (but
	// self-consistent) key would reject this same envelope, silently
	// killing the fast path while looking, in isolation, like a passing
	// test. Prove the key check is actually discriminating, not a no-op
	// that would pass regardless of what pl.Key contained.
	stopped = 0
	transposed := sess.Name + "/" + sess.Namespace // the plausible <name>/<namespace> swap
	require.NoError(t, sessionhold.New(transposed, func() { stopped++ }).Invalidate(pl.Key))
	assert.Zero(t, stopped,
		"a differently-formatted key must NOT match the reconciler's published key — "+
			"otherwise this test would pass even if the two sides disagreed")
}

package sessionhold

// cascade_watch_test.go tests mapSessionToAncestorHolds directly (package
// sessionhold, white-box) rather than through Reconcile/Decide: it has no
// entry point through either — controller-runtime's manager invokes it
// directly as a Watches(...) mapping function, which nothing in this
// package's own request/response cycle ever calls.
//
// The property under test that matters most: the self-skip inside
// mapSessionToAncestorHolds ("the hold names sess itself, not an ancestor")
// is correct only because v1alpha1.WalkAncestors visits the starting session
// first before climbing to its parent. That coupling is exactly the kind a
// refactor (of either function) could break silently without a test pinned
// directly to this one.
//
// key, newReconcilerWith, sessionWithBinding, and headlessChild are
// headless_test.go's fixtures, in this same white-box package.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestMapSessionToAncestorHolds_SkipsAHoldOnSessItself(t *testing.T) {
	root := sessionWithBinding(t, "demo-root", nil)
	hold := &v1.SessionHold{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "demo-hold"},
		Spec:       v1.SessionHoldSpec{SessionRef: v1.NamespacedRef{Namespace: "ns", Name: "demo-root"}},
		Status:     v1.SessionHoldStatus{Phase: v1.SessionHoldPhaseActive},
	}
	r, _ := newReconcilerWith(t, root, hold)

	reqs := r.mapSessionToAncestorHolds(context.Background(), root)

	assert.Empty(t, reqs,
		"a hold naming sess itself is not an ancestor hold -- cascadeHold only ever fans out to DESCENDANTS, so this watch has nothing to catch here; the ordinary SessionHold watch already covers a direct hold on sess")
}

func TestMapSessionToAncestorHolds_FindsAnActiveHoldOnAnAncestor(t *testing.T) {
	root := sessionWithBinding(t, "demo-root", nil)
	mid := headlessChild(t, "demo-mid", "demo-root")
	leaf := headlessChild(t, "demo-leaf", "demo-mid")
	hold := &v1.SessionHold{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "demo-hold"},
		Spec:       v1.SessionHoldSpec{SessionRef: v1.NamespacedRef{Namespace: "ns", Name: "demo-root"}},
		Status:     v1.SessionHoldStatus{Phase: v1.SessionHoldPhaseActive},
	}
	r, _ := newReconcilerWith(t, root, mid, leaf, hold)

	reqs := r.mapSessionToAncestorHolds(context.Background(), leaf)

	require.Len(t, reqs, 1, "an active hold two hops up leaf's own ancestor chain must be re-enqueued")
	assert.Equal(t, "demo-hold", reqs[0].Name)
	assert.Equal(t, "ns", reqs[0].Namespace)
}

func TestMapSessionToAncestorHolds_SkipsAReleasedHold(t *testing.T) {
	root := sessionWithBinding(t, "demo-root", nil)
	leaf := headlessChild(t, "demo-leaf", "demo-root")
	hold := &v1.SessionHold{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "demo-hold"},
		Spec:       v1.SessionHoldSpec{SessionRef: v1.NamespacedRef{Namespace: "ns", Name: "demo-root"}},
		Status:     v1.SessionHoldStatus{Phase: v1.SessionHoldPhaseReleased},
	}
	r, _ := newReconcilerWith(t, root, leaf, hold)

	reqs := r.mapSessionToAncestorHolds(context.Background(), leaf)

	assert.Empty(t, reqs, "a released hold needs no cascade -- nothing left for this watch to catch")
}

func TestMapSessionToAncestorHolds_NoActiveHoldAnywhere_ReturnsEmpty(t *testing.T) {
	root := sessionWithBinding(t, "demo-root", nil)
	leaf := headlessChild(t, "demo-leaf", "demo-root")
	r, _ := newReconcilerWith(t, root, leaf)

	reqs := r.mapSessionToAncestorHolds(context.Background(), leaf)

	assert.Empty(t, reqs)
}

func TestMapSessionToAncestorHolds_NonAgentSessionObject_ReturnsNil(t *testing.T) {
	r, _ := newReconcilerWith(t)

	reqs := r.mapSessionToAncestorHolds(context.Background(), &v1.SessionHold{})

	assert.Nil(t, reqs, "the type assertion must fail closed to nil, never panic, for an object that is not an AgentSession")
}

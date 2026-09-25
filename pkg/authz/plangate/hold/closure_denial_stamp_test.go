package hold_test

// §2.8: a denial must be "consulted by EVERY member of the delegation
// closure". The reason is concrete — a denial the parent cannot get past is
// exactly what a parent routes around, by delegating to a child that can make
// the call instead. A per-session record makes that work.

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate/hold"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// treeSession builds a member of a delegation tree. An empty root means this
// session IS the root — a root is never labelled with its own name.
func treeSession(name, root string) *spiceboxv1alpha1.AgentSession {
	s := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "demo"},
	}
	if root != "" {
		s.Labels = map[string]string{spiceboxv1alpha1.LabelDelegationRoot: root}
		s.Spec.Parent = &spiceboxv1alpha1.NamespacedRef{Namespace: "demo", Name: root}
	}
	return s
}

func stampFixture(t *testing.T, denied bool, objs ...client.Object) (*hold.ClosureDenialStamper, client.Client) {
	t.Helper()
	sch := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(sch))
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(objs...).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).Build()
	return hold.NewClosureDenialStamper(hold.ClosureDenialStamperDeps{
		Client: c,
		Denied: func(context.Context, memory.Scope) (bool, error) { return denied, nil },
	}), c
}

func closureDeniedOf(t *testing.T, c client.Client, name string) bool {
	t.Helper()
	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "demo", Name: name}, &got))
	return got.Status.ClosureDenied != nil && *got.Status.ClosureDenied
}

// TestADenialMarksEVERYMemberOfTheTree, including the root and the siblings
// that did nothing wrong.
//
// That breadth is the point rather than collateral: the sibling is precisely
// where a parent would route the refused work next.
func TestADenialMarksEVERYMemberOfTheTree(t *testing.T) {
	root := treeSession("root", "")
	childA := treeSession("child-a", "root")
	childB := treeSession("child-b", "root")
	s, c := stampFixture(t, true, root, childA, childB)

	require.NoError(t, s.OnSignal(context.Background(),
		memory.Signal{Scope: memory.Scope{Kind: "session", ID: "demo/child-a"}}))

	assert.True(t, closureDeniedOf(t, c, "child-a"), "the session that was denied")
	assert.True(t, closureDeniedOf(t, c, "root"), "its root, which could re-delegate the same work")
	assert.True(t, closureDeniedOf(t, c, "child-b"),
		"and the SIBLING — the branch a parent routing around the refusal would use next")
}

// TestACleanSignalStampsNothing: the stamper rides the same signal as the
// judgements beside it, and most signals are not denials.
func TestACleanSignalStampsNothing(t *testing.T) {
	root := treeSession("root", "")
	child := treeSession("child", "root")
	s, c := stampFixture(t, false, root, child)

	require.NoError(t, s.OnSignal(context.Background(),
		memory.Signal{Scope: memory.Scope{Kind: "session", ID: "demo/child"}}))

	assert.False(t, closureDeniedOf(t, c, "child"))
	assert.False(t, closureDeniedOf(t, c, "root"))
}

// TestTheFlagIsSETONLYAndNeverCleared.
//
// A later approval does not undo the fact that a refusal happened in this
// closure. Clearing on the next clean signal would let a parent launder a
// denial by making one approved call afterwards — which is the same
// route-around this whole mechanism exists to stop, one step later.
func TestTheFlagIsSETONLYAndNeverCleared(t *testing.T) {
	root := treeSession("root", "")
	already := true
	root.Status.ClosureDenied = &already
	s, c := stampFixture(t, false, root) // a CLEAN signal now

	require.NoError(t, s.OnSignal(context.Background(),
		memory.Signal{Scope: memory.Scope{Kind: "session", ID: "demo/root"}}))

	assert.True(t, closureDeniedOf(t, c, "root"),
		"an approval afterwards must not erase the denial that already happened")
}

// TestALoneSessionIsItsOwnClosure — the 99% case, and it must work without a
// tree: a root with no descendants still records its own denial.
func TestALoneSessionIsItsOwnClosure(t *testing.T) {
	root := treeSession("solo", "")
	s, c := stampFixture(t, true, root)

	require.NoError(t, s.OnSignal(context.Background(),
		memory.Signal{Scope: memory.Scope{Kind: "session", ID: "demo/solo"}}))

	assert.True(t, closureDeniedOf(t, c, "solo"))
}

// TestOneUnstampableMemberDoesNotStopTheRest.
//
// A closure where half the members learned of the denial is strictly better
// than one where the first error stopped the fan-out — the gate fails closed
// per session, so every member that DID get the flag is still protected.
func TestOneUnstampableMemberDoesNotStopTheRest(t *testing.T) {
	sch := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(sch))

	// The status patch fails for exactly one member. Ordering inside the
	// fan-out is not fixed, so the refusal is keyed on the object rather than
	// on which call happens to be first.
	c := fake.NewClientBuilder().WithScheme(sch).
		WithObjects(treeSession("root", ""), treeSession("child", "root"), treeSession("wedged", "root")).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, cl client.Client, sr string, obj client.Object,
				patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if obj.GetName() == "wedged" {
					return errors.New("conflict on this member")
				}
				return cl.SubResource(sr).Patch(ctx, obj, patch, opts...)
			},
		}).Build()

	s := hold.NewClosureDenialStamper(hold.ClosureDenialStamperDeps{
		Client: c,
		Denied: func(context.Context, memory.Scope) (bool, error) { return true, nil },
	})

	err := s.OnSignal(context.Background(),
		memory.Signal{Scope: memory.Scope{Kind: "session", ID: "demo/child"}})

	assert.Error(t, err, "the failure must still surface rather than being swallowed")
	assert.True(t, closureDeniedOf(t, c, "root"),
		"the members that could be stamped were — a half-marked closure still protects those members")
	assert.True(t, closureDeniedOf(t, c, "child"))
	assert.False(t, closureDeniedOf(t, c, "wedged"))
}

// TestAnUnreadableDecisionSetIsAnErrorNotACleanClosure: the gate refuses on
// this flag in every mode, so a transient failure must not read as "nobody was
// denied".
func TestAnUnreadableDecisionSetIsAnErrorNotACleanClosure(t *testing.T) {
	sch := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(sch))
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(treeSession("root", "")).Build()
	s := hold.NewClosureDenialStamper(hold.ClosureDenialStamperDeps{
		Client: c,
		Denied: func(context.Context, memory.Scope) (bool, error) {
			return false, errors.New("memory unavailable")
		},
	})

	err := s.OnSignal(context.Background(),
		memory.Signal{Scope: memory.Scope{Kind: "session", ID: "demo/root"}})
	require.Error(t, err)
}

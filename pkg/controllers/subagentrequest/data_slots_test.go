package subagentrequest

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// requestWithDataSlots builds an on-roster request asking for data slots.
func requestWithDataSlots(t *testing.T, name string, slots ...v1.DataSlotRequest) *v1.SubagentRequest {
	t.Helper()
	sr := request(t, name, "demo-parent", "demo-coder")
	sr.Spec.DataSlots = slots
	return sr
}

func reconcilerWithAuthz(t *testing.T, az *fakeAuthz, sr *v1.SubagentRequest) (*Reconciler, *fakeAuthz) {
	t.Helper()
	sch := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(sch).
		WithObjects(
			parentClass(t, "demo-lead", "demo-coder"),
			childClass(t, "demo-coder"),
			parentSession(t, "demo-parent", "demo-lead"),
			sr,
		).
		WithStatusSubresource(&v1.SubagentRequest{}, &v1.AgentSession{}).Build()
	return &Reconciler{Client: c, Scheme: sch, Authz: az, MaxDelegationDepth: 3}, az
}

// TestDataSlots_OnlyTagsTheParentCanReadAreBound is the attenuation rule at
// the handoff.
//
// The parent asks for two tags and holds standing on one. It gets one — its
// judgment SELECTS within an envelope it cannot widen.
func TestDataSlots_OnlyTagsTheParentCanReadAreBound(t *testing.T) {
	az := &fakeAuthz{tagAccess: map[string]bool{"ptt-mine": true}}
	r, _ := reconcilerWithAuthz(t, az, requestWithDataSlots(t, "req-ds",
		v1.DataSlotRequest{Slot: "diff", TagID: "ptt-mine"},
		v1.DataSlotRequest{Slot: "logs", TagID: "ptt-theirs"},
	))

	reconcileOnce(t, r, "req-ds")

	require.Len(t, az.grantedDataSlots, 1, "only the delegable tag may be bound")
	assert.Equal(t, "ptt-mine", az.grantedDataSlots[0].TagID)
	assert.NotZero(t, az.dataSlotExpiry,
		"granted_to is declared with expiration, and the expiry is the backstop for a teardown that never runs")
}

// TestDataSlots_RefusedSlotsAreReportedNotDropped is why status carries them.
//
// A child waiting on data that will never arrive, with nothing anywhere saying
// why, is a far worse failure than a refusal the parent can read back.
func TestDataSlots_RefusedSlotsAreReportedNotDropped(t *testing.T) {
	az := &fakeAuthz{tagAccess: map[string]bool{"ptt-mine": true}}
	r, _ := reconcilerWithAuthz(t, az, requestWithDataSlots(t, "req-refused",
		v1.DataSlotRequest{Slot: "diff", TagID: "ptt-mine"},
		v1.DataSlotRequest{Slot: "logs", TagID: "ptt-theirs"},
	))

	reconcileOnce(t, r, "req-refused")

	var got v1.SubagentRequest
	require.NoError(t, r.Get(context.Background(),
		types.NamespacedName{Namespace: "ns", Name: "req-refused"}, &got))

	require.Len(t, got.Status.BoundDataSlots, 1)
	assert.Equal(t, "diff", got.Status.BoundDataSlots[0].Slot)
	require.Len(t, got.Status.RefusedDataSlots, 1,
		"a slot the parent could not delegate must be visible, not silently absent")
	assert.Equal(t, "logs", got.Status.RefusedDataSlots[0].Slot)
	assert.Equal(t, v1.SubagentRequestPhaseRunning, got.Status.Phase,
		"a refused slot narrows the handoff; it does not fail the delegation")
}

// TestDataSlots_NoneRequestedBindsNothing keeps the common path free of a
// SpiceDB round trip. Every delegation today names no data slots.
func TestDataSlots_NoneRequestedBindsNothing(t *testing.T) {
	az := &fakeAuthz{}
	r, _ := reconcilerWithAuthz(t, az, request(t, "req-nods", "demo-parent", "demo-coder"))

	reconcileOnce(t, r, "req-nods")

	assert.Empty(t, az.grantedDataSlots)
	var got v1.SubagentRequest
	require.NoError(t, r.Get(context.Background(),
		types.NamespacedName{Namespace: "ns", Name: "req-nods"}, &got))
	assert.Empty(t, got.Status.BoundDataSlots)
	assert.Empty(t, got.Status.RefusedDataSlots)
}

// TestDataSlots_AnUnanswerableCheckDoesNotLeaveAChild pins the failure
// direction, and it is the one that matters.
//
// An attenuation check that could not be answered is UNKNOWN, not a yes. The
// delegation is retried rather than proceeding with an envelope nobody
// established — and the half-built child is rolled back, exactly as a failed
// lineage write rolls one back.
func TestDataSlots_AnUnanswerableCheckDoesNotLeaveAChild(t *testing.T) {
	az := &fakeAuthz{tagAccessErr: assert.AnError}
	r, _ := reconcilerWithAuthz(t, az, requestWithDataSlots(t, "req-dserr",
		v1.DataSlotRequest{Slot: "diff", TagID: "ptt-mine"},
	))

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "ns", Name: "req-dserr"},
	})
	require.Error(t, err, "an unanswerable attenuation check must be retried, not swallowed")

	var sessions v1.AgentSessionList
	require.NoError(t, r.List(context.Background(), &sessions))
	assert.Len(t, sessions.Items, 1,
		"only the parent may remain: a child must not run with an envelope nobody could establish")
}

// TestDataSlots_AGrantFailureDoesNotLeaveAChild is the same property for the
// write rather than the check. A child that exists while its data slots do not
// would silently run without the information it was delegated to work on.
func TestDataSlots_AGrantFailureDoesNotLeaveAChild(t *testing.T) {
	az := &fakeAuthz{
		tagAccess:         map[string]bool{"ptt-mine": true},
		grantDataSlotsErr: assert.AnError,
	}
	r, _ := reconcilerWithAuthz(t, az, requestWithDataSlots(t, "req-dsgrant",
		v1.DataSlotRequest{Slot: "diff", TagID: "ptt-mine"},
	))

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "ns", Name: "req-dsgrant"},
	})
	require.Error(t, err)

	var sessions v1.AgentSessionList
	require.NoError(t, r.List(context.Background(), &sessions))
	assert.Len(t, sessions.Items, 1,
		"a child whose data slots failed to bind must not run: it would work without the data it was given")
}

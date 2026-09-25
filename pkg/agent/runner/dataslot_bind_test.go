package runner_test

// The parent's mid-flight hand-over: appending a slot to a live delegation.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/pkg/agent/runner"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func bindFixture(t *testing.T, phase string, slots ...spiceboxv1alpha1.DataSlotRequest) (client.Client, func(context.Context, string, string, string) error) {
	t.Helper()
	sch := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(sch))
	sr := &spiceboxv1alpha1.SubagentRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "req-1", Namespace: "demo"},
		Spec: spiceboxv1alpha1.SubagentRequestSpec{
			Parent:    spiceboxv1alpha1.NamespacedRef{Namespace: "demo", Name: "parent"},
			Class:     "demo-writer",
			Task:      "summarize",
			DataSlots: slots,
		},
		Status: spiceboxv1alpha1.SubagentRequestStatus{Phase: phase},
	}
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(sr).
		WithStatusSubresource(&spiceboxv1alpha1.SubagentRequest{}).Build()
	return c, runner.BindDataSlotFor(c, "demo", "parent")
}

func specSlots(t *testing.T, c client.Client) []spiceboxv1alpha1.DataSlotRequest {
	t.Helper()
	var got spiceboxv1alpha1.SubagentRequest
	require.NoError(t, c.Get(context.Background(),
		client.ObjectKey{Namespace: "demo", Name: "req-1"}, &got))
	return got.Spec.DataSlots
}

// TestAnOfferLandsOnTheSpecNotAsAGrant.
//
// send_input records what the parent OFFERS; whether it reaches the child is
// the controller's call, re-checked against live state. Writing a grant here
// would decide on facts that were true when the parent spoke.
func TestAnOfferLandsOnTheSpecNotAsAGrant(t *testing.T) {
	c, bind := bindFixture(t, spiceboxv1alpha1.SubagentRequestPhaseRunning)

	require.NoError(t, bind(context.Background(), "req-1", "diff", "ptt-a"))

	assert.Equal(t, []spiceboxv1alpha1.DataSlotRequest{{Slot: "diff", TagID: "ptt-a"}}, specSlots(t, c))
}

// TestRepeatingAnOfferIsANoOp.
//
// The spec is a set of offers and a repeated one is the same offer. Appending
// twice would have the controller grade it twice and — the part that reaches a
// person — could publish a second disclosure card for a question already in
// front of them.
func TestRepeatingAnOfferIsANoOp(t *testing.T) {
	c, bind := bindFixture(t, spiceboxv1alpha1.SubagentRequestPhaseRunning,
		spiceboxv1alpha1.DataSlotRequest{Slot: "diff", TagID: "ptt-a"})

	require.NoError(t, bind(context.Background(), "req-1", "diff", "ptt-a"))

	assert.Len(t, specSlots(t, c), 1)
}

// TestTheSAMESlotWithADifferentTagIsANewOffer — a re-point is a different
// datum, and the controller must get the chance to judge it afresh.
func TestTheSAMESlotWithADifferentTagIsANewOffer(t *testing.T) {
	c, bind := bindFixture(t, spiceboxv1alpha1.SubagentRequestPhaseRunning,
		spiceboxv1alpha1.DataSlotRequest{Slot: "diff", TagID: "ptt-a"})

	require.NoError(t, bind(context.Background(), "req-1", "diff", "ptt-b"))

	assert.Len(t, specSlots(t, c), 2)
}

// TestSendingToAFinishedDelegationIsRefused.
//
// There is nobody left to receive it. A silent write would leave the parent
// believing it answered a request nobody is waiting on — and the child it
// meant to answer has already stopped.
func TestSendingToAFinishedDelegationIsRefused(t *testing.T) {
	c, bind := bindFixture(t, spiceboxv1alpha1.SubagentRequestPhaseSucceeded)

	err := bind(context.Background(), "req-1", "diff", "ptt-a")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "already finished")
	assert.Empty(t, specSlots(t, c), "and nothing is written")
}

// The delegation handle is model-authored and names a request in the
// NAMESPACE, which is not the same as naming one of THIS session's
// delegations. reply_to_subagent makes this exact check and documents it as
// load-bearing; the send_input path reached the Get and the Update without it,
// so a parent could append a data offer to a sibling session's live
// delegation. Two unrelated controls happen to stop the write from mattering
// today, and neither of them is this check.
func TestBindRefusesADelegationThisSessionDidNotCreate(t *testing.T) {
	c, _ := bindFixture(t, spiceboxv1alpha1.SubagentRequestPhaseRunning)
	// Same namespace, same live request — a DIFFERENT parent.
	stranger := runner.BindDataSlotFor(c, "demo", "some-other-session")

	err := stranger(context.Background(), "req-1", "diff", "ptt-a")

	require.Error(t, err, "a session may only fill slots on delegations it created")
	assert.Contains(t, err.Error(), "not yours")
	assert.Empty(t, specSlots(t, c), "and nothing may be written on the refused path")
}

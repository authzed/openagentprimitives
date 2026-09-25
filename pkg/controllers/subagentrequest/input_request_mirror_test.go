package subagentrequest

// Mirroring the child's mid-flight ask for DATA onto the request its parent
// polls. The parent's runner has no permission on the child's AgentSession, so
// without this the ask is written where nobody who could act on it can read.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// inputRequestFixture builds a live delegation whose child carries whatever
// InputRequest the case is about.
func inputRequestFixture(t *testing.T, ir *v1.InputRequest, mode string) (*Reconciler, client.Client) {
	t.Helper()
	sr := requestWithChild(t, "req1", "demo-parent", "demo-coder", "req1-child")
	sr.Spec.Mode = mode
	child := childSession(t, "req1-child", "demo-coder", v1.AgentSessionStatus{
		Phase:        v1.AgentSessionPhaseRunning,
		InputRequest: ir,
	})
	ch := &v1.Channel{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "req1-inbox"}}
	r, c := newReconciler(t,
		conversationalParentClass(t),
		childClass(t, "demo-coder"),
		parentSession(t, "demo-parent", "demo-lead"),
		sr, child, ch,
	)
	r.Now = (&fixedClock{t: time.Now()}).now
	return r, c
}

// TestTheAskReachesTheParentsRequest.
func TestTheAskReachesTheParentsRequest(t *testing.T) {
	r, c := inputRequestFixture(t, &v1.InputRequest{
		Slot: "diff", Why: "the task refers to a diff I cannot see", Exchange: 1, Pending: true,
	}, v1.SubagentModeChat)

	reconcileOnce(t, r, "req1")

	got := getRequest(t, c, "req1")
	require.NotNil(t, got.Status.InputRequest)
	assert.Equal(t, "diff", got.Status.InputRequest.Slot)
	assert.Equal(t, "the task refers to a diff I cannot see", got.Status.InputRequest.Why)
}

// TestAskingForDataDoesNotParkTheDelegation.
//
// request_input's contract says plainly that it does not pause the child, so
// the request must not move the delegation out of Running. A delegation that
// stopped because its child asked a question it was told would not block is
// the hang that contract rules out.
func TestAskingForDataDoesNotParkTheDelegation(t *testing.T) {
	r, c := inputRequestFixture(t, &v1.InputRequest{
		Slot: "diff", Why: "need it", Exchange: 1, Pending: true,
	}, v1.SubagentModeChat)

	reconcileOnce(t, r, "req1")

	got := getRequest(t, c, "req1")
	assert.NotEqual(t, v1.SubagentRequestPhaseAwaitingParent, got.Status.Phase,
		"asking for data is not asking a question")
	assert.NotEqual(t, v1.SubagentRequestPhaseAwaitingDisclosure, got.Status.Phase,
		"and nothing is pending a decision until the parent actually offers a datum")
}

// TestASingleTurnChildMayStillAskForData.
//
// The mode restricts CONVERSATION — a headless child must not promote its
// delegation into one the roster never granted — and a data request grants
// nothing: the parent may only bind a tag it can itself read, and a disclosing
// one still routes to a human. Ignoring it here would withhold a capability
// the mode never restricted.
func TestASingleTurnChildMayStillAskForData(t *testing.T) {
	r, c := inputRequestFixture(t, &v1.InputRequest{
		Slot: "diff", Why: "need it", Exchange: 1, Pending: true,
	}, v1.SubagentModeSingleTurn)

	reconcileOnce(t, r, "req1")

	got := getRequest(t, c, "req1")
	require.NotNil(t, got.Status.InputRequest,
		"a single_turn child's ask for data is still mirrored")
	assert.Equal(t, "diff", got.Status.InputRequest.Slot)
}

// TestAnAnsweredAskStopsShowingAsOutstanding, so a parent polling the request
// does not keep seeing a question it already dealt with.
func TestAnAnsweredAskStopsShowingAsOutstanding(t *testing.T) {
	r, c := inputRequestFixture(t, &v1.InputRequest{
		Slot: "diff", Why: "need it", Exchange: 1, Pending: false,
	}, v1.SubagentModeChat)
	// Seed the request as though a previous pass had mirrored it.
	sr := getRequest(t, c, "req1")
	sr.Status.InputRequest = &v1.InputRequest{Slot: "diff", Exchange: 1, Pending: true}
	require.NoError(t, c.Status().Update(t.Context(), &sr))

	reconcileOnce(t, r, "req1")

	assert.Nil(t, getRequest(t, c, "req1").Status.InputRequest)
}

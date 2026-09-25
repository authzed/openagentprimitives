package subagentrequest

// The resumable half of a conversational delegation: mirroring a child's
// outstanding question onto the request its parent polls, flipping back when
// the answer lands, and ending a delegation whose parent never answered.
//
// Its own file for the same reason controller_conversational_test.go is one:
// controller_test.go is the single_turn surface Track 1a was built on, and the
// regression guard for it stays separate from the lifecycle that must not
// disturb it.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// fixedClock is a settable time source for the parent-reply bound, so the
// timeout is tested by moving time rather than by sleeping through it.
type fixedClock struct{ t time.Time }

func (c *fixedClock) now() time.Time          { return c.t }
func (c *fixedClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// awaitingFixture seeds a conversational delegation already past the create
// step — Running, with a child — plus the child itself, whose status carries
// whatever ParentExchange the case is about.
func awaitingFixture(t *testing.T, pe *v1.ParentExchange, childPhase string) (*Reconciler, client.Client, *fixedClock) {
	t.Helper()
	sr := requestWithChild(t, "req1", "demo-parent", "demo-coder", "req1-child")
	sr.Spec.Mode = v1.SubagentModeChat
	child := childSession(t, "req1-child", "demo-coder", v1.AgentSessionStatus{
		Phase:          childPhase,
		ParentExchange: pe,
	})
	// The Channel a conversational delegation gets. Present so the teardown
	// path has something real to delete, and so a test asserting it survives
	// is asserting about an object that exists.
	ch := &v1.Channel{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "req1-inbox"}}
	r, c := newReconciler(t,
		conversationalParentClass(t),
		childClass(t, "demo-coder"),
		parentSession(t, "demo-parent", "demo-lead"),
		sr, child, ch,
	)
	clk := &fixedClock{t: time.Now()}
	r.Now = clk.now
	return r, c, clk
}

func getRequest(t *testing.T, c client.Client, name string) v1.SubagentRequest {
	t.Helper()
	var got v1.SubagentRequest
	require.NoError(t, c.Get(t.Context(), types.NamespacedName{Namespace: "ns", Name: name}, &got))
	return got
}

// Brief test 1 (controller half): an outstanding question moves the request
// into the awaiting state, carrying the child's words and the exchange number.
func TestReconcileChild_PendingQuestion_MovesTheRequestToAwaitingParent(t *testing.T) {
	r, c, _ := awaitingFixture(t, &v1.ParentExchange{
		Exchange: 1, Pending: true, Question: "which of the two repos?",
	}, v1.AgentSessionPhaseRunning)

	res := reconcileOnce(t, r, "req1")

	got := getRequest(t, c, "req1")
	assert.Equal(t, v1.SubagentRequestPhaseAwaitingParent, got.Status.Phase)
	assert.Equal(t, "which of the two repos?", got.Status.Message, "the child's own words, copied verbatim")
	assert.Equal(t, int64(1), got.Status.Exchange)
	require.NotNil(t, got.Status.AwaitingParentSince, "the parent's clock must start when the question is recorded")
	assert.Greater(t, res.RequeueAfter, time.Duration(0),
		"nothing else re-enqueues a request whose child is silently parked, so the bound needs its own requeue")
}

// A child parked while REAPED (phase Idle, its pod gone) is still waiting.
// Deriving the awaiting state from the child's phase instead of from
// parentExchange.Pending is exactly how that case would be lost.
func TestReconcileChild_PendingQuestionSurvivesTheChildGoingIdle(t *testing.T) {
	r, c, _ := awaitingFixture(t, &v1.ParentExchange{
		Exchange: 1, Pending: true, Question: "still waiting",
	}, v1.AgentSessionPhaseIdle)

	reconcileOnce(t, r, "req1")

	got := getRequest(t, c, "req1")
	assert.Equal(t, v1.SubagentRequestPhaseAwaitingParent, got.Status.Phase,
		"a reaped child is parked, not finished; the delegation is still resumable")
	assert.Equal(t, "still waiting", got.Status.Message)
}

// The answer landed and the child resumed: the request goes back to Running,
// the clock stops, and the exchange number is KEPT — a parent's next reply
// waits for it to advance, so forgetting it would strand the conversation.
func TestReconcileChild_AnsweredQuestion_ReturnsToRunningAndKeepsTheExchange(t *testing.T) {
	r, c, _ := awaitingFixture(t, &v1.ParentExchange{
		Exchange: 2, Pending: false, Question: "answered already",
	}, v1.AgentSessionPhaseRunning)
	// Seed the request as though a prior pass had recorded exchange 2.
	sr := getRequest(t, c, "req1")
	now := metav1.NewTime(time.Now())
	sr.Status.Phase = v1.SubagentRequestPhaseAwaitingParent
	sr.Status.Exchange = 2
	sr.Status.Message = "answered already"
	sr.Status.AwaitingParentSince = &now
	require.NoError(t, c.Status().Update(t.Context(), &sr))

	reconcileOnce(t, r, "req1")

	got := getRequest(t, c, "req1")
	assert.Equal(t, v1.SubagentRequestPhaseRunning, got.Status.Phase)
	assert.Nil(t, got.Status.AwaitingParentSince, "a working child is not measured against the parent-reply bound")
	assert.Equal(t, int64(2), got.Status.Exchange, "the parent's next reply waits for this to advance")
}

// A SECOND question gets its own clock. Comparing the exchange NUMBER rather
// than the message text is what makes that true even when a child asks the
// same thing twice.
func TestReconcileChild_ASecondQuestion_RestartsTheParentClock(t *testing.T) {
	r, c, clk := awaitingFixture(t, &v1.ParentExchange{
		Exchange: 2, Pending: true, Question: "same words",
	}, v1.AgentSessionPhaseRunning)
	firstSeen := metav1.NewTime(clk.now().Add(-25 * time.Minute))
	sr := getRequest(t, c, "req1")
	sr.Status.Phase = v1.SubagentRequestPhaseAwaitingParent
	sr.Status.Exchange = 1
	sr.Status.Message = "same words"
	sr.Status.AwaitingParentSince = &firstSeen
	require.NoError(t, c.Status().Update(t.Context(), &sr))

	reconcileOnce(t, r, "req1")

	got := getRequest(t, c, "req1")
	assert.Equal(t, int64(2), got.Status.Exchange)
	require.NotNil(t, got.Status.AwaitingParentSince)
	assert.True(t, got.Status.AwaitingParentSince.Time.After(firstSeen.Time),
		"exchange 2 is a new wait, not the remainder of exchange 1's")
}

// Brief test 5: a parent that never answers does not park the child forever.
// The bound fires, the delegation is resolved Failed (retryable, not a policy
// denial), and the child and its Channel are actually removed — a request
// marked Failed while the child sat on forever would leave the ceiling and the
// objects exactly as stuck as before.
func TestReconcileChild_ParentNeverAnswers_EndsTheDelegationAndStopsTheChild(t *testing.T) {
	r, c, clk := awaitingFixture(t, &v1.ParentExchange{
		Exchange: 1, Pending: true, Question: "anybody there?",
	}, v1.AgentSessionPhaseIdle)

	// Pass one records the question and asks to be re-run when the bound is up.
	first := reconcileOnce(t, r, "req1")
	require.Equal(t, v1.SubagentRequestPhaseAwaitingParent, getRequest(t, c, "req1").Status.Phase)
	require.Equal(t, DefaultMaxAwaitingParent, first.RequeueAfter)

	// Still inside the bound: nothing is torn down, and it asks again for the
	// remainder rather than for the whole bound a second time.
	clk.advance(DefaultMaxAwaitingParent / 2)
	mid := reconcileOnce(t, r, "req1")
	assert.Equal(t, v1.SubagentRequestPhaseAwaitingParent, getRequest(t, c, "req1").Status.Phase,
		"half the bound is not the bound")
	assert.InDelta(t, float64(DefaultMaxAwaitingParent/2), float64(mid.RequeueAfter), float64(time.Second))
	assertExists(t, c, "req1-child", true)

	// Past it.
	clk.advance(DefaultMaxAwaitingParent/2 + time.Second)
	reconcileOnce(t, r, "req1")

	got := getRequest(t, c, "req1")
	assert.Equal(t, v1.SubagentRequestPhaseFailed, got.Status.Phase,
		"an unanswered delegation is a retryable failure, never a policy denial")
	assert.Equal(t, "ParentUnanswered", got.Status.FailureReason)
	assert.Contains(t, got.Status.Determination, "demo-parent")
	assertExists(t, c, "req1-child", false)
	assertChannelExists(t, c, "req1-inbox", false)
}

// Brief test 7 (controller half, Track 1a regression guard): a single_turn
// delegation never reaches the awaiting state. Its child is headless and asks
// nothing, so the non-terminal arm leaves the request exactly where Track 1a
// left it — Running, waiting on a terminal child phase.
func TestReconcileChild_SingleTurn_NeverEntersAwaitingParent(t *testing.T) {
	sr := requestWithChild(t, "req1", "demo-parent", "demo-coder", "req1-child") // no spec.mode
	child := childSession(t, "req1-child", "demo-coder", v1.AgentSessionStatus{
		Phase: v1.AgentSessionPhaseRunning,
		// A headless child has no channel and is offered no ask_parent, so
		// this is nil for its whole life.
		ParentExchange: nil,
	})
	r, c := newReconciler(t,
		parentClass(t, "demo-lead", "demo-coder"),
		childClass(t, "demo-coder"),
		parentSession(t, "demo-parent", "demo-lead"),
		sr, child,
	)

	res := reconcileOnce(t, r, "req1")

	got := getRequest(t, c, "req1")
	assert.Equal(t, v1.SubagentRequestPhaseRunning, got.Status.Phase)
	assert.Empty(t, got.Status.Message)
	assert.Zero(t, got.Status.Exchange)
	assert.Nil(t, got.Status.AwaitingParentSince, "nothing measures a single_turn child against a parent-reply bound")
	assert.Equal(t, reconcile.Result{}, res, "no requeue: the AgentSession watch is what re-triggers this")
}

// Fail-closed: the CHILD writes status.parentExchange (its runner Role grants
// patch on its own agentsessions/status), so a headless child could set it and
// promote its own delegation to a conversation its parent's roster never
// granted. spec.mode is an attack-surface declaration, and the party it
// constrains must not be able to widen it.
func TestReconcileChild_SingleTurn_IgnoresAChildThatWritesAParentExchangeAnyway(t *testing.T) {
	sr := requestWithChild(t, "req1", "demo-parent", "demo-coder", "req1-child") // no spec.mode
	child := childSession(t, "req1-child", "demo-coder", v1.AgentSessionStatus{
		Phase: v1.AgentSessionPhaseRunning,
		ParentExchange: &v1.ParentExchange{
			Exchange: 1, Pending: true, Question: "let me talk to you",
		},
	})
	r, c := newReconciler(t,
		parentClass(t, "demo-lead", "demo-coder"),
		childClass(t, "demo-coder"),
		parentSession(t, "demo-parent", "demo-lead"),
		sr, child,
	)

	reconcileOnce(t, r, "req1")

	got := getRequest(t, c, "req1")
	assert.Equal(t, v1.SubagentRequestPhaseRunning, got.Status.Phase,
		"a single_turn delegation never becomes a conversation, whatever its child's status claims")
	assert.Empty(t, got.Status.Message, "the child's words must not reach the parent through a mode it was not granted")
	assert.Zero(t, got.Status.Exchange)
}

// A missing clock must never read as "no bound": that is how a status written
// by anything but the branch that stamps it would park a child forever.
func TestReconcileChild_AwaitingWithNoClock_RestampsRatherThanWaitingForever(t *testing.T) {
	r, c, _ := awaitingFixture(t, &v1.ParentExchange{
		Exchange: 1, Pending: true, Question: "q",
	}, v1.AgentSessionPhaseRunning)
	sr := getRequest(t, c, "req1")
	sr.Status.Phase = v1.SubagentRequestPhaseAwaitingParent
	sr.Status.Exchange = 1
	sr.Status.AwaitingParentSince = nil // the shape under test
	require.NoError(t, c.Status().Update(t.Context(), &sr))

	res := reconcileOnce(t, r, "req1")

	got := getRequest(t, c, "req1")
	assert.NotNil(t, got.Status.AwaitingParentSince)
	assert.Greater(t, res.RequeueAfter, time.Duration(0))
	assertExists(t, c, "req1-child", true, "restamping is not a reason to end the delegation")
}

func TestMaxAwaitingParent_ZeroIsTheDefaultNotUnlimited(t *testing.T) {
	assert.Equal(t, DefaultMaxAwaitingParent, (&Reconciler{}).maxAwaitingParent(),
		"an un-wired reconciler must still bound a parked child")
	assert.Equal(t, time.Minute, (&Reconciler{MaxAwaitingParent: time.Minute}).maxAwaitingParent(),
		"an explicit bound wins")
}

func assertExists(t *testing.T, c client.Client, name string, want bool, msgAndArgs ...any) {
	t.Helper()
	var sess v1.AgentSession
	err := c.Get(t.Context(), types.NamespacedName{Namespace: "ns", Name: name}, &sess)
	if want {
		assert.NoError(t, err, msgAndArgs...)
		return
	}
	assert.True(t, apierrors.IsNotFound(err), "expected session %s to be gone, got %v", name, err)
}

func assertChannelExists(t *testing.T, c client.Client, name string, want bool) {
	t.Helper()
	var ch v1.Channel
	err := c.Get(t.Context(), types.NamespacedName{Namespace: "ns", Name: name}, &ch)
	if want {
		assert.NoError(t, err)
		return
	}
	assert.True(t, apierrors.IsNotFound(err),
		"a delegation that ended must not leave its Channel behind, got %v", err)
}

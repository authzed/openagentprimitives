package toolcall_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/toolcall"
)

// The live failure this pins: a ToolCall's reconcile errored on every attempt,
// controller-runtime requeued it forever, and the ONLY trace was an operator
// log line. The session stayed Running, the pods stayed healthy, and the person
// waiting on the tool call was owed a reason nobody could give them.
//
// The fixture makes every reconcile fail the same way (a non-NotFound read of
// the bundle session), which is what "repeats" means here — the specific stage
// does not matter, only that it never stops failing.

const retryTestNS = "default"

// failingSessionReads builds a client whose SpiceboxSession reads fail with a
// server error for as long as *fail is true. ToolCall reads are untouched, so
// the reconciler still loads its own object and can write status.
func failingSessionReads(t *testing.T, fail *bool, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(snapshotTestScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&spiceboxv1alpha1.ToolCall{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, isSession := obj.(*spiceboxv1alpha1.SpiceboxSession); isSession && *fail {
					return apierrors.NewInternalError(errors.New("etcdserver: request timed out"))
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).
		Build()
}

// retryToolCall is a ToolCall already past the finalizer step, so a reconcile
// runs straight through to the stage that fails.
func retryToolCall(t *testing.T) *spiceboxv1alpha1.ToolCall {
	t.Helper()
	return &spiceboxv1alpha1.ToolCall{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "tc-retry",
			Namespace:  retryTestNS,
			Finalizers: []string{spiceboxv1alpha1.FinalizerToolCall},
			// Admission refuses a ToolCall with no AgentSession owner, and the
			// reconciler refuses one whose bundle belongs to a different
			// session, so a fixture without an owner does not describe anything
			// that can reach the reconciler in production.
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: spiceboxv1alpha1.SchemeBuilder.GroupVersion.String(),
				Kind:       "AgentSession", Name: retryTestSession, UID: "uid-retry",
			}},
		},
		Spec: spiceboxv1alpha1.ToolCallSpec{Session: "bundle", Tool: "git"},
	}
}

// retryTestSession is the AgentSession these fixtures act for. The bundle
// SpiceboxSession must carry it under the agentsession label, exactly as the
// AgentSession reconciler stamps it in production — the toolcall reconciler
// resolves the parent session from that label and fails closed without it.
const retryTestSession = "sess-retry"

func loadToolCall(t *testing.T, c client.Client, name string) *spiceboxv1alpha1.ToolCall {
	t.Helper()
	var out spiceboxv1alpha1.ToolCall
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: retryTestNS, Name: name}, &out))
	return &out
}

// A single failure must still retry — a control-plane blip is not a dead tool
// call — but it must already be legible on the object, not only in a log.
func TestReconcile_firstFailureRetriesAndIsAlreadyVisibleOnTheToolCall(t *testing.T) {
	fail := true
	tc := retryToolCall(t)
	c := failingSessionReads(t, &fail, tc)
	r := &toolcall.Reconciler{Client: c, Now: func() time.Time { return time.Unix(1000, 0) }}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: retryTestNS, Name: tc.Name}})
	require.Error(t, err, "a transient failure must be returned so controller-runtime retries it")

	got := loadToolCall(t, c, tc.Name)
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.ToolCallConditionReconcileRetrying)
	require.NotNil(t, cond, "the failing reconcile must be recorded on the ToolCall, not only logged")
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Contains(t, cond.Message, "etcdserver", "the condition must carry the underlying error")
	assert.False(t,
		meta.IsStatusConditionTrue(got.Status.Conditions, spiceboxv1alpha1.ToolCallConditionFailed),
		"one failure is not a dead tool call")
}

// Once the failure has repeated past the budget the call is finished: a caller
// waiting on it gets a terminal ToolCall carrying the reason, which the runner
// turns into an IsError tool result the agent can relay.
func TestReconcile_failureRepeatingPastTheBudgetFailsTheToolCallInsteadOfRetryingForever(t *testing.T) {
	fail := true
	tc := retryToolCall(t)
	c := failingSessionReads(t, &fail, tc)
	clock := time.Unix(1000, 0)
	r := &toolcall.Reconciler{Client: c, Now: func() time.Time { return clock }}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: retryTestNS, Name: tc.Name}}

	_, err := r.Reconcile(context.Background(), req)
	require.Error(t, err, "the streak starts here")

	clock = clock.Add(toolcall.ReconcileFailureBudget + time.Second)
	_, err = r.Reconcile(context.Background(), req)
	require.NoError(t, err, "a call that can never progress is finished, not requeued again")

	got := loadToolCall(t, c, tc.Name)
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.ToolCallConditionFailed)
	require.NotNil(t, cond, "the ToolCall must end terminally so the waiting caller is answered")
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonToolCallReconcileFailed, cond.Reason)
	assert.Contains(t, cond.Message, "etcdserver",
		"the terminal message must name the cause; 'the operator log has it' is not an answer")
	assert.NotNil(t, got.Status.FinishedAt, "a finished call records when it finished")
}

// Recovery resets the streak. Without this a ToolCall that blipped once early
// on would be executed on a budget that had already half expired, and a much
// later, unrelated blip would kill it on its first attempt.
func TestReconcile_aSuccessfulReconcileResetsTheFailureStreak(t *testing.T) {
	fail := true
	tc := retryToolCall(t)
	// A session whose toolspec candidate is unreconciled makes a reconcile
	// SUCCEED with a requeue — the one non-terminal success a ToolCall this
	// early can have.
	sess := &spiceboxv1alpha1.SpiceboxSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "bundle", Namespace: retryTestNS,
			Labels: map[string]string{"agentprimitives.authzed.com/agentsession": retryTestSession},
		},
		Status: spiceboxv1alpha1.SpiceboxSessionStatus{
			Conditions: []metav1.Condition{{
				Type: spiceboxv1alpha1.SpiceboxSessionConditionReady, Status: metav1.ConditionTrue,
				Reason: "Ready", LastTransitionTime: metav1.Now(),
			}},
			ResolvedClass: &spiceboxv1alpha1.SpiceboxClassSpec{
				Tools: []spiceboxv1alpha1.SpiceboxTool{{Name: "git"}},
			},
			Sandbox:            &spiceboxv1alpha1.SandboxHandle{Kind: "pod", Ref: "default/pod-1"},
			EffectiveToolspecs: []string{"ts-git"},
		},
	}
	ts := &spiceboxv1alpha1.SpiceboxToolspec{
		ObjectMeta: metav1.ObjectMeta{Name: "ts-git"},
		Spec:       spiceboxv1alpha1.SpiceboxToolspecSpec{Toolkit: spiceboxv1alpha1.ToolspecToolkitRef{Name: "git"}},
	}
	c := failingSessionReads(t, &fail, tc, sess, ts)
	clock := time.Unix(1000, 0)
	r := &toolcall.Reconciler{Client: c, Now: func() time.Time { return clock }}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: retryTestNS, Name: tc.Name}}

	_, err := r.Reconcile(context.Background(), req)
	require.Error(t, err, "streak starts")

	fail = false
	clock = clock.Add(time.Second)
	_, err = r.Reconcile(context.Background(), req)
	require.NoError(t, err, "the blip cleared")
	assert.False(t,
		meta.IsStatusConditionTrue(loadToolCall(t, c, tc.Name).Status.Conditions,
			spiceboxv1alpha1.ToolCallConditionReconcileRetrying),
		"a recovered call is no longer retrying")

	fail = true
	clock = clock.Add(100 * toolcall.ReconcileFailureBudget)
	_, err = r.Reconcile(context.Background(), req)
	require.Error(t, err, "a fresh failure long after a recovery gets a fresh budget, not an instant death")
	assert.False(t,
		meta.IsStatusConditionTrue(loadToolCall(t, c, tc.Name).Status.Conditions,
			spiceboxv1alpha1.ToolCallConditionFailed),
		"the streak restarted at this failure, so the budget has not expired")
}

// An optimistic-concurrency conflict is the API server telling the reconciler
// to read and retry, not a failure of the work. Counting it would let a busy
// object burn its budget on writes that were never going to be lost.
func TestReconcile_aConflictIsRetriedWithoutStartingAFailureStreak(t *testing.T) {
	tc := retryToolCall(t)
	c := fake.NewClientBuilder().
		WithScheme(snapshotTestScheme(t)).
		WithObjects(tc).
		WithStatusSubresource(&spiceboxv1alpha1.ToolCall{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, isSession := obj.(*spiceboxv1alpha1.SpiceboxSession); isSession {
					return apierrors.NewConflict(
						schema.GroupResource{Group: spiceboxv1alpha1.SchemeGroupVersion.Group, Resource: "spiceboxsessions"},
						"bundle", errors.New("object was modified"))
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).
		Build()
	clock := time.Unix(1000, 0)
	r := &toolcall.Reconciler{Client: c, Now: func() time.Time { return clock }}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: retryTestNS, Name: tc.Name}}

	_, err := r.Reconcile(context.Background(), req)
	require.Error(t, err)
	clock = clock.Add(100 * toolcall.ReconcileFailureBudget)
	_, err = r.Reconcile(context.Background(), req)
	require.Error(t, err, "conflicts keep being retried")

	got := loadToolCall(t, c, tc.Name)
	assert.Nil(t, meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.ToolCallConditionReconcileRetrying),
		"a conflict is not a failing streak")
	assert.False(t, meta.IsStatusConditionTrue(got.Status.Conditions, spiceboxv1alpha1.ToolCallConditionFailed),
		"a conflict must never finish the ToolCall")
}

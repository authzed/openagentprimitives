package toolcall_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/toolcall"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// A tool_dispatch_snapshot append-only conflict is PERMANENT: the existing
// entry never changes and the record the reconcile recomputes never changes,
// so every retry produces the identical conflict. Riding out the full
// ReconcileFailureBudget on it is two minutes of a session that looks healthy
// and can never move — exactly the "silent until it finally reports" hang the
// budget was meant to shorten, not a transient to wait out. So an append-only
// conflict must finish the ToolCall on the FIRST reconcile, carrying the
// reason, rather than starting a streak.
//
// The failure is injected through the session read because that is the
// package's established fault-injection seam; the classification under test is
// source-independent — errors.Is(err, memory.ErrAppendOnlyConflict) — so any
// reconcile path that surfaces it (in production, WaitPreDispatchSnapshot's
// record call) finishes the call the same way.
func TestReconcile_aPermanentAppendOnlyConflictFinishesImmediatelyWithoutWaitingOutTheBudget(t *testing.T) {
	tc := retryToolCall(t)
	c := fake.NewClientBuilder().
		WithScheme(snapshotTestScheme(t)).
		WithObjects(tc).
		WithStatusSubresource(&spiceboxv1alpha1.ToolCall{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, isSession := obj.(*spiceboxv1alpha1.SpiceboxSession); isSession {
					return fmt.Errorf("WaitPreDispatchSnapshot: record audit: %w: default/sess tool_dispatch_snapshot tds-000013-000-",
						memory.ErrAppendOnlyConflict)
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).
		Build()
	clock := time.Unix(1000, 0)
	r := &toolcall.Reconciler{Client: c, Now: func() time.Time { return clock }}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: retryTestNS, Name: tc.Name}}

	// ONE reconcile — no clock advance past the budget.
	_, err := r.Reconcile(context.Background(), req)
	require.NoError(t, err, "a permanent conflict is finished, not requeued for controller-runtime to retry")

	got := loadToolCall(t, c, tc.Name)
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.ToolCallConditionFailed)
	require.NotNil(t, cond, "the ToolCall must end terminally on the first permanent conflict")
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonToolCallReconcileFailed, cond.Reason)
	assert.Contains(t, cond.Message, "append-only",
		"the terminal message must name the permanent cause")
	assert.NotNil(t, got.Status.FinishedAt, "a finished call records when it finished")
	assert.False(t,
		meta.IsStatusConditionTrue(got.Status.Conditions, spiceboxv1alpha1.ToolCallConditionReconcileRetrying),
		"a permanent conflict is not a transient streak")
}

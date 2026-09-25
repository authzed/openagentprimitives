package toolcall

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func gatherTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	sch := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(sch))
	return sch
}

// sessionForTool builds a session whose EffectiveToolspecs reference the only
// toolspec that could authorize `tool`. The toolspec itself is intentionally
// NOT created in the fake client — the interceptor decides what Get returns.
func sessionForTool(tool, specName string) *spiceboxv1alpha1.SpiceboxSession {
	return &spiceboxv1alpha1.SpiceboxSession{
		ObjectMeta: metav1.ObjectMeta{Name: "sess", Namespace: "default"},
		Status: spiceboxv1alpha1.SpiceboxSessionStatus{
			EffectiveToolspecs: []string{specName},
		},
	}
}

func toolCallFor(tool string) *spiceboxv1alpha1.ToolCall {
	return &spiceboxv1alpha1.ToolCall{
		ObjectMeta: metav1.ObjectMeta{Name: "tc", Namespace: "default"},
		Spec:       spiceboxv1alpha1.ToolCallSpec{Session: "sess", Tool: tool},
	}
}

// TestGatherCandidates_GetError_TransientVsNotFound is the regression test for
// the audit finding: a transient (non-NotFound) Get error on the only
// authorizing toolspec must surface as a returned error (→ reconciler requeue),
// NOT be silently dropped (which would leave the candidate set empty and the
// ToolCall permanently denied with a misleading "no spec authorizes" message).
// A NotFound, by contrast, is a legitimate "this candidate doesn't exist" and
// is skipped, leaving the empty-set classification to the caller.
func TestGatherCandidates_GetError_TransientVsNotFound(t *testing.T) {
	const specName = "echo-spec"
	const tool = "echo"

	cases := []struct {
		name string
		// getErr is returned by the intercepted Get for the candidate toolspec.
		getErr error
		// wantErr: gatherCandidates must propagate an error (transient → requeue).
		wantErr bool
		// wantPending/wantInvalid: when no error, how the candidate is classified.
		wantPending int
		wantInvalid int
	}{
		{
			name:    "transient ServiceUnavailable: error propagated, not silently dropped",
			getErr:  apierrors.NewServiceUnavailable("apiserver is having a moment"),
			wantErr: true,
		},
		{
			name:    "transient Timeout: error propagated so the reconciler requeues",
			getErr:  apierrors.NewTimeoutError("etcd timeout", 1),
			wantErr: true,
		},
		{
			name: "NotFound: candidate skipped, empty-set classified by caller (no error)",
			getErr: apierrors.NewNotFound(
				schema.GroupResource{Group: "agentprimitives.authzed.com", Resource: "spiceboxtoolspecs"},
				specName,
			),
			wantErr:     false,
			wantPending: 0,
			wantInvalid: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sch := gatherTestScheme(t)
			c := fake.NewClientBuilder().WithScheme(sch).
				WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ctx context.Context, _ client.WithWatch, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
						return tc.getErr
					},
				}).Build()

			r := &Reconciler{Client: c}
			cands, res, err := r.gatherCandidates(context.Background(), toolCallFor(tool), sessionForTool(tool, specName))

			if tc.wantErr {
				require.Error(t, err, "transient Get error must be propagated for requeue, not swallowed")
				assert.Empty(t, cands, "no candidates on transient error")
				assert.Empty(t, res.pending, "transient error must not be misclassified as pending")
				assert.Empty(t, res.invalid, "transient error must not be misclassified as invalid")
				return
			}

			require.NoError(t, err, "NotFound is a legitimate skip, not an error")
			assert.Empty(t, cands, "missing candidate yields no candidates")
			assert.Len(t, res.pending, tc.wantPending)
			assert.Len(t, res.invalid, tc.wantInvalid)
		})
	}
}

// TestValidateToolspec_TransientGetError_RequeuesNotPermanentDeny exercises the
// full audit scenario end to end: the only spec that could authorize the tool
// is unreadable due to a transient error. The outcome must be a returned error
// (the reconciler requeues with backoff), NOT acceptedBy=="" with a permanent
// "no spec authorizes" failure.
func TestValidateToolspec_TransientGetError_RequeuesNotPermanentDeny(t *testing.T) {
	const specName = "echo-spec"
	const tool = "echo"

	sch := gatherTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(sch).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, _ client.WithWatch, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
				return apierrors.NewServiceUnavailable("apiserver is having a moment")
			},
		}).Build()

	r := &Reconciler{Client: c}
	sess := sessionForTool(tool, specName)
	tc := toolCallFor(tool)

	acceptedBy, failures, err := r.validateToolspec(context.Background(), tc, sess, &resolvedCall{session: sess})

	require.Error(t, err, "transient read of the only authorizing spec must requeue via a returned error")
	assert.Empty(t, acceptedBy, "must not accept on a transient error")
	assert.Empty(t, failures, "must not emit a permanent 'no spec authorizes' deny on a transient error")
}

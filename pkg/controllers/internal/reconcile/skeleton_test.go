package reconcile_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	apreconcile "github.com/authzed/openagentprimitives/pkg/controllers/internal/reconcile"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s))
	return s
}

func newPod(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
	}
}

// statusUpdateRecord tracks Status().Update calls.
type statusUpdateRecord struct {
	calls int
}

func newClientCountingStatus(t *testing.T, rec *statusUpdateRecord, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&corev1.Pod{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				rec.calls++
				return c.SubResource(sub).Update(ctx, obj, opts...)
			},
		}).
		Build()
}

func newClientFailingStatusUpdate(t *testing.T, failErr error, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&corev1.Pod{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				return failErr
			},
		}).
		Build()
}

func TestRunPhases_AllRunWhenNoneStop(t *testing.T) {
	rec := &statusUpdateRecord{}
	pod := newPod("a")
	c := newClientCountingStatus(t, rec, pod)

	calls := 0
	phases := []apreconcile.Phase{
		func(context.Context) apreconcile.Outcome { calls++; return apreconcile.Continue() },
		func(context.Context) apreconcile.Outcome { calls++; return apreconcile.Continue() },
		func(context.Context) apreconcile.Outcome { calls++; return apreconcile.Continue() },
	}
	require.NoError(t, apreconcile.RunPhases(context.Background(), c, pod, phases))
	assert.Equal(t, 3, calls, "phase calls")
	assert.Equal(t, 1, rec.calls, "status updates")
}

func TestRunPhases_StopHaltsButStillUpdates(t *testing.T) {
	rec := &statusUpdateRecord{}
	pod := newPod("a")
	c := newClientCountingStatus(t, rec, pod)

	calls := 0
	phases := []apreconcile.Phase{
		func(context.Context) apreconcile.Outcome { calls++; return apreconcile.Continue() },
		func(context.Context) apreconcile.Outcome { calls++; return apreconcile.StopAfter() },
		func(context.Context) apreconcile.Outcome { calls++; return apreconcile.Continue() },
	}
	require.NoError(t, apreconcile.RunPhases(context.Background(), c, pod, phases))
	assert.Equal(t, 2, calls, "stopped after second phase")
	assert.Equal(t, 1, rec.calls, "status updates")
}

func TestRunPhases_ErrHaltsAndPropagates(t *testing.T) {
	rec := &statusUpdateRecord{}
	pod := newPod("a")
	c := newClientCountingStatus(t, rec, pod)

	wantErr := errors.New("boom")
	calls := 0
	phases := []apreconcile.Phase{
		func(context.Context) apreconcile.Outcome { calls++; return apreconcile.Continue() },
		func(context.Context) apreconcile.Outcome { calls++; return apreconcile.FailWith(wantErr) },
		func(context.Context) apreconcile.Outcome { calls++; return apreconcile.Continue() },
	}
	err := apreconcile.RunPhases(context.Background(), c, pod, phases)
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr)
	assert.Equal(t, 2, calls, "phase calls")
	assert.Equal(t, 1, rec.calls, "status update should still be called even on err")
}

func TestRunPhases_StatusUpdateErrSurfacedWhenNoPhaseErr(t *testing.T) {
	pod := newPod("a")
	updateErr := errors.New("status borked")
	c := newClientFailingStatusUpdate(t, updateErr, pod)

	phases := []apreconcile.Phase{
		func(context.Context) apreconcile.Outcome { return apreconcile.Continue() },
	}
	err := apreconcile.RunPhases(context.Background(), c, pod, phases)
	require.Error(t, err)
	assert.ErrorIs(t, err, updateErr)
}

func TestRunPhases_PhaseErrTakesPrecedenceOverUpdateErr(t *testing.T) {
	pod := newPod("a")
	updateErr := errors.New("status borked")
	c := newClientFailingStatusUpdate(t, updateErr, pod)

	phaseErr := errors.New("phase failed")
	phases := []apreconcile.Phase{
		func(context.Context) apreconcile.Outcome { return apreconcile.FailWith(phaseErr) },
	}
	err := apreconcile.RunPhases(context.Background(), c, pod, phases)
	require.Error(t, err)
	assert.ErrorIs(t, err, phaseErr)
	assert.False(t, errors.Is(err, updateErr), "update err should not be returned when phase err present")
}

func TestRunPhases_EmptyListStillUpdatesStatus(t *testing.T) {
	rec := &statusUpdateRecord{}
	pod := newPod("a")
	c := newClientCountingStatus(t, rec, pod)

	require.NoError(t, apreconcile.RunPhases(context.Background(), c, pod, nil))
	assert.Equal(t, 1, rec.calls, "status updates")
}

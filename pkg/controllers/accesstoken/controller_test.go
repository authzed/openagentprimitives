package accesstoken

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
)

type fakeDeleter struct {
	deleted []string
	err     error
}

func (f *fakeDeleter) DeleteAccessTokenTuples(_ context.Context, tokenID string) error {
	if f.err != nil {
		return f.err
	}
	f.deleted = append(f.deleted, tokenID)
	return nil
}

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s))
	return s
}

func newToken(expiry time.Time) *spiceboxv1alpha1.AccessToken {
	return &spiceboxv1alpha1.AccessToken{
		ObjectMeta: metav1.ObjectMeta{Name: "at-0123456789ab", Namespace: "agentprimitives-system"},
		Spec: spiceboxv1alpha1.AccessTokenSpec{
			TokenHash: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			Owner:     "dXNlckBleGFtcGxlLmNvbQ",
			ExpiresAt: metav1.NewTime(expiry),
		},
	}
}

func reconcileOnce(t *testing.T, r *Reconciler, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&spiceboxv1alpha1.AccessToken{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "agentprimitives-system"},
		})})
	require.NoError(t, err, "Reconcile")
	return res
}

func TestLiveTokenGetsFinalizerReadyAndExpiryRequeue(t *testing.T) {
	now := time.Now()
	tok := newToken(now.Add(time.Hour))
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(tok).WithStatusSubresource(tok).Build()
	r := &Reconciler{Client: c, SpiceDB: &fakeDeleter{}, Clock: func() time.Time { return now }}

	res := reconcileOnce(t, r, tok.Name)

	var got spiceboxv1alpha1.AccessToken
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(tok), &got))
	assert.True(t, controllerutil.ContainsFinalizer(&got, spiceboxv1alpha1.FinalizerAccessToken))
	assert.True(t, conditions.IsTrue(got.Status.Conditions, spiceboxv1alpha1.AccessTokenConditionReady))
	assert.InDelta(t, time.Hour, res.RequeueAfter, float64(time.Minute), "requeue lands at expiry")
}

func TestExpiredTokenIsDeleted(t *testing.T) {
	now := time.Now()
	tok := newToken(now.Add(-time.Minute))
	controllerutil.AddFinalizer(tok, spiceboxv1alpha1.FinalizerAccessToken)
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(tok).WithStatusSubresource(tok).Build()
	del := &fakeDeleter{}
	r := &Reconciler{Client: c, SpiceDB: del, Clock: func() time.Time { return now }}

	reconcileOnce(t, r, tok.Name) // issues the Delete
	reconcileOnce(t, r, tok.Name) // finalizes

	var got spiceboxv1alpha1.AccessToken
	err := c.Get(context.Background(), client.ObjectKeyFromObject(tok), &got)
	assert.True(t, apierrors.IsNotFound(err), "expired token CR is gone")
	assert.Equal(t, []string{tok.Name}, del.deleted, "tuples removed during finalization")
}

func TestDeletionRemovesTuplesThenFinalizer(t *testing.T) {
	now := time.Now()
	tok := newToken(now.Add(time.Hour))
	tok.Finalizers = []string{spiceboxv1alpha1.FinalizerAccessToken}
	tok.DeletionTimestamp = &metav1.Time{Time: now}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(tok).WithStatusSubresource(tok).Build()
	del := &fakeDeleter{}
	r := &Reconciler{Client: c, SpiceDB: del, Clock: func() time.Time { return now }}

	reconcileOnce(t, r, tok.Name)

	assert.Equal(t, []string{tok.Name}, del.deleted)
	var got spiceboxv1alpha1.AccessToken
	err := c.Get(context.Background(), client.ObjectKeyFromObject(tok), &got)
	assert.True(t, apierrors.IsNotFound(err), "finalizer released, object gone")
}

func TestTupleDeleteFailureKeepsFinalizer(t *testing.T) {
	now := time.Now()
	tok := newToken(now.Add(time.Hour))
	tok.Finalizers = []string{spiceboxv1alpha1.FinalizerAccessToken}
	tok.DeletionTimestamp = &metav1.Time{Time: now}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(tok).WithStatusSubresource(tok).Build()
	r := &Reconciler{Client: c, SpiceDB: &fakeDeleter{err: errors.New("spicedb down")}, Clock: func() time.Time { return now }}

	_, err := r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: client.ObjectKeyFromObject(tok)})
	require.Error(t, err, "failed tuple delete must surface and retry")

	var got spiceboxv1alpha1.AccessToken
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(tok), &got))
	assert.True(t, controllerutil.ContainsFinalizer(&got, spiceboxv1alpha1.FinalizerAccessToken),
		"finalizer stays until tuples are actually gone — fail closed")
}

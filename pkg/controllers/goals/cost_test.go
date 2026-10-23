package goals

import (
	"context"
	"errors"
	"testing"
	"time"

	domain "github.com/authzed/openagentprimitives/pkg/agent/goals"
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type failedCostStore struct {
	domain.OccurrenceStore
	domain.RunStore
}

func (*failedCostStore) RecordCost(context.Context, domain.Occurrence, domain.RunCost, time.Time) (domain.Occurrence, error) {
	return domain.Occurrence{}, errors.New("accounting unavailable")
}
func (*failedCostStore) RecordOutcome(_ context.Context, o domain.Occurrence, r domain.RunReason, at time.Time) (domain.Occurrence, error) {
	o.Outcome = &domain.RunOutcome{Reason: r, ObservedAt: at}
	return o, nil
}

func TestAccountingOutageDoesNotPreventGoalCancellation(t *testing.T) {
	ctx := context.Background()
	o := domain.Occurrence{ID: "occurrence", SessionUID: "root-uid", SessionName: "root", Domain: domain.Domain{Namespace: "team"}}
	sess := &v1.AgentSession{ObjectMeta: metav1.ObjectMeta{Namespace: o.Domain.Namespace, Name: o.SessionName, UID: "root-uid"}, Spec: v1.AgentSessionSpec{GoalExecution: ref(o)}}
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(scheme))
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sess).Build()
	d := &Dispatcher{Store: &failedCostStore{}, Client: k8s, Reader: k8s}
	require.ErrorContains(t, d.observeAndStop(ctx, o, sess, true, domain.RunCancelled), "accounting unavailable")
	require.True(t, apierrors.IsNotFound(k8s.Get(ctx, client.ObjectKeyFromObject(sess), &v1.AgentSession{})), "accounting cannot hold a cancelled runner open")
}

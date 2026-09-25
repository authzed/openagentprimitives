package monitoring

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
)

// recorder is a thread-safe channelevents.PublishFunc that decodes every
// published MonitoringEvent.
type recorder struct {
	mu     sync.Mutex
	events []channelevents.MonitoringEvent
}

func (r *recorder) publish(_ string, data []byte) error {
	var ev channelevents.MonitoringEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return err
	}
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
	return nil
}

func (r *recorder) snapshot() []channelevents.MonitoringEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]channelevents.MonitoringEvent(nil), r.events...)
}

func agentIdentityTarget(t *testing.T) Target {
	t.Helper()
	for _, tg := range Targets() {
		if tg.GVKName == "AgentIdentity" {
			return tg
		}
	}
	t.Fatal("AgentIdentity target missing")
	return Target{}
}

func aiWithRefresh(status metav1.ConditionStatus, reason string) *spiceboxv1alpha1.AgentIdentity {
	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "github-bot"},
	}
	ai.Status.Conditions = []metav1.Condition{
		{Type: "Refresh", Status: status, Reason: reason, Message: "detail"},
	}
	return ai
}

func reconcileAI(t *testing.T, r *Reconciler) {
	t.Helper()
	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: client.ObjectKey{Namespace: "default", Name: "github-bot"},
	})
	require.NoError(t, err)
}

func TestReconciler_EmitsOnTransitionsOnly(t *testing.T) {
	scheme := testfixtures.NewScheme(t)
	rec := &recorder{}
	ai := aiWithRefresh(metav1.ConditionFalse, "TokenEndpointError")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ai).WithStatusSubresource(&spiceboxv1alpha1.AgentIdentity{}).Build()
	r := &Reconciler{Client: c, Publish: rec.publish, Target: agentIdentityTarget(t), tracker: newTracker()}

	reconcileAI(t, r)
	events := rec.snapshot()
	require.Len(t, events, 1)
	assert.Equal(t, channelevents.MonitoringTransitionFailed, events[0].Transition)
	assert.Equal(t, channelevents.MonitoringLevelError, events[0].Level)
	assert.Equal(t, "credential", events[0].Category)
	assert.Equal(t, "AgentIdentity", events[0].Source.Kind)
	assert.Equal(t, "github-bot", events[0].Source.Name)
	assert.Equal(t, "TokenEndpointError", events[0].Reason)
	assert.Contains(t, events[0].Hint, "oap identity refresh github-bot")

	reconcileAI(t, r)
	assert.Len(t, rec.snapshot(), 1, "no event on an unchanged condition")
}

func TestReconciler_EmitsRecovery(t *testing.T) {
	scheme := testfixtures.NewScheme(t)
	rec := &recorder{}
	ai := aiWithRefresh(metav1.ConditionFalse, "TokenEndpointError")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ai).WithStatusSubresource(&spiceboxv1alpha1.AgentIdentity{}).Build()
	r := &Reconciler{Client: c, Publish: rec.publish, Target: agentIdentityTarget(t), tracker: newTracker()}

	reconcileAI(t, r) // failed

	// Flip the condition to True. Get first so the Update carries the
	// current resourceVersion, then reconcile again.
	var cur spiceboxv1alpha1.AgentIdentity
	require.NoError(t, c.Get(context.Background(),
		client.ObjectKey{Namespace: "default", Name: "github-bot"}, &cur))
	cur.Status.Conditions = []metav1.Condition{
		{Type: "Refresh", Status: metav1.ConditionTrue, Reason: "RefreshSucceeded", Message: "ok"},
	}
	require.NoError(t, c.Status().Update(context.Background(), &cur))
	reconcileAI(t, r)

	events := rec.snapshot()
	require.Len(t, events, 2)
	assert.Equal(t, channelevents.MonitoringTransitionRecovered, events[1].Transition)
	assert.Equal(t, channelevents.MonitoringLevelError, events[1].Level)
}

func TestReconciler_HealthyCondition_NoEmit(t *testing.T) {
	scheme := testfixtures.NewScheme(t)
	rec := &recorder{}
	ai := aiWithRefresh(metav1.ConditionTrue, "RefreshSucceeded")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ai).WithStatusSubresource(&spiceboxv1alpha1.AgentIdentity{}).Build()
	r := &Reconciler{Client: c, Publish: rec.publish, Target: agentIdentityTarget(t), tracker: newTracker()}

	reconcileAI(t, r)
	assert.Empty(t, rec.snapshot(), "a healthy condition emits nothing")
}

func TestReconciler_AbsentObject_NoEmitAndNoError(t *testing.T) {
	scheme := testfixtures.NewScheme(t)
	rec := &recorder{}
	c := fake.NewClientBuilder().WithScheme(scheme).Build() // object absent
	r := &Reconciler{Client: c, Publish: rec.publish, Target: agentIdentityTarget(t), tracker: newTracker()}

	reconcileAI(t, r)
	assert.Empty(t, rec.snapshot(), "a missing object emits nothing")
}

func TestReconciler_DeleteThenRecreate_EmitsFailed(t *testing.T) {
	scheme := testfixtures.NewScheme(t)
	rec := &recorder{}
	ai := aiWithRefresh(metav1.ConditionFalse, "TokenEndpointError")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ai).
		WithStatusSubresource(&spiceboxv1alpha1.AgentIdentity{}).Build()
	r := &Reconciler{Client: c, Publish: rec.publish, Target: agentIdentityTarget(t), tracker: newTracker()}

	reconcileAI(t, r) // first observe: emits failed
	require.Len(t, rec.snapshot(), 1)

	// Delete the object: the NotFound reconcile must call tracker.forget.
	require.NoError(t, c.Delete(context.Background(), ai))
	reconcileAI(t, r)
	require.Len(t, rec.snapshot(), 1, "delete itself emits nothing")

	// Recreate in the same failing state — after forget, this is a fresh transition.
	ai2 := aiWithRefresh(metav1.ConditionFalse, "TokenEndpointError")
	require.NoError(t, c.Create(context.Background(), ai2))
	// The fake client splits spec and status; set the conditions via Status().Update
	// so the Reconcile sees them on the Get.
	var cur spiceboxv1alpha1.AgentIdentity
	require.NoError(t, c.Get(context.Background(),
		client.ObjectKey{Namespace: "default", Name: "github-bot"}, &cur))
	cur.Status.Conditions = []metav1.Condition{
		{Type: "Refresh", Status: metav1.ConditionFalse, Reason: "TokenEndpointError", Message: "detail"},
	}
	require.NoError(t, c.Status().Update(context.Background(), &cur))
	reconcileAI(t, r)
	events := rec.snapshot()
	require.Len(t, events, 2, "re-created failing object re-emits failed after forget")
	assert.Equal(t, channelevents.MonitoringTransitionFailed, events[1].Transition)
}

func TestReconciler_MultiRule_BothFailing_EmitsBoth(t *testing.T) {
	scheme := testfixtures.NewScheme(t)
	rec := &recorder{}
	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "github-bot"},
	}
	ai.Status.Conditions = []metav1.Condition{
		{Type: "Refresh", Status: metav1.ConditionFalse, Reason: "TokenEndpointError", Message: "x"},
		{Type: "Valid", Status: metav1.ConditionFalse, Reason: "SecretMissing", Message: "y"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ai).
		WithStatusSubresource(&spiceboxv1alpha1.AgentIdentity{}).Build()
	r := &Reconciler{Client: c, Publish: rec.publish, Target: agentIdentityTarget(t), tracker: newTracker()}

	reconcileAI(t, r)
	events := rec.snapshot()
	require.Len(t, events, 2, "one event per failing rule")
	conditions := []string{events[0].Condition, events[1].Condition}
	assert.Contains(t, conditions, "Refresh")
	assert.Contains(t, conditions, "Valid")
}

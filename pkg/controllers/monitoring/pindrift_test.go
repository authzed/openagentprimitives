package monitoring

import (
	"context"
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

func mcpServerTarget(t *testing.T) Target {
	t.Helper()
	for _, tg := range Targets() {
		if tg.GVKName == "MCPServer" {
			return tg
		}
	}
	t.Fatal("MCPServer target missing")
	return Target{}
}

func mcpWithPinDrift(status metav1.ConditionStatus, reason, msg string) *spiceboxv1alpha1.MCPServer {
	srv := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "gh-mcp"},
	}
	srv.Status.Conditions = []metav1.Condition{
		{Type: spiceboxv1alpha1.PinDriftCondition, Status: status, Reason: reason, Message: msg},
	}
	return srv
}

func reconcileMCP(t *testing.T, r *Reconciler) {
	t.Helper()
	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: client.ObjectKey{Namespace: "default", Name: "gh-mcp"},
	})
	require.NoError(t, err)
}

func TestMCPServerPinDrift_EmitsWarningOnDrift(t *testing.T) {
	// PinDrift=False/PinDrifted → monitoring event with level=warning, category=pinning.
	scheme := testfixtures.NewScheme(t)
	rec := &recorder{}
	srv := mcpWithPinDrift(metav1.ConditionFalse, spiceboxv1alpha1.ReasonPinDrifted,
		"tools/list manifest drifted from pinned baseline: sha256:old -> sha256:new; set spec.pinnedManifestHash to accept")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(srv).
		WithStatusSubresource(&spiceboxv1alpha1.MCPServer{}).Build()
	r := &Reconciler{Client: c, Publish: rec.publish, Target: mcpServerTarget(t), tracker: newTracker()}

	reconcileMCP(t, r)

	events := rec.snapshot()
	// Filter to PinDrift events (Valid may also fire if absent/false).
	var pinEvents []channelevents.MonitoringEvent
	for _, ev := range events {
		if ev.Condition == spiceboxv1alpha1.PinDriftCondition {
			pinEvents = append(pinEvents, ev)
		}
	}
	require.Len(t, pinEvents, 1, "expected exactly one PinDrift event")
	assert.Equal(t, channelevents.MonitoringTransitionFailed, pinEvents[0].Transition)
	assert.Equal(t, channelevents.MonitoringLevelWarning, pinEvents[0].Level)
	assert.Equal(t, "pinning", pinEvents[0].Category)
	assert.Equal(t, "MCPServer", pinEvents[0].Source.Kind)
	assert.Equal(t, "gh-mcp", pinEvents[0].Source.Name)
	assert.Equal(t, spiceboxv1alpha1.ReasonPinDrifted, pinEvents[0].Reason)
}

func TestMCPServerPinDrift_EmitsRecovery(t *testing.T) {
	// PinDrift: False → True transition emits a recovery event.
	scheme := testfixtures.NewScheme(t)
	rec := &recorder{}
	srv := mcpWithPinDrift(metav1.ConditionFalse, spiceboxv1alpha1.ReasonPinDrifted, "drift detected")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(srv).
		WithStatusSubresource(&spiceboxv1alpha1.MCPServer{}).Build()
	r := &Reconciler{Client: c, Publish: rec.publish, Target: mcpServerTarget(t), tracker: newTracker()}

	reconcileMCP(t, r) // failed transition

	// Flip to healthy (PinMatch).
	var cur spiceboxv1alpha1.MCPServer
	require.NoError(t, c.Get(context.Background(),
		client.ObjectKey{Namespace: "default", Name: "gh-mcp"}, &cur))
	cur.Status.Conditions = []metav1.Condition{
		{Type: spiceboxv1alpha1.PinDriftCondition, Status: metav1.ConditionTrue,
			Reason: spiceboxv1alpha1.ReasonPinMatch, Message: "hash verified"},
	}
	require.NoError(t, c.Status().Update(context.Background(), &cur))
	reconcileMCP(t, r)

	// Find the recovery event for PinDrift.
	var recovered bool
	for _, ev := range rec.snapshot() {
		if ev.Condition == spiceboxv1alpha1.PinDriftCondition &&
			ev.Transition == channelevents.MonitoringTransitionRecovered {
			recovered = true
		}
	}
	assert.True(t, recovered, "expected a PinDrift recovery event after hash re-matches")
}

func TestMCPServerPinDrift_HealthyNoEmit(t *testing.T) {
	// A healthy PinDrift=True condition emits nothing on first observe.
	scheme := testfixtures.NewScheme(t)
	rec := &recorder{}
	srv := mcpWithPinDrift(metav1.ConditionTrue, spiceboxv1alpha1.ReasonPinMatch, "hash verified")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(srv).
		WithStatusSubresource(&spiceboxv1alpha1.MCPServer{}).Build()
	r := &Reconciler{Client: c, Publish: rec.publish, Target: mcpServerTarget(t), tracker: newTracker()}

	reconcileMCP(t, r)

	for _, ev := range rec.snapshot() {
		assert.NotEqual(t, spiceboxv1alpha1.PinDriftCondition, ev.Condition,
			"healthy PinDrift must not emit a monitoring event")
	}
}

// ---------------------------------------------------------------------------
// SidecarToolbox PinDrift monitoring tests
// ---------------------------------------------------------------------------

func sidecarToolboxTarget(t *testing.T) Target {
	t.Helper()
	for _, tg := range Targets() {
		if tg.GVKName == "SidecarToolbox" {
			return tg
		}
	}
	t.Fatal("SidecarToolbox target missing")
	return Target{}
}

func sidecarWithPinDrift(status metav1.ConditionStatus, reason, msg string) *spiceboxv1alpha1.SidecarToolbox {
	tb := &spiceboxv1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "echo-sidecar"},
	}
	tb.Status.Conditions = []metav1.Condition{
		{Type: spiceboxv1alpha1.PinDriftCondition, Status: status, Reason: reason, Message: msg},
	}
	return tb
}

func reconcileSidecar(t *testing.T, r *Reconciler) {
	t.Helper()
	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: client.ObjectKey{Namespace: "default", Name: "echo-sidecar"},
	})
	require.NoError(t, err)
}

func TestSidecarToolboxPinDrift_EmitsWarningOnDrift(t *testing.T) {
	// PinDrift=False/PinDrifted → monitoring event with level=warning, category=pinning.
	scheme := testfixtures.NewScheme(t)
	rec := &recorder{}
	tb := sidecarWithPinDrift(metav1.ConditionFalse, spiceboxv1alpha1.ReasonPinDrifted,
		"image tag :v1 now resolves to a different digest; pin by digest to accept")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tb).
		WithStatusSubresource(&spiceboxv1alpha1.SidecarToolbox{}).Build()
	r := &Reconciler{Client: c, Publish: rec.publish, Target: sidecarToolboxTarget(t), tracker: newTracker()}

	reconcileSidecar(t, r)

	events := rec.snapshot()
	var pinEvents []channelevents.MonitoringEvent
	for _, ev := range events {
		if ev.Condition == spiceboxv1alpha1.PinDriftCondition {
			pinEvents = append(pinEvents, ev)
		}
	}
	require.Len(t, pinEvents, 1, "expected exactly one PinDrift event")
	assert.Equal(t, channelevents.MonitoringTransitionFailed, pinEvents[0].Transition)
	assert.Equal(t, channelevents.MonitoringLevelWarning, pinEvents[0].Level)
	assert.Equal(t, "pinning", pinEvents[0].Category)
	assert.Equal(t, "SidecarToolbox", pinEvents[0].Source.Kind)
	assert.Equal(t, "echo-sidecar", pinEvents[0].Source.Name)
	assert.Equal(t, spiceboxv1alpha1.ReasonPinDrifted, pinEvents[0].Reason)
}

func TestSidecarToolboxPinDrift_EmitsRecovery(t *testing.T) {
	// PinDrift: False → True transition emits a recovery event.
	scheme := testfixtures.NewScheme(t)
	rec := &recorder{}
	tb := sidecarWithPinDrift(metav1.ConditionFalse, spiceboxv1alpha1.ReasonPinDrifted, "drift detected")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tb).
		WithStatusSubresource(&spiceboxv1alpha1.SidecarToolbox{}).Build()
	r := &Reconciler{Client: c, Publish: rec.publish, Target: sidecarToolboxTarget(t), tracker: newTracker()}

	reconcileSidecar(t, r) // failed transition

	// Flip to healthy (PinMatch).
	var cur spiceboxv1alpha1.SidecarToolbox
	require.NoError(t, c.Get(context.Background(),
		client.ObjectKey{Namespace: "default", Name: "echo-sidecar"}, &cur))
	cur.Status.Conditions = []metav1.Condition{
		{Type: spiceboxv1alpha1.PinDriftCondition, Status: metav1.ConditionTrue,
			Reason: spiceboxv1alpha1.ReasonPinMatch, Message: "digest verified"},
	}
	require.NoError(t, c.Status().Update(context.Background(), &cur))
	reconcileSidecar(t, r)

	var recovered bool
	for _, ev := range rec.snapshot() {
		if ev.Condition == spiceboxv1alpha1.PinDriftCondition &&
			ev.Transition == channelevents.MonitoringTransitionRecovered {
			recovered = true
		}
	}
	assert.True(t, recovered, "expected a PinDrift recovery event after digest re-matches")
}

func TestSidecarToolboxPinDrift_HealthyNoEmit(t *testing.T) {
	// A healthy PinDrift=True condition emits nothing on first observe.
	scheme := testfixtures.NewScheme(t)
	rec := &recorder{}
	tb := sidecarWithPinDrift(metav1.ConditionTrue, spiceboxv1alpha1.ReasonPinMatch, "digest verified")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tb).
		WithStatusSubresource(&spiceboxv1alpha1.SidecarToolbox{}).Build()
	r := &Reconciler{Client: c, Publish: rec.publish, Target: sidecarToolboxTarget(t), tracker: newTracker()}

	reconcileSidecar(t, r)

	for _, ev := range rec.snapshot() {
		assert.NotEqual(t, spiceboxv1alpha1.PinDriftCondition, ev.Condition,
			"healthy PinDrift must not emit a monitoring event")
	}
}

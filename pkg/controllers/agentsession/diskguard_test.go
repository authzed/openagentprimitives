// pkg/controllers/agentsession/diskguard_test.go
//
// Unit tests for the workspace-disk guard: the PURE high-watermark decision
// function (assessNodeDisk), the PURE per-node PV-byte / allocatable gatherers,
// and the deduped monitoring-warning wiring (maybeWarnNodeDiskPressure).
package agentsession

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// diskMonRecorder is a thread-safe channelevents.PublishFunc that decodes every
// published MonitoringEvent. (This white-box test is package agentsession; the
// external-test-package monRecorder in bundle_unschedulable_surface_test.go is
// not visible here.)
type diskMonRecorder struct {
	mu     sync.Mutex
	events []channelevents.MonitoringEvent
}

func (m *diskMonRecorder) publish(_ string, data []byte) error {
	var ev channelevents.MonitoringEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return err
	}
	m.mu.Lock()
	m.events = append(m.events, ev)
	m.mu.Unlock()
	return nil
}

func (m *diskMonRecorder) snapshot() []channelevents.MonitoringEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]channelevents.MonitoringEvent(nil), m.events...)
}

// bytesOf parses a quantity string to its byte value (resource.Quantity.Value
// has a pointer receiver, so it can't be called on a MustParse temporary).
func bytesOf(s string) int64 {
	q := resource.MustParse(s)
	return q.Value()
}

func TestAssessNodeDisk(t *testing.T) {
	cases := []struct {
		name         string
		used, alloc  int64
		wantSeverity diskSeverity
	}{
		{"unknown allocatable → OK (fail-safe)", 50, 0, diskSeverityOK},
		{"negative allocatable → OK (fail-safe)", 50, -1, diskSeverityOK},
		{"empty node → OK", 0, 100, diskSeverityOK},
		{"just under warn watermark → OK", 84, 100, diskSeverityOK},
		{"exactly at warn watermark → Warn", 85, 100, diskSeverityWarn},
		{"between warn and critical → Warn", 94, 100, diskSeverityWarn},
		{"exactly at critical watermark → Critical", 95, 100, diskSeverityCritical},
		{"full → Critical", 100, 100, diskSeverityCritical},
		{"over-committed (used > alloc) → Critical", 120, 100, diskSeverityCritical},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := assessNodeDisk(tc.used, tc.alloc)
			assert.Equal(t, tc.wantSeverity, got.Severity, "Severity")
			assert.Equal(t, tc.used, got.UsedBytes)
			assert.Equal(t, tc.alloc, got.AllocBytes)
			// OverWatermark is exactly "not OK".
			assert.Equal(t, tc.wantSeverity != diskSeverityOK, got.OverWatermark())
		})
	}
}

// workspacePV builds a PersistentVolume of the given class, affined to node via
// the hostPath provisioner's kubernetes.io/hostname node selector (node="" omits
// affinity), with the given capacity and phase.
func workspacePV(name, class, node, capacity string, phase corev1.PersistentVolumePhase) *corev1.PersistentVolume {
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{
			StorageClassName: class,
			Capacity:         corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(capacity)},
		},
		Status: corev1.PersistentVolumeStatus{Phase: phase},
	}
	if node != "" {
		pv.Spec.NodeAffinity = &corev1.VolumeNodeAffinity{
			Required: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{{
					Key:      "kubernetes.io/hostname",
					Operator: corev1.NodeSelectorOpIn,
					Values:   []string{node},
				}},
			}}},
		}
	}
	return pv
}

func nodeWithEphemeral(name, allocatable string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{corev1.ResourceEphemeralStorage: resource.MustParse(allocatable)},
		},
	}
}

func TestWorkspacePVBytesByNode(t *testing.T) {
	const class = cloud.BundledWorkspaceStorageClass
	pvs := []corev1.PersistentVolume{
		*workspacePV("a", class, "n1", "2Gi", corev1.VolumeBound),
		*workspacePV("b", class, "n1", "3Gi", corev1.VolumeReleased), // released still occupies disk
		*workspacePV("c", class, "n2", "1Gi", corev1.VolumeBound),
		*workspacePV("d", class, "n1", "8Gi", corev1.VolumeAvailable),     // not yet holding a session's bytes
		*workspacePV("e", "other-class", "n1", "9Gi", corev1.VolumeBound), // different class: ignored
		*workspacePV("f", class, "", "4Gi", corev1.VolumeBound),           // no node affinity: unattributable
	}
	got := workspacePVBytesByNode(pvs, class)
	assert.Equal(t, bytesOf("5Gi"), got["n1"], "n1 = 2Gi bound + 3Gi released")
	assert.Equal(t, bytesOf("1Gi"), got["n2"], "n2 = 1Gi bound")
	assert.NotContains(t, got, "", "PV with no node affinity is not attributed")
}

func TestEphemeralAllocatableByNode(t *testing.T) {
	nodes := []corev1.Node{
		*nodeWithEphemeral("n1", "100Gi"),
		*nodeWithEphemeral("n2", "50Gi"),
		{ObjectMeta: metav1.ObjectMeta{Name: "n3"}}, // no allocatable reported
	}
	got := ephemeralAllocatableByNode(nodes)
	assert.Equal(t, bytesOf("100Gi"), got["n1"])
	assert.Equal(t, bytesOf("50Gi"), got["n2"])
	assert.NotContains(t, got, "n3", "a node reporting no ephemeral allocatable is omitted")
}

func newDiskGuardReconciler(t *testing.T, rec *diskMonRecorder, class string, objs ...client.Object) *Reconciler {
	t.Helper()
	c := buildFakeClient(t, objs...)
	r := &Reconciler{
		Client:                c,
		APIReader:             c,
		Now:                   func() time.Time { return time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC) },
		WorkspaceStorageClass: class,
	}
	if rec != nil {
		r.MonitoringPublish = rec.publish
	}
	return r
}

func TestMaybeWarnNodeDiskPressure_CriticalEmitsOnceThenDedups(t *testing.T) {
	rec := &diskMonRecorder{}
	// n1: 96Gi of workspace PVs against 100Gi allocatable → 96% → Critical.
	r := newDiskGuardReconciler(t, rec, cloud.BundledWorkspaceStorageClass,
		nodeWithEphemeral("n1", "100Gi"),
		workspacePV("a", cloud.BundledWorkspaceStorageClass, "n1", "96Gi", corev1.VolumeBound),
	)

	r.maybeWarnNodeDiskPressure(context.Background())
	events := rec.snapshot()
	require.Len(t, events, 1, "one warning emitted on first crossing")
	ev := events[0]
	assert.Equal(t, channelevents.MonitoringLevelError, ev.Level, "critical → error level")
	assert.Equal(t, "capacity", ev.Category)
	assert.Equal(t, channelevents.MonitoringTransitionFailed, ev.Transition)
	assert.Equal(t, "Node", ev.Source.Kind)
	assert.Equal(t, "n1", ev.Source.Name)
	assert.NotEmpty(t, ev.Condition, "condition required by MonitoringEvent.Validate")
	assert.Contains(t, ev.Summary, "n1")

	// Second call within the same episode must not re-emit.
	r.maybeWarnNodeDiskPressure(context.Background())
	assert.Len(t, rec.snapshot(), 1, "deduped: still one event for the same episode")
}

func TestMaybeWarnNodeDiskPressure_WarnLevel(t *testing.T) {
	rec := &diskMonRecorder{}
	// 88Gi / 100Gi = 88% → Warn.
	r := newDiskGuardReconciler(t, rec, cloud.BundledWorkspaceStorageClass,
		nodeWithEphemeral("n1", "100Gi"),
		workspacePV("a", cloud.BundledWorkspaceStorageClass, "n1", "88Gi", corev1.VolumeBound),
	)
	r.maybeWarnNodeDiskPressure(context.Background())
	events := rec.snapshot()
	require.Len(t, events, 1)
	assert.Equal(t, channelevents.MonitoringLevelWarning, events[0].Level, "over warn but under critical → warning")
}

func TestMaybeWarnNodeDiskPressure_NonNodePinnedClassNeverWarns(t *testing.T) {
	rec := &diskMonRecorder{}
	// A Filestore (cross-node) class: no single node is at risk, so no warning
	// even though the PVs are huge relative to the node.
	r := newDiskGuardReconciler(t, rec, "enterprise-multishare-rwx",
		nodeWithEphemeral("n1", "100Gi"),
		workspacePV("a", "enterprise-multishare-rwx", "n1", "99Gi", corev1.VolumeBound),
	)
	r.maybeWarnNodeDiskPressure(context.Background())
	assert.Empty(t, rec.snapshot(), "non-node-pinned workspace class puts no node at risk")
}

func TestMaybeWarnNodeDiskPressure_NilPublishIsSafe(t *testing.T) {
	// No MonitoringPublish wired: must not panic and must do nothing.
	r := newDiskGuardReconciler(t, nil, cloud.BundledWorkspaceStorageClass,
		nodeWithEphemeral("n1", "100Gi"),
		workspacePV("a", cloud.BundledWorkspaceStorageClass, "n1", "99Gi", corev1.VolumeBound),
	)
	require.NotPanics(t, func() { r.maybeWarnNodeDiskPressure(context.Background()) })
}

func TestMaybeWarnNodeDiskPressure_EpisodeResetsWhenPressureClears(t *testing.T) {
	rec := &diskMonRecorder{}
	crit := workspacePV("a", cloud.BundledWorkspaceStorageClass, "n1", "96Gi", corev1.VolumeBound)
	r := newDiskGuardReconciler(t, rec, cloud.BundledWorkspaceStorageClass,
		nodeWithEphemeral("n1", "100Gi"), crit)

	r.maybeWarnNodeDiskPressure(context.Background())
	require.Len(t, rec.snapshot(), 1, "first crossing warns")

	// Pressure clears (the session's PV is reclaimed): the dedup marker must be
	// dropped so a future crossing warns again.
	require.NoError(t, r.Client.Delete(context.Background(), crit))
	r.maybeWarnNodeDiskPressure(context.Background())
	assert.Len(t, rec.snapshot(), 1, "no new event while under watermark")

	// Pressure returns: a fresh episode must warn again.
	require.NoError(t, r.Client.Create(context.Background(),
		workspacePV("b", cloud.BundledWorkspaceStorageClass, "n1", "97Gi", corev1.VolumeBound)))
	r.maybeWarnNodeDiskPressure(context.Background())
	assert.Len(t, rec.snapshot(), 2, "re-crossing after recovery warns again")
}

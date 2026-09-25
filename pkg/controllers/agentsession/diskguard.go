// pkg/controllers/agentsession/diskguard.go
//
// The workspace-disk guard warns cluster monitors when a node hosting
// session-workspace PVs is running out of ephemeral storage.
//
// Why it exists: the bundled workspace StorageClass (cloud.BundledWorkspace-
// StorageClass, "ap-workspace-rwx") is a NODE-LOCAL hostPath provisioner — every
// PV's bytes live on ONE node's disk, and the provisioner pins all of a
// session's pods to that node. With no per-volume quota, many concurrent (or
// un-reclaimed terminal) session workspaces pile onto whichever node the
// scheduler favored and can exhaust its ephemeral-storage, wedging every pod on
// that node ("disk-pressure deadlock"). A cross-node RWX class (Filestore/NFS)
// spreads bytes off any single node and is not at risk, so the guard only
// engages for the node-pinned class.
//
// The PRIMARY prevention lives in reconcileStorageReclaim (short-grace reclaim
// of a terminal session's scratch claims when the class is node-pinned). This
// guard is the BACKSTOP: it fans a one-shot-per-episode MonitoringEvent to the
// role=monitoring channel — via the same MonitoringPublish path
// maybeEmitUnschedulable uses — so a human is warned when many concurrent
// sessions outrun reclaim, before the node deadlocks.
package agentsession

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	"github.com/authzed/openagentprimitives/pkg/platform/schedfit"
)

// Watermarks: fractions of a node's ephemeral-storage allocatable consumed by
// workspace PVs at/above which the node is at risk. Warn is early guidance;
// Critical means a deadlock is imminent.
const (
	diskWarnWatermark     = 0.85
	diskCriticalWatermark = 0.95
)

// workspaceHostnameLabelKey is the node label the hostPath provisioner affines
// each workspace PV by — the same key the workspacevolume janitor reads.
const workspaceHostnameLabelKey = "kubernetes.io/hostname"

// diskSeverity ranks how close a node hosting workspace PVs is to exhausting
// its ephemeral storage.
type diskSeverity int

const (
	diskSeverityOK diskSeverity = iota
	diskSeverityWarn
	diskSeverityCritical
)

// diskAssessment is the PURE verdict for one node.
type diskAssessment struct {
	Severity   diskSeverity
	UsedBytes  int64
	AllocBytes int64
	Fraction   float64 // UsedBytes/AllocBytes; 0 when AllocBytes is unknown
}

// OverWatermark reports whether the node crossed any watermark (i.e. not OK).
func (a diskAssessment) OverWatermark() bool { return a.Severity != diskSeverityOK }

// summary renders the operator-facing one-liner for a MonitoringEvent.
func (a diskAssessment) summary(node string) string {
	return fmt.Sprintf(
		"node %s: session workspaces use %s of %s ephemeral-storage allocatable (%.0f%%) on the node-local workspace class",
		node, schedfit.HumanBytes(a.UsedBytes), schedfit.HumanBytes(a.AllocBytes), a.Fraction*100)
}

// assessNodeDisk is the PURE high-watermark decision: given the bytes of
// workspace PVs pinned to a node and that node's ephemeral-storage allocatable,
// it returns the severity. FAIL-SAFE: an unknown (<=0) allocatable never warns
// — missing capacity data must not raise a false alarm, mirroring schedfit's
// Known=false semantics.
func assessNodeDisk(usedBytes, allocBytes int64) diskAssessment {
	a := diskAssessment{UsedBytes: usedBytes, AllocBytes: allocBytes}
	if allocBytes <= 0 {
		return a
	}
	a.Fraction = float64(usedBytes) / float64(allocBytes)
	switch {
	case a.Fraction >= diskCriticalWatermark:
		a.Severity = diskSeverityCritical
	case a.Fraction >= diskWarnWatermark:
		a.Severity = diskSeverityWarn
	}
	return a
}

// pvAffinedHostname returns the node a workspace PV is pinned to via its
// required kubernetes.io/hostname node affinity, or "" when it names none.
// Workspace PVs are pinned to exactly one node, so the first match is it. This
// mirrors workspacevolume.affinedHostnames' node-affinity read (kept local to
// avoid a controller-to-controller import for one small pure helper).
func pvAffinedHostname(pv *corev1.PersistentVolume) string {
	if pv.Spec.NodeAffinity == nil || pv.Spec.NodeAffinity.Required == nil {
		return ""
	}
	for _, term := range pv.Spec.NodeAffinity.Required.NodeSelectorTerms {
		for _, expr := range term.MatchExpressions {
			if expr.Key == workspaceHostnameLabelKey && expr.Operator == corev1.NodeSelectorOpIn && len(expr.Values) > 0 {
				return expr.Values[0]
			}
		}
	}
	return ""
}

// pvCapacityBytes returns a PV's declared storage capacity in bytes, or 0.
func pvCapacityBytes(pv *corev1.PersistentVolume) int64 {
	if q, ok := pv.Spec.Capacity[corev1.ResourceStorage]; ok {
		return q.Value()
	}
	return 0
}

// workspacePVBytesByNode is PURE: it sums, per node, the capacity of PVs of the
// given class that still occupy a node's disk. Bound (in use) and Released (data
// not yet reclaimed) both count; Available/Pending/Failed do not (no session
// bytes on a node yet). A PV with no attributable node is skipped — it cannot be
// charged to anyone.
func workspacePVBytesByNode(pvs []corev1.PersistentVolume, class string) map[string]int64 {
	out := map[string]int64{}
	for i := range pvs {
		pv := &pvs[i]
		if pv.Spec.StorageClassName != class {
			continue
		}
		switch pv.Status.Phase {
		case corev1.VolumeBound, corev1.VolumeReleased:
		default:
			continue
		}
		node := pvAffinedHostname(pv)
		if node == "" {
			continue
		}
		out[node] += pvCapacityBytes(pv)
	}
	return out
}

// ephemeralAllocatableByNode is PURE: it maps each node reporting an
// ephemeral-storage allocatable to that byte count. A node reporting none is
// omitted (assessNodeDisk then treats it as unknown → OK).
func ephemeralAllocatableByNode(nodes []corev1.Node) map[string]int64 {
	out := map[string]int64{}
	for i := range nodes {
		if q, ok := nodes[i].Status.Allocatable[corev1.ResourceEphemeralStorage]; ok {
			out[nodes[i].Name] = q.Value()
		}
	}
	return out
}

// maybeWarnNodeDiskPressure assesses every node hosting session-workspace PVs
// and fans a one-shot-per-episode MonitoringEvent for each node over a
// watermark. It engages ONLY for the node-local bundled class — a cross-node RWX
// class puts no single node at risk — and is a no-op when monitoring is
// unconfigured. Fail-safe: any List error is logged (never swallowed) and skips
// the pass rather than failing the reconcile. The PV and Node lists are served
// from the shared informer cache (both are watched elsewhere in the operator),
// so calling this per provisioning reconcile is cheap.
func (r *Reconciler) maybeWarnNodeDiskPressure(ctx context.Context) {
	if r.MonitoringPublish == nil {
		return
	}
	if r.WorkspaceStorageClass != cloud.BundledWorkspaceStorageClass {
		return
	}
	var pvs corev1.PersistentVolumeList
	if err := r.Client.List(ctx, &pvs); err != nil {
		log.FromContext(ctx).Info("disk guard: list PersistentVolumes failed; skipping node-disk assessment",
			"err", err.Error())
		return
	}
	var nodes corev1.NodeList
	if err := r.Client.List(ctx, &nodes); err != nil {
		log.FromContext(ctx).Info("disk guard: list Nodes failed; skipping node-disk assessment",
			"err", err.Error())
		return
	}
	used := workspacePVBytesByNode(pvs.Items, r.WorkspaceStorageClass)
	alloc := ephemeralAllocatableByNode(nodes.Items)
	for node, usedBytes := range used {
		r.emitNodeDiskWarning(ctx, node, assessNodeDisk(usedBytes, alloc[node]))
	}
	// Clear the dedup marker for any node that no longer hosts workspace PVs at
	// all: its episode has ended (usage 0 ⇒ OK), so a future crossing must warn
	// again. Nodes still present but back under the watermark are cleared inside
	// emitNodeDiskWarning.
	r.diskPressureEmitted.Range(func(k, _ any) bool {
		if _, ok := used[k.(string)]; !ok {
			r.diskPressureEmitted.Delete(k)
		}
		return true
	})
}

// emitNodeDiskWarning publishes a capacity MonitoringEvent for a node once per
// episode, deduped on node name via r.diskPressureEmitted (mirroring
// maybeEmitUnschedulable's per-pod-UID one-shot). An OK assessment clears the
// marker so the next crossing warns again. A publish error is logged, never
// swallowed.
func (r *Reconciler) emitNodeDiskWarning(ctx context.Context, node string, a diskAssessment) {
	if !a.OverWatermark() {
		r.diskPressureEmitted.Delete(node) // episode ended
		return
	}
	if _, loaded := r.diskPressureEmitted.LoadOrStore(node, struct{}{}); loaded {
		return // already warned this episode
	}
	level := channelevents.MonitoringLevelWarning
	if a.Severity == diskSeverityCritical {
		level = channelevents.MonitoringLevelError
	}
	ev := channelevents.MonitoringEvent{
		Level:      level,
		Category:   "capacity",
		Transition: channelevents.MonitoringTransitionFailed,
		Source:     channelevents.MonitoringSourceRef{Kind: "Node", Name: node},
		Condition:  "WorkspaceDiskPressure",
		Reason:     "NodeDiskPressure",
		Summary:    a.summary(node),
		Hint:       "reclaim terminal-session workspaces, reduce concurrent sessions, add a node, or move to a cross-node RWX class via --workspace-storage-class",
		Timestamp:  r.now(),
	}
	if err := channelevents.PublishMonitoring(r.MonitoringPublish, ev); err != nil {
		log.FromContext(ctx).Info("disk guard: publish node-disk monitoring event failed",
			"node", node, "err", err.Error())
	}
}

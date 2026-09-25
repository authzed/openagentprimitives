//go:build e2e

// test/e2e/restart_helpers.go
package e2e

import (
	"context"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pkgmemory "github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/workspace"
)

// noopSnapshotter satisfies workspace.Snapshotter without launching
// any K8s Jobs. The E2E restart tests don't exercise the cp-by-pod
// CLEAN-path workspace clone (which requires real PVCs + Jobs);
// instead they assert the *reconciler-level* fork sequence (memory
// copy, lineage edges, SpiceDB writes, parent supersession).
type noopSnapshotter struct{}

func (noopSnapshotter) Snapshot(_ context.Context, _ workspace.PVCRef, _ workspace.SnapshotHandle) error {
	return nil
}
func (noopSnapshotter) Restore(_ context.Context, _ workspace.SnapshotHandle, _ workspace.PVCRef) error {
	return nil
}
func (noopSnapshotter) GC(_ context.Context, _ workspace.SnapshotHandle) error { return nil }

// SnapshotDone/RestoreDone report instant completion: envtest runs no
// Job controller, so a real Done probe would never observe JobComplete
// and the restart reconciler would poll forever. The scenarios assert
// the fork sequence, not the copy itself.
func (noopSnapshotter) SnapshotDone(_ context.Context, _ workspace.PVCRef, _ workspace.SnapshotHandle) (bool, error) {
	return true, nil
}
func (noopSnapshotter) RestoreDone(_ context.Context, _ workspace.SnapshotHandle, _ workspace.PVCRef) (bool, error) {
	return true, nil
}

// completingSnapshotter is the Snapshotter wired into the e2e ToolCall
// controller. A stateful tool dispatch (stateImpact ∈ {readwrite, external})
// stamps spec.preDispatchSnapshot, and the controller then launches a snapshot
// Job and requeues until it Succeeds BEFORE entering reconcileStreaming. Under
// envtest there is no kubelet to run that Job, so a real (or noop) Snapshotter
// would hang the controller forever (the interactive p2 scenarios time out
// waiting for status.streaming). This fake creates the snapshot Job the
// controller's WaitPreDispatchSnapshot looks up (by workspace.SnapshotJobName)
// and immediately marks it Succeeded, so the controller proceeds to the
// streaming path. Restore/GC are no-ops (the scenarios don't rewind workspaces).
type completingSnapshotter struct{ c client.Client }

func (s completingSnapshotter) Snapshot(ctx context.Context, src workspace.PVCRef, h workspace.SnapshotHandle) error {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: workspace.SnapshotJobName(h), Namespace: src.Namespace},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers:    []corev1.Container{{Name: "snap", Image: "busybox"}},
				},
			},
		},
	}
	if err := s.c.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	// Re-get and mark Succeeded (status is a subresource — ignored on Create).
	// envtest runs no Job controller, so the controller's WaitPreDispatchSnapshot
	// only ever sees the status we stamp here.
	if err := s.c.Get(ctx, client.ObjectKeyFromObject(job), job); err != nil {
		return err
	}
	if job.Status.Succeeded == 0 {
		job.Status.Succeeded = 1
		if err := s.c.Status().Update(ctx, job); err != nil {
			return err
		}
	}
	return nil
}
func (completingSnapshotter) Restore(_ context.Context, _ workspace.SnapshotHandle, _ workspace.PVCRef) error {
	return nil
}
func (completingSnapshotter) GC(_ context.Context, _ workspace.SnapshotHandle) error { return nil }

// SnapshotDone/RestoreDone report instant completion: envtest runs no
// Job controller, so the Jobs this fake creates never gain a
// JobComplete condition — a real probe would spin the reconciler
// forever. Snapshot above already stamps status.Succeeded for
// WaitPreDispatchSnapshot's benefit.
func (completingSnapshotter) SnapshotDone(_ context.Context, _ workspace.PVCRef, _ workspace.SnapshotHandle) (bool, error) {
	return true, nil
}
func (completingSnapshotter) RestoreDone(_ context.Context, _ workspace.SnapshotHandle, _ workspace.PVCRef) (bool, error) {
	return true, nil
}

// MemStore exposes the harness's shared memory facade for tests that
// need to assert the restart reconciler's memory side-effects (copied
// turns, lineage edges, channel_msg_ref entries).
func (h *Harness) MemStore() pkgmemory.Memory {
	return h.memStore
}

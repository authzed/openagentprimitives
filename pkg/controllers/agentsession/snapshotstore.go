package agentsession

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/podspec"
)

// EnsureSnapshotStorePVC ensures the snapshot-store PVC exists for
// sess. Returns the claim name when the workspace storage class is
// configured; "" otherwise (no storage class means workspaces are
// isolated and snapshot capability is unavailable — restart-from-here
// will be gated off until a class is configured). Idempotent.
//
// Exposed as a free function so it's straightforward to unit-test
// against a fake client; the AgentSession reconciler's Reconcile
// calls through.
func EnsureSnapshotStorePVC(ctx context.Context, r *Reconciler, sess *spiceboxv1alpha1.AgentSession) (string, error) {
	if r.WorkspaceStorageClass == "" {
		return "", nil
	}
	name := podspec.SnapshotStoreClaimName(sess)
	var existing corev1.PersistentVolumeClaim
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: sess.Namespace, Name: name}, &existing)
	switch {
	case err == nil:
		return name, nil
	case errors.IsNotFound(err):
		size, serr := r.clampSizeToClassFloor(ctx, r.snapshotStoreSize())
		if serr != nil {
			return "", fmt.Errorf("resolve snapshot-store PVC size: %w", serr)
		}
		pvc := podspec.BuildSnapshotStorePVC(sess, r.WorkspaceStorageClass, size)
		if cerr := r.Client.Create(ctx, pvc); cerr != nil && !errors.IsAlreadyExists(cerr) {
			return "", fmt.Errorf("create snapshot-store PVC %q: %w", name, cerr)
		}
		return name, nil
	default:
		return "", fmt.Errorf("get snapshot-store PVC %q: %w", name, err)
	}
}

func (r *Reconciler) snapshotStoreSize() string {
	if r.SnapshotStoreSize != "" {
		return r.SnapshotStoreSize
	}
	// Default to the WORKSPACE size: a snapshot only has to hold a copy of the
	// workspace, so a fixed 8Gi was oversized — on a node-pinned (local-path)
	// class that doubled a session's node-ephemeral footprint (2Gi workspace +
	// 8Gi snapshot = 10Gi) and drove nodes into disk pressure. Sizing it to the
	// workspace keeps them matched (2Gi + 2Gi by default); the StorageClass floor
	// still raises BOTH on a class that needs it (Filestore multishare -> 10Gi).
	return r.workspaceSize()
}

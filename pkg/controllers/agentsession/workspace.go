package agentsession

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	"github.com/authzed/openagentprimitives/pkg/platform/podspec"
)

// BuildWorkspacePVC constructs the PVC backing an AgentSession's shared
// workspace. It is owner-referenced by the AgentSession so Kubernetes GC
// deletes it when the session is deleted.
//
// Access mode is ReadWriteOnce, not ReadWriteMany: the bundled local-path
// provisioner (nodePathMap mode) stamps each PV with nodeAffinity, pinning all
// of a session's pods to one node, and RWO in k8s ≥1.22 is single-NODE (not
// single-pod) — so the co-located bundle sidecars + runner share it fine. A
// genuine cross-node RWX class (Filestore/NFS) would also satisfy an RWO
// request, so RWO is the portable floor. See config/workspace-provisioner/.
func BuildWorkspacePVC(sess *spiceboxv1alpha1.AgentSession, storageClass, size string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podspec.WorkspaceClaimName(sess),
			Namespace: sess.Namespace,
			Labels: map[string]string{
				"agentprimitives.authzed.com/agentsession": sess.Name,
			},
			OwnerReferences: sessionOwnerRef(sess),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: ptr.To(storageClass),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(size),
				},
			},
		},
	}
}

// ensureWorkspacePVC creates the shared workspace PVC if it does not exist.
// Returns (claimName, nil) when a shared workspace is in effect, or ("", nil)
// when no StorageClass is configured (isolated fallback). Idempotent.
func (r *Reconciler) ensureWorkspacePVC(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) (string, error) {
	if r.WorkspaceStorageClass == "" {
		return "", nil // isolated fallback
	}
	name := podspec.WorkspaceClaimName(sess)
	var existing corev1.PersistentVolumeClaim
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: sess.Namespace, Name: name}, &existing)
	switch {
	case err == nil:
		return name, nil
	case errors.IsNotFound(err):
		size, serr := r.clampSizeToClassFloor(ctx, r.workspaceSize())
		if serr != nil {
			return "", fmt.Errorf("resolve workspace PVC size: %w", serr)
		}
		pvc := BuildWorkspacePVC(sess, r.WorkspaceStorageClass, size)
		if cerr := r.Client.Create(ctx, pvc); cerr != nil && !errors.IsAlreadyExists(cerr) {
			return "", fmt.Errorf("create workspace PVC %q: %w", name, cerr)
		}
		return name, nil
	default:
		return "", fmt.Errorf("get workspace PVC %q: %w", name, err)
	}
}

func (r *Reconciler) workspaceSize() string {
	if r.WorkspaceSize != "" {
		return r.WorkspaceSize
	}
	return "2Gi"
}

// clampSizeToClassFloor returns requested, raised to r.WorkspaceStorageClass's
// known minimum provisionable size when requested falls below it. It is the
// runtime counterpart of the install-time PVC-floor preflight
// (cmd/oap/internal/installcmd/pvcfloor.go): the per-session workspace and
// snapshot-store PVCs are created by the operator, not by the install bundle, so
// that preflight never sees them. Without this, a 2Gi workspace / 8Gi
// snapshot-store request lands below a class floor — e.g. Filestore multishare's
// 10 GiB minimum share — and the PVC sits Pending forever with
// "less than minimum share size", wedging the session.
//
// A class with no known floor (cloud.StorageFloor known=false), or a class that
// no longer exists, leaves requested unchanged: the floor only ever RAISES a
// request the backend would otherwise reject, it never blocks. Errors only on a
// malformed size or a StorageClass read failure other than NotFound.
func (r *Reconciler) clampSizeToClassFloor(ctx context.Context, requested string) (string, error) {
	req, err := resource.ParseQuantity(requested)
	if err != nil {
		return "", fmt.Errorf("parse requested size %q: %w", requested, err)
	}
	var sc storagev1.StorageClass
	if gerr := r.Client.Get(ctx, client.ObjectKey{Name: r.WorkspaceStorageClass}, &sc); gerr != nil {
		if errors.IsNotFound(gerr) {
			return requested, nil // class missing → PVC create surfaces it; nothing to floor against
		}
		// Fail open: the floor only RAISES a request the backend would reject
		// anyway, so a StorageClass read failure (e.g. missing RBAC, apiserver
		// blip) must not wedge session creation. Log it — an operator needs to
		// know the floor check was skipped — and proceed with the request as-is.
		log.FromContext(ctx).Info("workspace PVC floor: StorageClass read failed; using requested size unclamped",
			"storageClass", r.WorkspaceStorageClass, "requested", requested, "err", gerr.Error())
		return requested, nil
	}
	if floor, known := cloud.StorageFloor(&sc); known && req.Cmp(floor) < 0 {
		return floor.String(), nil
	}
	return requested, nil
}

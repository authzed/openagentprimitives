// Package workspacevolume reclaims workspace PersistentVolume OBJECTS whose
// data can no longer exist: a Released, hostPath-backed volume of the
// workspace StorageClass, node-affined to a node the cluster no longer has.
//
// Why this controller exists: the workspace provisioner is hostPath-based, so
// a volume's bytes live on one node's local disk and its Delete-reclaim runs
// as a helper pod ON that node. When the autoscaler removes the node, the
// bytes are already gone — but the reclaim can never run (no node to schedule
// its helper on), so the PV parks in Released forever. Session-storage
// retention (the agentsession controller's reconcileStorageReclaim) deletes
// PVCs on a steady cadence, and node churn is routine on an autoscaled
// cluster, so these stranded objects accumulate indefinitely without a
// janitor. This was first cleaned by hand: 92 such objects on one cluster.
//
// What it deliberately does NOT touch, each a fail-safe:
//   - any phase but Released (Bound is in use; Available is claimable),
//   - any StorageClass but the configured workspace class (not ours to judge),
//   - any non-hostPath source (a CSI volume's data survives its node),
//   - a volume with no node affinity (cannot prove the data is gone),
//   - a volume whose named node still exists (the provisioner's own reclaim
//     is the right cleaner there, and it deletes the PV itself when done).
package workspacevolume

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch

// Reconciler is the stranded-workspace-volume janitor. StorageClass is the
// workspace class (the operator's --workspace-storage-class); the reconciler
// judges volumes of that class only.
type Reconciler struct {
	client.Client
	StorageClass string
}

// hostnameLabelKey is the node label hostPath provisioners affine volumes by.
const hostnameLabelKey = "kubernetes.io/hostname"

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pv corev1.PersistentVolume
	if err := r.Client.Get(ctx, req.NamespacedName, &pv); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if pv.Spec.StorageClassName != r.StorageClass ||
		pv.Status.Phase != corev1.VolumeReleased ||
		pv.Spec.HostPath == nil {
		return ctrl.Result{}, nil
	}
	hostnames := affinedHostnames(&pv)
	if len(hostnames) == 0 {
		return ctrl.Result{}, nil // cannot prove the data is gone; keep it
	}
	for _, hostname := range hostnames {
		var node corev1.Node
		err := r.Client.Get(ctx, types.NamespacedName{Name: hostname}, &node)
		if err == nil {
			return ctrl.Result{}, nil // node lives: the provisioner's reclaim owns this volume
		}
		if !apierrors.IsNotFound(err) {
			// Uncertainty is not death: an API error must never be read as
			// "the node is gone". Surface it and retry.
			return ctrl.Result{}, err
		}
	}
	if err := r.Client.Delete(ctx, &pv); client.IgnoreNotFound(err) != nil {
		return ctrl.Result{}, err
	}
	log.FromContext(ctx).Info("workspacevolume: deleted Released volume stranded on a removed node",
		"pv", pv.Name, "hostPath", pv.Spec.HostPath.Path, "nodes", hostnames)
	return ctrl.Result{}, nil
}

// affinedHostnames extracts every kubernetes.io/hostname value the volume's
// required node affinity names. Values across terms/expressions are unioned:
// the volume is stranded only when NONE of them names a live node, which the
// caller checks.
func affinedHostnames(pv *corev1.PersistentVolume) []string {
	if pv.Spec.NodeAffinity == nil || pv.Spec.NodeAffinity.Required == nil {
		return nil
	}
	var out []string
	for _, term := range pv.Spec.NodeAffinity.Required.NodeSelectorTerms {
		for _, expr := range term.MatchExpressions {
			if expr.Key != hostnameLabelKey || expr.Operator != corev1.NodeSelectorOpIn {
				continue
			}
			out = append(out, expr.Values...)
		}
	}
	return out
}

// SetupWithManager watches PersistentVolumes, filtered to the workspace
// StorageClass before anything is enqueued so a busy cluster's unrelated
// volumes never reach the reconciler. Node deletions are the OTHER edge that
// strands a volume, and no PV event fires for them — the periodic resync and
// the Released transition (which always postdates PVC deletion, and usually
// node death for an already-idle session) cover it in practice; a node-watch
// mapping would be cheaper than it is worth for a janitor.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	classMatch := func(obj client.Object) bool {
		pv, ok := obj.(*corev1.PersistentVolume)
		return ok && pv.Spec.StorageClassName == r.StorageClass
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.PersistentVolume{}).
		WithEventFilter(predicate.Funcs{
			CreateFunc:  func(e event.CreateEvent) bool { return classMatch(e.Object) },
			UpdateFunc:  func(e event.UpdateEvent) bool { return classMatch(e.ObjectNew) },
			DeleteFunc:  func(event.DeleteEvent) bool { return false },
			GenericFunc: func(e event.GenericEvent) bool { return classMatch(e.Object) },
		}).
		Named("workspacevolume-janitor").
		Complete(r)
}

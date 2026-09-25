package reconcile

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// EnsureFinalizer adds name to obj's finalizers if absent and persists
// the change with a plain (non-status) Update. It is the dedup of the
// ensure-half of the finalizer prologue that recurs at the top of every
// reconciler:
//
//	if !controllerutil.ContainsFinalizer(obj, name) {
//	    controllerutil.AddFinalizer(obj, name)
//	    if err := c.Update(ctx, obj); err != nil {
//	        return ctrl.Result{}, err
//	    }
//	    return ctrl.Result{Requeue: true}, nil
//	}
//
// The caller owns the return shape so each site keeps its exact existing
// requeue semantics:
//
//	added, err := reconcile.EnsureFinalizer(ctx, r.Client, obj, name)
//	if added || err != nil {
//	    return ctrl.Result{Requeue: added}, err
//	}
//
// added is true only when the finalizer was newly added AND the Update
// succeeded. On Update error, added is false and err is non-nil. When the
// finalizer was already present, returns (false, nil) and the caller
// proceeds. This helper deliberately covers ONLY the ensure-half — the
// removal/finalize half carries bespoke deletion-blocker logic per
// controller and stays at the call site.
func EnsureFinalizer(ctx context.Context, c client.Client, obj client.Object, name string) (added bool, err error) {
	if controllerutil.ContainsFinalizer(obj, name) {
		return false, nil
	}
	controllerutil.AddFinalizer(obj, name)
	if err := c.Update(ctx, obj); err != nil {
		return false, err
	}
	return true, nil
}

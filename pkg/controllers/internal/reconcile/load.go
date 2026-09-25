package reconcile

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// LoadInto fetches the object identified by key into obj. The bool
// return is the "continue reconciling?" signal:
//
//   - true, nil: object found, populated, proceed.
//   - false, nil: object not found (deleted between enqueue and now).
//     Caller should return ctrl.Result{} with no error.
//   - false, err: API error other than NotFound. Caller should return
//     ctrl.Result{} with the error so controller-runtime requeues.
//
// Replaces the boilerplate
//
//	if err := r.Client.Get(ctx, req.NamespacedName, &obj); err != nil {
//	    return ctrl.Result{}, client.IgnoreNotFound(err)
//	}
//
// with
//
//	if cont, err := reconcile.LoadInto(ctx, r.Client, req.NamespacedName, &obj); !cont {
//	    return ctrl.Result{}, err
//	}
//
// The win is intent: the new shape names what we're doing (load-or-exit)
// where the old shape relied on reading IgnoreNotFound carefully.
func LoadInto(ctx context.Context, r client.Reader, key types.NamespacedName, obj client.Object) (bool, error) {
	if err := r.Get(ctx, key, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

package reconcile

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
)

// SetInvalid stamps the top-level status ObservedGeneration, sets the
// given condition to False (reason/message) via the project-wide
// conditions helper, then persists status through the supplied strategy.
// It is the dedup of the near-identical per-controller setInvalid helpers
// that stamp ObservedGeneration + conditions.SetFalse(Valid, …) + persist.
//
// The persist strategy is a closure so each call site keeps its exact
// existing persist semantics: controllers that converged on Status().Patch
// (with a MergeFrom prior) and controllers that use Status().Update both
// pass their own one-liner. The helper does not load a prior; a Patch
// caller captures its DeepCopy'd prior in the closure.
//
// Field-nilling that some call sites do alongside an invalid stamp
// (clearing ResolvedCredentials, AvailableCredentials, …) is per-controller
// residue and stays at the call site, before invoking this helper.
//
//	prior := a.DeepCopy()
//	a.Status.ResolvedCredentials = nil
//	return reconcile.SetInvalid(ctx, &a, &a.Status.ObservedGeneration,
//	    &a.Status.Conditions, ConditionValid, reason, msg,
//	    func(ctx context.Context) error {
//	        return r.Client.Status().Patch(ctx, &a, client.MergeFrom(prior))
//	    })
func SetInvalid(
	ctx context.Context,
	obj conditions.Generationer,
	observedGen *int64,
	conds *[]metav1.Condition,
	condType, reason, msg string,
	persist func(context.Context) error,
) (ctrl.Result, error) {
	*observedGen = obj.GetGeneration()
	conditions.SetFalse(obj, conds, condType, reason, msg)
	return ctrl.Result{}, persist(ctx)
}

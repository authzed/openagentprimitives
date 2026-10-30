package workshop

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// RestoreTokens re-registers the sidecar bearer of every Ready Workshop that
// is not being deleted, reading it back from the bearer Secret, and returns
// how many it registered. It is this controller's tokenrestore.Restorer.
//
// Without it a Workshop's bearer came back only when Reconcile next reached
// step 8, and Reconcile is leader-only: a restarted operator can wait out a
// whole lease before it runs, and the registry would stop answering "retry"
// long before then, refusing a live sidecar for good.
//
// It restores, never mints: a Workshop whose Secret is gone or empty gets
// nothing. Only Ready Workshops are restored, the ones whose bearer step 8
// had registered before the restart; one still provisioning is registered by
// its next Reconcile, before its sidecar has a reason to call. A Workshop
// being deleted is skipped, because teardown may already have revoked its
// bearer, and Reconcile waits for the restore to finish before teardown can
// run, so no revoke lands between this pass reading a Workshop and
// registering it.
//
// One Secret that cannot be read does not stop the rest; its error is
// returned once every Workshop has been tried, so the pass is retried.
func (r *Reconciler) RestoreTokens(ctx context.Context) (int, error) {
	if r.Tokens == nil {
		return 0, nil
	}
	var list spiceboxv1alpha1.WorkshopList
	// Read-only use of the cached objects; the deep copies would only cost.
	if err := r.Client.List(ctx, &list, client.UnsafeDisableDeepCopy); err != nil {
		return 0, fmt.Errorf("list Workshops: %w", err)
	}
	restored := 0
	var errs []error
	for i := range list.Items {
		ws := &list.Items[i]
		if !ws.DeletionTimestamp.IsZero() || ws.Status.Phase != spiceboxv1alpha1.WorkshopPhaseReady {
			continue
		}
		session := ws.Spec.Session.Name
		key := memory.NamespacedName{Namespace: ws.Namespace, Name: spiceboxv1alpha1.WorkshopName(session)}
		if r.Tokens.Registered(key) {
			continue
		}
		secretKey := types.NamespacedName{Namespace: ws.Namespace, Name: spiceboxv1alpha1.WorkshopTokenSecretName(session)}
		var sec corev1.Secret
		// The uncached reader, for the same reason as step 8: the operator's
		// Secret informer is label-filtered and never holds this Secret.
		if err := r.reader().Get(ctx, secretKey, &sec); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			errs = append(errs, fmt.Errorf("workshop %s/%s: read bearer Secret %s: %w", ws.Namespace, ws.Name, secretKey, err))
			continue
		}
		tok := string(sec.Data[workshopSecretTokenKey])
		if tok == "" {
			log.FromContext(ctx).Info("workshop token restore: the bearer Secret carries no token; the memory API will refuse this workshop's sidecar",
				"workshop", ws.Namespace+"/"+ws.Name, "secret", secretKey.String())
			continue
		}
		r.Tokens.Set(key, tok, "" /* no human caller */)
		restored++
	}
	return restored, errors.Join(errs...)
}

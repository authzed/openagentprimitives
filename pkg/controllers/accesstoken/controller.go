// Package accesstoken reconciles AccessToken objects: it guarantees that a
// deleted (or expired) token's SpiceDB tuples are removed, and nothing else.
// Minting — tuple write + CR create — happens in webd at the OAuth token
// endpoint (pkg/web/mcpfront), tuples FIRST, so a freshly minted token is
// checkable before the CR exists. This controller is the cleanup half.
package accesstoken

// Main-resource verbs are minimized to actual client calls: get (LoadInto),
// list;watch (the For informer — and admind's tokens list, hosted in this same
// operator binary), update (EnsureFinalizer/RemoveFinalizer's plain Updates),
// delete (the expiry self-delete, and admind's revoke). No create — minting is
// webd's (pkg/web/mcpfront.Minter) — and no main-resource patch: the only
// patch this reconciler issues targets the status subresource below.
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=accesstokens,verbs=get;list;watch;update;delete
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=accesstokens/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=accesstokens/finalizers,verbs=update

import (
	"context"
	"fmt"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	apreconcile "github.com/authzed/openagentprimitives/pkg/controllers/internal/reconcile"
)

// TupleDeleter is the one SpiceDB capability this reconciler needs. Declared
// as an interface so an unwired SpiceDB stays a genuine nil interface
// (AGENTS.md typed-nil rule).
type TupleDeleter interface {
	DeleteAccessTokenTuples(ctx context.Context, tokenID string) error
}

type Reconciler struct {
	Client  client.Client
	SpiceDB TupleDeleter
	Clock   func() time.Time
}

func (r *Reconciler) now() time.Time {
	if r.Clock != nil {
		return r.Clock()
	}
	return time.Now()
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)
	var tok spiceboxv1alpha1.AccessToken
	if cont, err := apreconcile.LoadInto(ctx, r.Client, req.NamespacedName, &tok); !cont || err != nil {
		return ctrl.Result{}, err
	}

	if tok.DeletionTimestamp != nil {
		return ctrl.Result{}, r.finalize(ctx, &tok)
	}

	// One clock read for the whole pass: the expiry gate and the RequeueAfter
	// below must agree on "now". With two reads straddling the API writes, a
	// clock crossing spec.expiresAt in between yields RequeueAfter <= 0 —
	// which controller-runtime never schedules — so the token would sit
	// unreconciled until the cache resync. (Same idiom as workshop's
	// testwatch: capture once, reuse.)
	now := r.now()

	// Expired: delete the CR; the finalizer path removes the tuples. The
	// tuples self-expire via the expiration trait anyway — this is hygiene,
	// not the enforcement.
	if exp := tok.Spec.ExpiresAt.Time; !exp.IsZero() && now.After(exp) {
		log.Info("deleting expired access token", "token", tok.Name)
		if err := r.Client.Delete(ctx, &tok); client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, fmt.Errorf("delete expired AccessToken %s: %w", tok.Name, err)
		}
		return ctrl.Result{}, nil
	}

	// Finalizer only when SpiceDB is wired: a finalizer that can never have
	// work to do is just a way to wedge a deletion (useridentity precedent).
	if r.SpiceDB != nil {
		if _, err := apreconcile.EnsureFinalizer(ctx, r.Client, &tok, spiceboxv1alpha1.FinalizerAccessToken); err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure AccessToken finalizer: %w", err)
		}
	}

	prior := tok.DeepCopy()
	conditions.SetTrue(&tok, &tok.Status.Conditions, spiceboxv1alpha1.AccessTokenConditionReady, "Observed")
	tok.Status.ObservedGeneration = tok.Generation
	if err := r.Client.Status().Patch(ctx, &tok, client.MergeFrom(prior)); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch AccessToken status: %w", err)
	}

	if exp := tok.Spec.ExpiresAt.Time; !exp.IsZero() {
		return ctrl.Result{RequeueAfter: exp.Sub(now)}, nil
	}
	return ctrl.Result{}, nil
}

func (r *Reconciler) finalize(ctx context.Context, tok *spiceboxv1alpha1.AccessToken) error {
	if !controllerutil.ContainsFinalizer(tok, spiceboxv1alpha1.FinalizerAccessToken) {
		return nil
	}
	if r.SpiceDB == nil {
		ctrllog.FromContext(ctx).Info("AccessToken deleted with no SpiceDB wiring; tuples (if any) are left to their expiration trait", "token", tok.Name)
	} else if err := r.SpiceDB.DeleteAccessTokenTuples(ctx, tok.Name); err != nil {
		return fmt.Errorf("delete SpiceDB tuples for AccessToken %s: %w", tok.Name, err)
	}
	controllerutil.RemoveFinalizer(tok, spiceboxv1alpha1.FinalizerAccessToken)
	return r.Client.Update(ctx, tok)
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&spiceboxv1alpha1.AccessToken{}).
		Complete(r)
}

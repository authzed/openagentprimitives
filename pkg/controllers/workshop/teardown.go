package workshop

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// expiredNoticeBody is what the person who started the build reads when the
// sweeper expires their workshop. Fixed copy, same shape as
// agentsession.startRefusedBody — the browser/Slack surface renders it
// verbatim, so it names nothing internal (no relation, no CR, no namespace).
const expiredNoticeBody = "This build space expired and was cleaned up. Your draft is safe — start a new build to continue from it."

// teardown reverses every layer Reconcile's provisioning path stood up —
// standing (the SpiceDB tuple) first, physical resources after — and
// releases the finalizer only once every one of them is confirmed gone. Any
// error short of the finalizer release is returned as-is: the finalizer
// stays, Kubernetes retries the delete on the next reconcile, and nothing
// this workshop granted is left standing on the strength of a step this
// package cannot prove completed.
func (r *Reconciler) teardown(ctx context.Context, ws *spiceboxv1alpha1.Workshop) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// status.Namespace is the DURABLE ANCHOR (Reconcile's early-anchor step,
	// controller.go) — set-once, persisted before any external state (the
	// namespace, the RBAC objects, the SpiceDB tuple, the bearer Secret)
	// exists. teardown reads it ALONE, with no session fallback: on the
	// ordinary owner-ref GC cascade the session is removed from etcd BEFORE
	// this Workshop is reaped (Workshop's ownerRef points at the session), so
	// a session Get from here would routinely 404 — deriving nsName from a
	// session lookup made the common cascade indistinguishable from "nothing
	// was ever provisioned," which is exactly the bug that let a live tuple
	// and a live namespace outlive their workshop with no log. Because the
	// anchor is written before the tuple can ever exist, an empty
	// status.Namespace here is now a reliable signal: provisioning never got
	// far enough to create anything external, so there is nothing to reverse.
	if ws.Status.Namespace == "" {
		logger.Info("workshop teardown: no provisioned namespace recorded; nothing to reverse",
			"workshop", ws.Namespace+"/"+ws.Name, "session", ws.Spec.Session.Namespace+"/"+ws.Spec.Session.Name)
		controllerutil.RemoveFinalizer(ws, spiceboxv1alpha1.FinalizerWorkshop)
		if err := r.Client.Update(ctx, ws); err != nil {
			return ctrl.Result{}, fmt.Errorf("teardown Workshop %s/%s: remove finalizer (never provisioned): %w", ws.Namespace, ws.Name, err)
		}
		return ctrl.Result{}, nil
	}
	nsName := ws.Status.Namespace

	// 1. The SpiceDB tuples — standing first. DeleteWorkshopRelationships takes
	// every relation on the workshop object, so build and close go together. A
	// nil Tuples dependency means this process cannot prove the relationships
	// are gone, so the workshop stays pinned rather than releasing a finalizer
	// over standing nobody confirmed was reversed.
	if r.Tuples == nil {
		return ctrl.Result{}, fmt.Errorf("teardown Workshop %s/%s: Reconciler.Tuples is not wired; cannot prove the workshop tuples were revoked", ws.Namespace, ws.Name)
	}
	if err := r.Tuples.DeleteWorkshopRelationships(ctx, nsName); err != nil {
		return ctrl.Result{}, fmt.Errorf("teardown Workshop %s/%s: delete workshop relationships for %s: %w", ws.Namespace, ws.Name, nsName, err)
	}

	// 2. The bearer registration. A nil Tokens registry only ever meant this
	// process never served the bearer — Reconcile already fails every such
	// workshop closed before Ready (the TokensReady step) — and the Secret
	// dies with the session's own cascade regardless of whether Revoke runs.
	// Log loudly and proceed rather than pinning a workshop on a dependency
	// that was never load-bearing here.
	if r.Tokens == nil {
		logger.Info("workshop teardown: Reconciler.Tokens is not wired; the bearer Secret will die with the session's cascade instead of an explicit revoke",
			"workshop", ws.Namespace+"/"+ws.Name)
	} else {
		r.Tokens.Revoke(memory.NamespacedName{Namespace: ws.Namespace, Name: spiceboxv1alpha1.WorkshopName(ws.Spec.Session.Name)})
	}

	// 3. Every cluster-scoped tool CR this workshop authored, by label — the
	// one thing owner-ref GC can never reach: SpiceboxToolspec/SpiceboxToolkit
	// are cluster-scoped and so cannot carry a namespaced owner ref to either
	// the Workshop or the workshop namespace.
	if err := r.deleteLabeledToolCRs(ctx, nsName); err != nil {
		return ctrl.Result{}, fmt.Errorf("teardown Workshop %s/%s: %w", ws.Namespace, ws.Name, err)
	}

	// 4. The toolwriter and reader ClusterRoleBindings, by their deterministic
	// names — both cluster-scoped, both unable to carry a namespaced owner
	// ref.
	crbName := WorkshopToolwriterCRBName(ws.Namespace, ws.Spec.Session.Name)
	if err := r.Client.Delete(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: crbName}}); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("teardown Workshop %s/%s: delete toolwriter ClusterRoleBinding %s: %w", ws.Namespace, ws.Name, crbName, err)
	}
	readerCRBName := WorkshopReaderCRBName(ws.Namespace, ws.Spec.Session.Name)
	if err := r.Client.Delete(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: readerCRBName}}); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("teardown Workshop %s/%s: delete reader ClusterRoleBinding %s: %w", ws.Namespace, ws.Name, readerCRBName, err)
	}

	// 5. The workshop namespace itself — but ONLY if it is still OURS.
	// status.Namespace is a NAME; a name-collision path is refused at provision
	// (controller.go) before the anchor is ever written, but a future ordering
	// regression must never let teardown delete a namespace this workshop does
	// not own. Verify this workshop's OWN session labels (from ws.Spec.Session,
	// which teardown already holds — no session Get) before deleting. On a
	// mismatch, skip and log loudly but still release the finalizer below:
	// nothing of ours stands in a namespace that is not ours. On our own
	// namespace, everything still inside it (the deny-all NetworkPolicy, the
	// ResourceQuota, the namespace-scoped Role/RoleBinding) dies with it. No
	// wait: Kubernetes finishes namespace deletion asynchronously, and nothing
	// this reconciler still holds standing over depends on that finishing before
	// the finalizer releases.
	var ns corev1.Namespace
	switch err := r.Client.Get(ctx, types.NamespacedName{Name: nsName}, &ns); {
	case apierrors.IsNotFound(err):
		// Already gone — nothing to delete.
	case err != nil:
		return ctrl.Result{}, fmt.Errorf("teardown Workshop %s/%s: get workshop namespace %s: %w", ws.Namespace, ws.Name, nsName, err)
	case ns.Labels[spiceboxv1alpha1.LabelWorkshopSessionNamespace] != ws.Spec.Session.Namespace ||
		ns.Labels[spiceboxv1alpha1.LabelWorkshopSessionName] != ws.Spec.Session.Name:
		logger.Info("workshop teardown: namespace does not carry this workshop's session labels — refusing to delete a namespace this workshop does not own",
			"workshop", ws.Namespace+"/"+ws.Name, "namespace", nsName)
	default:
		if err := r.Client.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}}); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("teardown Workshop %s/%s: delete workshop namespace %s: %w", ws.Namespace, ws.Name, nsName, err)
		}
	}

	// 6. Every layer above is confirmed gone — release the finalizer.
	controllerutil.RemoveFinalizer(ws, spiceboxv1alpha1.FinalizerWorkshop)
	if err := r.Client.Update(ctx, ws); err != nil {
		return ctrl.Result{}, fmt.Errorf("teardown Workshop %s/%s: remove finalizer: %w", ws.Namespace, ws.Name, err)
	}
	return ctrl.Result{}, nil
}

// deleteLabeledToolCRs deletes every SpiceboxToolspec and SpiceboxToolkit
// carrying LabelWorkshopNamespace == nsName. Both kinds are cluster-scoped,
// so List is unscoped by namespace; NotFound on Delete is ignored (another
// actor, or a previous teardown attempt on retry, already removed it).
func (r *Reconciler) deleteLabeledToolCRs(ctx context.Context, nsName string) error {
	sel := client.MatchingLabels{spiceboxv1alpha1.LabelWorkshopNamespace: nsName}

	var specs spiceboxv1alpha1.SpiceboxToolspecList
	if err := r.Client.List(ctx, &specs, sel); err != nil {
		return fmt.Errorf("list SpiceboxToolspecs labeled %s=%s: %w", spiceboxv1alpha1.LabelWorkshopNamespace, nsName, err)
	}
	for i := range specs.Items {
		if err := r.Client.Delete(ctx, &specs.Items[i]); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete SpiceboxToolspec %s: %w", specs.Items[i].Name, err)
		}
	}

	var kits spiceboxv1alpha1.SpiceboxToolkitList
	if err := r.Client.List(ctx, &kits, sel); err != nil {
		return fmt.Errorf("list SpiceboxToolkits labeled %s=%s: %w", spiceboxv1alpha1.LabelWorkshopNamespace, nsName, err)
	}
	for i := range kits.Items {
		if err := r.Client.Delete(ctx, &kits.Items[i]); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete SpiceboxToolkit %s: %w", kits.Items[i].Name, err)
		}
	}
	return nil
}

// sweepExpiry checks whether a Ready workshop has outlived spec.limits.maxAge
// (measured from status.provisionedAt, the FIRST successful provision — see
// Reconcile step 9) and, if so, expires it: marks Phase=Expired with a
// condition naming the age, best-effort tells the person who started it, and
// Deletes the CR. Deleting only sets DeletionTimestamp — the finalizer is
// still held — so the NEXT reconcile runs teardown exactly as a manual
// delete would.
//
// handled=true means the caller returns (res, err) immediately, whether or
// not err is nil; handled=false means "not expired, fall through to
// provisioning."
func (r *Reconciler) sweepExpiry(ctx context.Context, ws *spiceboxv1alpha1.Workshop) (handled bool, res ctrl.Result, err error) {
	if ws.Status.Phase != spiceboxv1alpha1.WorkshopPhaseReady || ws.Status.ProvisionedAt == nil {
		return false, ctrl.Result{}, nil
	}
	deadline := ws.Status.ProvisionedAt.Add(ws.Spec.Limits.MaxAge.Duration)
	if !r.now().After(deadline) {
		return false, ctrl.Result{}, nil
	}

	age := r.now().Sub(ws.Status.ProvisionedAt.Time).Round(time.Second)
	msg := fmt.Sprintf("workshop exceeded its %s max age (provisioned %s ago)", ws.Spec.Limits.MaxAge.Duration, age)
	ws.Status.Phase = spiceboxv1alpha1.WorkshopPhaseExpired
	ws.Status.ObservedGeneration = ws.Generation
	conditions.SetFalse(ws, &ws.Status.Conditions, spiceboxv1alpha1.WorkshopConditionNamespaceReady, spiceboxv1alpha1.ReasonWorkshopExpired, msg)
	if err := r.Client.Status().Update(ctx, ws); err != nil {
		return true, ctrl.Result{}, fmt.Errorf("persist expired Workshop %s/%s status: %w", ws.Namespace, ws.Name, err)
	}

	if ws.Spec.StarterCanonical == "" {
		log.FromContext(ctx).Info("workshop sweeper: no starter canonical recorded on the workshop; the person will not be told",
			"workshop", ws.Namespace+"/"+ws.Name)
	} else if r.ExpiredNoticePublish == nil {
		log.FromContext(ctx).Info("workshop sweeper: no expiry notice publisher wired; the person will not be told",
			"workshop", ws.Namespace+"/"+ws.Name)
	} else if perr := r.ExpiredNoticePublish(ctx, ws.Spec.Session.Namespace, ws.Spec.Session.Name, "user:"+ws.Spec.StarterCanonical, expiredNoticeBody); perr != nil {
		log.FromContext(ctx).Info("workshop sweeper: expiry notice publish failed; the expiry stands",
			"workshop", ws.Namespace+"/"+ws.Name, "err", perr.Error())
	}

	if err := r.Client.Delete(ctx, ws); err != nil && !apierrors.IsNotFound(err) {
		return true, ctrl.Result{}, fmt.Errorf("delete expired Workshop %s/%s: %w", ws.Namespace, ws.Name, err)
	}
	return true, ctrl.Result{}, nil
}

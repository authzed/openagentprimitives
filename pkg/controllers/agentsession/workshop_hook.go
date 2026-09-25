// pkg/controllers/agentsession/workshop_hook.go
//
// The sidecar identity seam (spec §13): the ONLY path to a sidecar pod
// running under the workshop ServiceAccount is a Ready, session-owned
// Workshop CR's controller-owned status. workshopIdentityFor is the read
// side of that boundary. ensureWorkshop is the write side (spec §1.1): the
// gate that decides a session gets a Workshop CR at all.
package agentsession

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/settingswiring"
)

// workshopIdentityFor resolves the workshop sidecar identity for sess, or a
// nil identity when none applies.
//
// SECURITY: this reads Workshop.status ALONE — never AgentSession.status,
// which the runner holds patch on. A non-nil identity is returned only when
// ALL of the following hold:
//
//   - A Workshop named WorkshopName(sess.Name) exists in sess.Namespace.
//   - It carries a CONTROLLER owner reference whose UID equals sess.UID.
//     An unowned or foreign-owned Workshop — a planted CR — buys nothing.
//   - Status.Phase == WorkshopPhaseReady.
//   - Status.SidecarIdentity != nil.
//
// wsNamespace is Status.Namespace (W, the workshop's provisioned namespace,
// "ws-<uid12>") returned alongside identity/sidecarRef so a caller can stamp
// it onto the sidecar pod's env (WORKSHOP_NAMESPACE/WORKSHOP_ID) without a
// second Workshop read — it is only ever meaningful when identity is
// non-nil, and is "" on every nil-identity return below.
//
// A Get error other than NotFound is logged and treated the same as "no
// Workshop" — the pod simply boots identity-less this pass and the next
// reconcile retries. That is the fail-closed direction: a sidecar with no
// projected token can do nothing. This method has no error return by design,
// so a workshop lookup failure never blocks the rest of the sidecar loop.
func (r *Reconciler) workshopIdentityFor(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) (identity *spiceboxv1alpha1.WorkshopSidecarIdentity, sidecarRef string, wsNamespace string) {
	var ws spiceboxv1alpha1.Workshop
	key := client.ObjectKey{Namespace: sess.Namespace, Name: spiceboxv1alpha1.WorkshopName(sess.Name)}
	if err := r.Client.Get(ctx, key, &ws); err != nil {
		if !errors.IsNotFound(err) {
			log.FromContext(ctx).Info("workshopIdentityFor: get Workshop failed; sidecar boots identity-less this pass",
				"session", sess.Namespace+"/"+sess.Name,
				"workshop", key.Name,
				"err", err.Error())
		}
		return nil, "", ""
	}

	if !controllerOwnedBySession(&ws, sess) {
		// Unowned, owned by something else, or owned by a different session's
		// UID (a planted CR): buys nothing.
		return nil, "", ""
	}

	if ws.Status.Phase != spiceboxv1alpha1.WorkshopPhaseReady || ws.Status.SidecarIdentity == nil {
		return nil, "", ""
	}

	return ws.Status.SidecarIdentity, ws.Spec.SidecarToolbox, ws.Status.Namespace
}

// controllerOwnedBySession reports whether obj carries a controller owner
// reference to sess specifically — an AgentSession-kind owner whose UID
// matches. Unowned, owned by something else, or owned by a different
// session's UID (a planted or leftover CR) all report false. Shared by
// workshopIdentityFor's read side and ensureWorkshop's write side so the two
// halves of the ownership check can never drift apart.
func controllerOwnedBySession(obj metav1.Object, sess *spiceboxv1alpha1.AgentSession) bool {
	owner := metav1.GetControllerOf(obj)
	return owner != nil && owner.Kind == "AgentSession" && owner.UID == sess.UID
}

// defaultWorkshopLimits are the resource ceilings copied onto a newly-created
// Workshop's spec.limits. Immutable after creation (CEL on WorkshopSpec): the
// sidecar SA must not be able to raise its own bounds by rewriting them.
func defaultWorkshopLimits() spiceboxv1alpha1.WorkshopLimits {
	return spiceboxv1alpha1.WorkshopLimits{
		MaxAge:              metav1.Duration{Duration: 168 * time.Hour},
		MaxObjectsPerKind:   20,
		MaxObjects:          100,
		MaxConcurrentProbes: 2,
	}
}

// limitsOf returns spec.Limits, nil-safe for a nil SettingsSpec (the tier's
// CR does not exist). Mirrors BuilderClassFor's own nil-receiver reading of
// "no sanction" — a missing tier imposes no ceiling and grants nothing.
func limitsOf(spec *spiceboxv1alpha1.SettingsSpec) *spiceboxv1alpha1.SettingsLimits {
	if spec == nil {
		return nil
	}
	return spec.Limits
}

// ensureWorkshop is the sanction hook (spec §1.1): the ONLY path to a
// Workshop CR for sess. Called immediately after EnforceStartGate — a
// session whose starter never passed the start gate must not cost a
// namespace — and on every reconcile after, because unlike the start gate's
// decided-once verdict, a workshop's sanction is re-checked continuously: a
// cluster admin revoking BuilderClasses (or a class dropping its sidecar
// ref) must tear the live Workshop down on the very next pass, not just
// refuse a future one.
//
// SECURITY: no sanction ⇒ no Workshop CR, ever. Two independent facts must
// both hold before one is created: ClusterAgentSettings — the CLUSTER tier
// ONLY, never the namespace tier, so a tenant cannot self-sanction — names
// this exact (namespace, class), AND the class's own spec actually
// references the named SidecarToolbox. Either one failing deletes an
// existing owned Workshop rather than merely refusing to create one, which
// is what makes a revocation self-enforcing.
//
// Same (res, halted, err) contract as EnforceStartGate: the caller returns
// immediately whenever halted || err != nil.
func (r *Reconciler) ensureWorkshop(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, ac *spiceboxv1alpha1.AgentClass) (ctrl.Result, bool, error) {
	cluster, _, err := settingswiring.FetchTiers(ctx, r.Client, sess.Namespace)
	if err != nil {
		return ctrl.Result{}, true, fmt.Errorf("ensureWorkshop: fetch settings tiers: %w", err)
	}

	sanction := limitsOf(cluster).BuilderClassFor(ac.Namespace, ac.Name)
	// The cluster sanction alone is not enough (spec §1.1): the class must
	// also actually wire that sidecar in, or "which sidecar receives the
	// workshop identity" is a name the admin typed with nothing behind it.
	// The browser start route asks the same question of the same method, so
	// the two gates cannot disagree about which classes make a workshop.
	if sanction == nil || !ac.ReferencesSidecarToolbox(sanction.SidecarToolbox) {
		why := "the class is not sanctioned for the agent-builder workshop"
		if sanction != nil {
			why = "the class no longer references its sanctioned sidecar " + sanction.SidecarToolbox
		}
		if derr := r.deleteOwnedWorkshopIfPresent(ctx, sess, why); derr != nil {
			return ctrl.Result{}, true, derr
		}
		return ctrl.Result{}, false, nil
	}

	// A session that has finished never gets a workshop, sanctioned or not.
	// The Workshop controller RELEASES the workshop of a terminal session (it
	// has stopped counting against the person's cap), and this function is the
	// only thing that could put one back: with the failed-pod reap grace
	// disabled a Failed session skips the reap short-circuit and reconciles
	// straight through to here, finds no Workshop, creates one, and release
	// deletes it again — forever, with nothing but namespace churn to show for
	// it. The guard belongs here rather than at the caller because this is the
	// ONLY path to a Workshop CR.
	//
	// Not halted: a finished session simply gets nothing, and the rest of its
	// reconcile (reaping, status, cleanup) still has work to do.
	if isTerminalPhase(sess.Status.Phase) {
		log.FromContext(ctx).Info("ensureWorkshop: finished session; not provisioning a workshop",
			"session", sess.Namespace+"/"+sess.Name, "phase", sess.Status.Phase)
		return ctrl.Result{}, false, nil
	}

	canonical := spiceboxv1alpha1.StartedByCanonical(sess)

	exceeded, cerr := r.workshopCapExceeded(ctx, sess, cluster, canonical.String())
	if cerr != nil {
		return ctrl.Result{}, true, cerr
	}
	if exceeded {
		if !canonical.IsZero() {
			if r.StartRefusedNoticePublish == nil {
				log.FromContext(ctx).Info("ensureWorkshop: no notice publisher wired; the person will not be told",
					"session", sess.Namespace+"/"+sess.Name)
			} else if nerr := r.StartRefusedNoticePublish(ctx, sess.Namespace, sess.Name, "user:"+canonical.String(), spiceboxv1alpha1.WorkshopLimitBody); nerr != nil {
				log.FromContext(ctx).Info("ensureWorkshop: limit notice publish failed; the refusal stands",
					"session", sess.Namespace+"/"+sess.Name, "err", nerr.Error())
			}
		}
		res, merr := r.markBootFailed(ctx, sess, spiceboxv1alpha1.ReasonAgentSessionWorkshopLimitExceeded, spiceboxv1alpha1.WorkshopLimitBody)
		return res, true, merr
	}

	ws := &spiceboxv1alpha1.Workshop{
		ObjectMeta: metav1.ObjectMeta{
			Name:            spiceboxv1alpha1.WorkshopName(sess.Name),
			Namespace:       sess.Namespace,
			OwnerReferences: sessionOwnerRef(sess),
		},
		Spec: spiceboxv1alpha1.WorkshopSpec{
			Session:          spiceboxv1alpha1.NamespacedRef{Namespace: sess.Namespace, Name: sess.Name},
			StarterCanonical: canonical.String(),
			SidecarToolbox:   sanction.SidecarToolbox,
			Limits:           defaultWorkshopLimits(),
		},
	}
	if cerr := r.Client.Create(ctx, ws); cerr != nil {
		if !errors.IsAlreadyExists(cerr) {
			return ctrl.Result{}, true, fmt.Errorf("ensureWorkshop: create Workshop %s/%s: %w", ws.Namespace, ws.Name, cerr)
		}
		// AlreadyExists: verify it is genuinely ours (an earlier pass of this
		// same reconcile loop that raced the write) before treating it as
		// idempotent. A same-named Workshop owned by a DIFFERENT session's UID
		// is a prior session's leftover — a name collision, never adopted.
		var existing spiceboxv1alpha1.Workshop
		key := client.ObjectKey{Namespace: ws.Namespace, Name: ws.Name}
		if gerr := r.Client.Get(ctx, key, &existing); gerr != nil {
			return ctrl.Result{}, true, fmt.Errorf("ensureWorkshop: get existing Workshop %s/%s after AlreadyExists: %w", key.Namespace, key.Name, gerr)
		}
		if !controllerOwnedBySession(&existing, sess) {
			return ctrl.Result{}, true, fmt.Errorf("ensureWorkshop: Workshop %s/%s already exists and is not owned by this session (a prior session's leftover; refusing to adopt)", key.Namespace, key.Name)
		}
	}
	// The Workshop provisions asynchronously — namespace, RBAC, the SpiceDB
	// tuple, the bearer Secret, then the minted sidecar identity. When the
	// session's workshop sidecar runs SEPARATE-POD, that pod needs the minted
	// identity to boot (it is a fail-closed image), and BuildSidecarPod's
	// identity branch only supplies it once the Workshop is Ready — so nothing
	// downstream may be created until then, or the sidecar is built
	// identity-less and crashes (the live race on oap-desktop 2026-09-11). Halt
	// and requeue until Ready; workshopIdentityFor then returns the identity
	// and the sidecar is created ONCE, with its token.
	//
	// Gated on the workshop toolbox's RUN MODE, not merely on the Workshop
	// existing: an IN-POD workshop sidecar (the e2e harness mounts one
	// in-process, and any toolbox declaring neither secretInputs nor
	// isolation) never receives a minted identity and has nothing to wait for
	// — halting it would wedge a session forever wherever no controller drives
	// the Workshop to Ready. The toolbox name is the one this reconcile wrote
	// into ws.Spec from the sanction, never a re-read of the Workshop: the
	// manager's informer cache can trail a Create by a watch event, and a
	// re-read that answered NotFound was once taken as "nothing to wait for"
	// — the reconcile fell through and built the sidecar identity-less (the
	// live race on oap-desktop 2026-09-13; pinned by
	// TestEnsureWorkshop_HaltsWhenCacheLagsBehindCreate). The AgentClass-
	// validity gate already ran, so the referenced toolbox exists; a toolbox
	// NotFound is a transient race, and the safe direction is to NOT halt (a
	// genuinely separate-pod sidecar whose toolbox vanished fails later at pod
	// build, surfaced there).
	needsIdentity, nerr := r.workshopSidecarNeedsIdentity(ctx, sess.Namespace, ws.Spec.SidecarToolbox)
	if nerr != nil {
		return ctrl.Result{}, true, fmt.Errorf("ensureWorkshop: resolve workshop sidecar run mode: %w", nerr)
	}
	if !needsIdentity {
		return ctrl.Result{}, false, nil
	}
	var provisioned spiceboxv1alpha1.Workshop
	wsKey := client.ObjectKey{Namespace: sess.Namespace, Name: spiceboxv1alpha1.WorkshopName(sess.Name)}
	if gerr := r.Client.Get(ctx, wsKey, &provisioned); gerr != nil {
		if errors.IsNotFound(gerr) {
			// A Workshop this reconcile just created, or just confirmed as its
			// own, cannot be absent: the cache has not caught up with the
			// write. Same answer as "not Ready yet" — hold, and come back.
			log.FromContext(ctx).Info("ensureWorkshop: Workshop not visible yet (cache trailing the create); holding the session until it is Ready",
				"session", sess.Namespace+"/"+sess.Name, "workshop", wsKey.Name)
			return ctrl.Result{RequeueAfter: 2 * time.Second}, true, nil
		}
		return ctrl.Result{}, true, fmt.Errorf("ensureWorkshop: re-get Workshop %s to gate on readiness: %w", wsKey, gerr)
	}
	if provisioned.Status.Phase != spiceboxv1alpha1.WorkshopPhaseReady {
		log.FromContext(ctx).Info("ensureWorkshop: Workshop not Ready yet; holding the session until its sidecar identity is minted",
			"session", sess.Namespace+"/"+sess.Name, "workshop", wsKey.Name, "phase", provisioned.Status.Phase)
		return ctrl.Result{RequeueAfter: 2 * time.Second}, true, nil
	}
	return ctrl.Result{}, false, nil
}

// workshopSidecarNeedsIdentity reports whether the workshop sidecar named
// sidecarToolbox (a SidecarToolbox in ns) runs separate-pod — the only mode
// that receives a minted WorkshopSidecar identity and so the only mode
// ensureWorkshop must wait for. It reads that toolbox's RunModeFor. The name
// is handed in by the caller, which holds it from the sanction it just wrote
// into the Workshop's spec; re-reading the Workshop here would put the
// informer cache's lag on the gate. An empty name or a missing toolbox
// reports false (do not halt): the class-validity gate guarantees existence
// on the live path, so absence here is a transient race, and a separate-pod
// sidecar whose toolbox truly vanished fails at pod build.
func (r *Reconciler) workshopSidecarNeedsIdentity(ctx context.Context, ns, sidecarToolbox string) (bool, error) {
	if sidecarToolbox == "" {
		return false, nil
	}
	var tb spiceboxv1alpha1.SidecarToolbox
	tbKey := client.ObjectKey{Namespace: ns, Name: sidecarToolbox}
	if err := r.Client.Get(ctx, tbKey, &tb); err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("get SidecarToolbox %s: %w", tbKey, err)
	}
	return RunModeFor(tb.Spec) == RunModeSeparatePod, nil
}

// deleteOwnedWorkshopIfPresent deletes sess's Workshop CR when it exists and
// is genuinely controller-owned by sess. why is logged, not surfaced to a
// person — this runs on both "never sanctioned" and "sanction revoked", and
// neither is a refusal a starter needs telling about (they simply never get
// a workshop, or lose one they should not have kept).
func (r *Reconciler) deleteOwnedWorkshopIfPresent(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, why string) error {
	var ws spiceboxv1alpha1.Workshop
	key := client.ObjectKey{Namespace: sess.Namespace, Name: spiceboxv1alpha1.WorkshopName(sess.Name)}
	if err := r.Client.Get(ctx, key, &ws); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("ensureWorkshop: get Workshop %s/%s: %w", key.Namespace, key.Name, err)
	}
	if !controllerOwnedBySession(&ws, sess) {
		// Not ours: never touch someone else's CR or a planted one.
		return nil
	}
	log.FromContext(ctx).Info("ensureWorkshop: deleting workshop",
		"session", sess.Namespace+"/"+sess.Name, "workshop", key.Name, "reason", why)
	if err := r.Client.Delete(ctx, &ws); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("ensureWorkshop: delete Workshop %s/%s: %w", key.Namespace, key.Name, err)
	}
	return nil
}

// workshopCapExceeded counts LIVE Workshops (no deletionTimestamp) across
// every namespace whose spec.starterCanonical matches canonical, excluding
// sess's own workshop (it must never count against itself), and compares
// against the cluster's MaxWorkshopsPerStarter ceiling (default 3).
func (r *Reconciler) workshopCapExceeded(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, cluster *spiceboxv1alpha1.SettingsSpec, canonical string) (bool, error) {
	var list spiceboxv1alpha1.WorkshopList
	if err := r.Client.List(ctx, &list); err != nil {
		return false, fmt.Errorf("ensureWorkshop: list workshops: %w", err)
	}
	maxWorkshops := limitsOf(cluster).MaxWorkshopsPerStarterOrDefault()
	ownName := spiceboxv1alpha1.WorkshopName(sess.Name)
	var count int32
	for i := range list.Items {
		w := &list.Items[i]
		if w.Spec.StarterCanonical != canonical || w.DeletionTimestamp != nil {
			continue
		}
		if w.Namespace == sess.Namespace && w.Name == ownName {
			continue // this session's own workshop never counts against itself
		}
		count++
	}
	return count >= maxWorkshops, nil
}

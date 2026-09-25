// Package spiceboxsession implements the SpiceboxSession controller.
//
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=spiceboxsessions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=spiceboxsessions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=spiceboxsessions/finalizers,verbs=update
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=spiceboxtoolchains,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// The agent-sandbox (sigs.k8s.io/agent-sandbox) backend runs a peer CRD's
// Sandbox object. Its NewRuntime availability check is RESTMapper *discovery*,
// which needs no RBAC on the resource — so on a cluster where the CRD is
// installed the runtime registers its Owns(&sandboxv1beta1.Sandbox{}) informer
// regardless of whether any class opted in. Without these rules that informer's
// list/watch is Forbidden, the cache never syncs, mgr.Start returns, and the
// operator crash-loops — taking the built-in pod backend down with it.
// +kubebuilder:rbac:groups=agents.x-k8s.io,resources=sandboxes,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=agents.x-k8s.io,resources=sandboxes/status,verbs=get
// A DIFFERENT API group, shipping as a separate CRD bundle: the pre-warming
// types. The agent-sandbox backend adopts a warm sandbox by creating a
// SandboxClaim from THIS controller's Ensure, and tears one down from its
// finalizer, so the grants belong here and not only on the class controller
// that builds the pool. list;watch, not just get: the operator's client reads
// through the cache, and the first Get starts an informer.
// +kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxclaims,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxwarmpools,verbs=get;list;watch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
package spiceboxsession

import (
	"context"
	stderrors "errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	apreconcile "github.com/authzed/openagentprimitives/pkg/controllers/internal/reconcile"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/podspec"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds/pod"
)

type Reconciler struct {
	Client client.Client
	Scheme *apiruntime.Scheme

	// SandboxImage is the default sandbox/bundle pod image used when a
	// SpiceboxClass does not pin one (spec.image empty). The operator sets it from
	// its --sandbox-image flag, which `oap install` registry-qualifies for the
	// cluster — so classes (and identity-setup bundles) need not hardcode a
	// registry-specific, non-portable image. Empty only in tests that build pods
	// from classes that always pin an image.
	SandboxImage string

	// AuditMemory publishes the signed `resolved` toolchain-audit entry. It MUST
	// be the operator's provenance-signing facade. Declared as the interface (not
	// a concrete pointer) so an unset value is a genuine nil interface rather
	// than a typed-nil that panics on first call — see AGENTS.md.
	AuditMemory memory.Memory

	// Runtimes holds one constructed sandbox Runtime per registered kind, built
	// once at startup. Declared as the map type so an unset value is an empty
	// map that misses fail-closed, never a typed-nil that panics.
	Runtimes sandboxkinds.Runtimes
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&spiceboxv1alpha1.SpiceboxSession{})

	// Each sandbox kind contributes the owned-object types this controller
	// must watch to observe its backend's state changes (the pod kind
	// contributes &corev1.Pod{}). De-duplicated by concrete type so two kinds
	// naming the same object type register one Owns call, not two.
	seenWatchTypes := make(map[reflect.Type]bool)
	for _, rt := range r.Runtimes {
		for _, w := range rt.Watches() {
			t := reflect.TypeOf(w.Object)
			if seenWatchTypes[t] {
				continue
			}
			seenWatchTypes[t] = true
			b = b.Owns(w.Object)
		}
	}

	if err := b.
		Watches(
			&spiceboxv1alpha1.SpiceboxClass{},
			handler.EnqueueRequestsFromMapFunc(r.mapClassToSessions),
		).
		Watches(
			&spiceboxv1alpha1.SpiceboxToolspec{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
				var list spiceboxv1alpha1.SpiceboxSessionList
				if err := r.Client.List(ctx, &list); err != nil {
					log.FromContext(ctx).Info("list SpiceboxSessions for SpiceboxToolspec watch failed; dropping re-enqueue (self-heals on next resync)",
						"spiceboxtoolspec", o.GetName(), "namespace", o.GetNamespace(), "err", err.Error())
					return nil
				}
				out := make([]reconcile.Request, 0, len(list.Items))
				for i := range list.Items {
					out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
				}
				return out
			}),
		).
		// A SpiceboxToolchain flipping Valid=True (or appearing after a session
		// already bound and got fail-closed) does not touch the SpiceboxClass that
		// names it, so mapClassToSessions never fires for it; and with no pod yet
		// created, Owns(&corev1.Pod{}) can't fire either. Without this watch, a
		// session that binds before its toolchain resolves cleanly (apply-ordering
		// race, an admin adding a toolchain a class already references, or a
		// transient toolchain-Get error) stalls Ready=False until the manager's
		// ~10h default resync.
		Watches(
			&spiceboxv1alpha1.SpiceboxToolchain{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
				var list spiceboxv1alpha1.SpiceboxSessionList
				if err := r.Client.List(ctx, &list); err != nil {
					log.FromContext(ctx).Info("list SpiceboxSessions for SpiceboxToolchain watch failed; dropping re-enqueue (self-heals on next resync)",
						"spiceboxtoolchain", o.GetName(), "namespace", o.GetNamespace(), "err", err.Error())
					return nil
				}
				out := make([]reconcile.Request, 0, len(list.Items))
				for i := range list.Items {
					// Invariant: a session's toolchains are frozen at first bind. Once
					// Status.ResolvedClass != nil, resolveToolchains never runs again
					// for that session — the pod builder reads only the frozen
					// Status.ResolvedToolchains snapshot, and a catalog edit can never
					// change a running pod. So a toolchain event provably cannot affect
					// an already-bound session; enqueueing it is pure waste that widens
					// races in other reconciles. Only unbound sessions — exactly the
					// stuck-on-toolchain-resolution case this watch exists to rescue —
					// get enqueued. Do not "helpfully" widen this back to all sessions.
					if list.Items[i].Status.ResolvedClass != nil {
						continue
					}
					out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
				}
				return out
			}),
		).
		Complete(r); err != nil {
		return err
	}
	return mgr.Add(manager.RunnableFunc(r.runTTLSweep))
}

// mapClassToSessions returns reconcile requests for all Sessions referencing the given Class.
func (r *Reconciler) mapClassToSessions(ctx context.Context, obj client.Object) []ctrl.Request {
	class, ok := obj.(*spiceboxv1alpha1.SpiceboxClass)
	if !ok {
		return nil
	}
	var list spiceboxv1alpha1.SpiceboxSessionList
	if err := r.Client.List(ctx, &list); err != nil {
		log.FromContext(ctx).Info("list SpiceboxSessions for SpiceboxClass watch failed; dropping re-enqueue (self-heals on next resync)",
			"spiceboxclass", class.Name, "namespace", class.Namespace, "err", err.Error())
		return nil
	}
	var reqs []ctrl.Request
	for _, s := range list.Items {
		if s.Spec.Class == class.Name {
			reqs = append(reqs, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&s)})
		}
	}
	return reqs
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var sess spiceboxv1alpha1.SpiceboxSession
	if cont, err := apreconcile.LoadInto(ctx, r.Client, req.NamespacedName, &sess); !cont {
		return ctrl.Result{}, err
	}

	if !sess.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&sess, spiceboxv1alpha1.FinalizerSpiceboxSession) {
			return ctrl.Result{}, nil
		}

		// Tear down the sandbox through the seam (owner-ref GC would eventually
		// reclaim a pod-shaped backend, but we prefer explicit, and a non-pod
		// backend has no owner-ref GC to fall back on at all). A nil
		// status.sandbox means the session never bound to a backend, so there
		// is nothing to tear down.
		if sess.Status.Sandbox != nil {
			h := sandboxkinds.HandleFromStatus(sess.Status.Sandbox)
			if rt, ok := r.Runtimes.For(h.Kind); ok {
				if err := rt.Teardown(ctx, h); err != nil {
					return ctrl.Result{}, fmt.Errorf("teardown sandbox (kind %s): %w", h.Kind, err)
				}
			} else {
				// Deletion must NOT fail closed the way kind resolution on create
				// does: refusing to finalize here would make the session
				// permanently undeletable (a wedged object needing manual
				// finalizer surgery) rather than an orphaned resource an operator
				// can find and clean up by hand. Proceed to finalizer removal, but
				// this path must never be silent about what it left behind.
				logger.Info("sandbox kind has no registered runtime; finalizing without teardown, resource may be orphaned",
					"session", sess.Name, "namespace", sess.Namespace, "kind", h.Kind, "ref", h.Ref)
			}
		}

		// ArtifactStore cleanup is handled in Plan 2 (the store isn't yet used here).

		controllerutil.RemoveFinalizer(&sess, spiceboxv1alpha1.FinalizerSpiceboxSession)
		if err := r.Client.Update(ctx, &sess); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Ensure finalizer. Used by the deletion path added in Task 5.4.
	if added, err := apreconcile.EnsureFinalizer(ctx, r.Client, &sess, spiceboxv1alpha1.FinalizerSpiceboxSession); added || err != nil {
		return ctrl.Result{Requeue: added}, err
	}

	// Resolve the class.
	class, err := r.resolveClass(ctx, sess.Spec.Class)
	if err != nil {
		r.setClassMissing(&sess, err)
		return ctrl.Result{}, r.Client.Status().Update(ctx, &sess)
	}

	// boundThisPass tracks whether THIS reconcile pass is the one that computed a
	// fresh class+toolchain resolution (sess.Status.ResolvedClass was nil on
	// entry and got populated below) — true even for a class naming zero
	// toolchains. Combined with frozeThisPass below, it gates the persist-then-
	// requeue immediately after the freeze block: that extra round-trip exists
	// only to make a toolchain resolution durable and attested before a pod can
	// see it, so boundThisPass alone (with no toolchains involved) must not
	// trigger it — see frozeThisPass and the gate below.
	boundThisPass := false

	// frozeThisPass tracks whether an audit entry is owed: true only when THIS
	// pass froze a NON-EMPTY toolchain set, so the audit entry publishes exactly
	// once — at the first successful status write that makes the freeze durable —
	// and never again on a later status write in the same pass, or on a later
	// reconcile of an already-frozen session. A class naming zero toolchains
	// never gets an audit entry (unchanged, intentional), and — per the gate
	// below — never pays the extra persist+requeue either: with no resolution to
	// persist and no entry to attest, there is nothing that could reach a pod
	// prematurely.
	frozeThisPass := false

	// persistStatus is the chokepoint every status write below goes through.
	// Ordering matters: the signed toolchain-audit "resolved" entry must be
	// written only AFTER the freeze is durably persisted, so a status write that
	// fails right after a successful freeze can't leave a signed entry claiming a
	// pod ran with toolchains it never received. Whichever exit path's status
	// update succeeds first is that moment.
	persistStatus := func() error {
		err := r.Client.Status().Update(ctx, &sess)
		if err == nil && frozeThisPass {
			frozeThisPass = false
			r.recordResolved(ctx, &sess)
		}
		return err
	}

	// Snapshot the resolved class on first bind (frozen for session lifetime).
	if sess.Status.ResolvedClass == nil {
		rc := class.Spec.DeepCopy()
		// Default the sandbox image to the operator's configured default when the
		// class does not pin one, and freeze it into the snapshot. This is the one
		// chokepoint both bundle and agent sandbox pods flow through, so a class
		// with no image becomes portable (the default is registry-qualified at
		// install) instead of an unpullable bare ref. If neither is set,
		// podspec.Build fails closed below with a clear class.image error.
		if rc.Image == "" {
			rc.Image = r.SandboxImage
		}

		// Resolve the class's toolchains and freeze them next to the class. The
		// pod builder reads only this snapshot, so a catalog edit mid-session can
		// never change a running pod.
		mounts, setDigest, tcErr := resolveToolchains(ctx, r.Client, rc.Toolchains)
		if tcErr != nil {
			var reason string
			switch {
			case stderrors.Is(tcErr, errToolchainMissing):
				reason = spiceboxv1alpha1.ReasonToolchainMissing
			case stderrors.Is(tcErr, errToolchainNotValid):
				reason = spiceboxv1alpha1.ReasonToolchainNotValid
			default:
				// Not a genuine missing/invalid toolchain — a transient apiserver
				// Get error, or a resolve-time race (e.g. ToMount failing after the
				// Valid=True check TOCTOU'd). Persisting a Ready=False condition here
				// would be misleading (the toolchain may well be fine), and the
				// SpiceboxToolchain watch above cannot help since nothing about the
				// toolchain itself changed. Return the error so controller-runtime
				// requeues with backoff instead.
				return ctrl.Result{}, tcErr
			}
			conditions.SetFalse(&sess, &sess.Status.Conditions,
				spiceboxv1alpha1.SpiceboxSessionConditionReady, reason, tcErr.Error())
			return ctrl.Result{}, r.Client.Status().Update(ctx, &sess)
		}

		// The session-level override wins over the class's own setting: the
		// AgentSession reconciler stamps the tier-resolved decision onto
		// spec.sandbox, and the frozen snapshot must record the backend this
		// session will actually use.
		*rc = applySandboxOverride(*rc, sess.Spec.Sandbox)

		// Re-run the class-vs-backend validation chain against the kind that will
		// ACTUALLY run. The spiceboxclass controller validated this class against
		// the kind the class DECLARED; the override above can replace that kind
		// wholesale, and nothing between here and rt.Ensure re-asks — the runtime
		// lookup below only proves the kind is registered, not that it can run
		// this class. This is the one point where the final (kind, classSpec)
		// pair is known.
		//
		// Fail closed: an override to a backend that cannot run the class is an
		// operator misconfiguration, not a transient condition, so it lands as
		// Failed=ClassInvalid rather than a retry loop.
		if err := validateResolvedSandbox(*rc, sess.Spec.Mounts, len(sess.Spec.SkillBundles) > 0); err != nil {
			r.setClassInvalid(&sess, err)
			return ctrl.Result{}, r.Client.Status().Update(ctx, &sess)
		}

		sess.Status.ResolvedClass = rc
		sess.Status.ResolvedToolchains = mounts
		sess.Status.ToolchainSetDigest = setDigest
		boundThisPass = true
		frozeThisPass = len(mounts) > 0
	}

	// A selection that reaches a pod without a signed `resolved` entry is a bug:
	// persist and attest the freeze BEFORE anything below can create a Pod. That
	// only applies when frozeThisPass is true — this pass froze a NON-EMPTY
	// toolchain set, so there is a resolution that must become durable and an
	// audit entry that must be published before any pod can see it. A
	// zero-toolchain session (boundThisPass true, frozeThisPass false) has
	// neither: nothing was frozen that a pod could observe prematurely, and
	// recordResolved never runs for it either way (see frozeThisPass above), so
	// gating the requeue on boundThisPass alone would buy zero attestation at
	// the cost of a whole extra reconcile round-trip — and, with it, a delayed
	// pod. It falls through to pod creation in this same pass instead, exactly
	// as it did before this feature existed.
	//
	// A session WITH toolchains still persists the freeze and publishes the
	// signed `resolved` audit entry here, strictly before any pod exists: if we
	// fell through to pod creation in this same pass, a status write that fails
	// after Create (plausible — Owns(&corev1.Pod{}) means the Pod we just created
	// enqueues a concurrent reconcile, and a resourceVersion conflict is real)
	// would leave a running Pod whose toolchains were never durably recorded.
	// Requeuing here instead means the next pass builds the Pod from a resolution
	// that is already persisted and already attested.
	if boundThisPass && frozeThisPass {
		if err := persistStatus(); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Snapshot the resolved agent on first bind (frozen for session lifetime).
	if sess.Status.ResolvedAgent == "" {
		sess.Status.ResolvedAgent = sess.Spec.Agent
	}

	// Clear any prior Failed=ClassMissing/ClassInvalid now that the class resolves.
	for i, c := range sess.Status.Conditions {
		if c.Type == spiceboxv1alpha1.SpiceboxSessionConditionFailed &&
			c.Status == metav1.ConditionTrue &&
			(c.Reason == spiceboxv1alpha1.ReasonClassMissing || c.Reason == spiceboxv1alpha1.ReasonClassInvalid) {
			sess.Status.Conditions[i].Status = metav1.ConditionFalse
			sess.Status.Conditions[i].Reason = "ClassAppeared"
			sess.Status.Conditions[i].Message = "class resolved after prior ClassMissing/Invalid"
			sess.Status.Conditions[i].LastTransitionTime = metav1.Now()
		}
	}

	// Compute the effective toolspec set; reject sessions that reference
	// toolspecs not present in the class.
	effective, notInClass := computeEffectiveToolspecs(&sess, class)
	sess.Status.EffectiveToolspecs = effective
	if len(notInClass) > 0 {
		conditions.SetFalse(&sess, &sess.Status.Conditions,
			spiceboxv1alpha1.SpiceboxSessionConditionReady,
			spiceboxv1alpha1.ReasonToolspecNotInClass,
			fmt.Sprintf("toolspecs not in class: %s", strings.Join(notInClass, ", ")))
		return ctrl.Result{}, persistStatus()
	}

	// Ensure the sandbox exists via the resolved backend's kind seam. The
	// class's frozen snapshot decides which backend a session uses for its
	// whole lifetime, so a class edit mid-session can never move a running
	// sandbox to a different kind.
	kind := sess.Status.ResolvedClass.Sandbox.ResolvedKind()
	rt, ok := r.Runtimes.For(kind)
	if !ok {
		r.setClassInvalid(&sess, fmt.Errorf("sandbox kind %q has no registered runtime", kind))
		return ctrl.Result{}, persistStatus()
	}

	h, err := rt.Ensure(ctx, sandboxkinds.EnsureRequest{
		Session: &sess,
		Class:   *sess.Status.ResolvedClass,
		Config:  sess.Status.ResolvedClass.Sandbox.Config,
	})
	if err != nil {
		// A backend that cannot proceed yet because a prerequisite it does not
		// own (the shared workspace PVC, created by the parent AgentSession) is
		// still being created is not a failure: report it as Progressing and
		// requeue rather than failing the session outright.
		if stderrors.Is(err, sandboxkinds.ErrPreconditionPending) {
			r.setProgressing(&sess, spiceboxv1alpha1.ReasonCreating, err.Error())
			if uerr := persistStatus(); uerr != nil {
				return ctrl.Result{}, uerr
			}
			return ctrl.Result{RequeueAfter: pvcBindPollInterval}, nil
		}
		return ctrl.Result{}, fmt.Errorf("ensure sandbox (kind %s): %w", kind, err)
	}

	// firstBind is the pass on which Ensure hands back a handle for the first
	// time (status.sandbox was unset on entry). Report Progressing here,
	// mirroring the pre-seam "Pod just created" branch, rather than reading
	// Status immediately — a sandbox this fresh can only say Pending, and the
	// owned-object watch drives the next reconcile once it changes.
	firstBind := sess.Status.Sandbox == nil
	// The whole handle, through the seam's own conversion: every field the
	// backend needs to resolve this sandbox again after an operator restart
	// (including Prewarmed, which selects the resolution path) is persisted
	// here, and none is recomputed later. It is an observation written with the
	// handle that produced it.
	sess.Status.Sandbox = h.ToStatus()
	if h.Kind == pod.KindName {
		// PodName is kept populated for the built-in backend even though
		// status.sandbox is now the source of truth, because consumers fall back
		// to it inconsistently: `oap sandbox show`/`list`
		// (cmd/oap/internal/sandboxcmd/show.go, list.go) read status.sandbox first and
		// fall back to PodName for a pre-seam session that predates it. The admin
		// UI (pkg/web/admind/handlers.go's bundleSandboxes) has NO such fallback — it
		// reads only status.sandbox, so a pre-seam session renders blank sandbox
		// fields there rather than a PodName-derived value.
		sess.Status.PodName = podspec.PodNameFor(&sess)
	}
	if firstBind {
		r.setProgressing(&sess, spiceboxv1alpha1.ReasonCreating, "sandbox created; waiting for readiness")
		return ctrl.Result{}, persistStatus()
	}

	st, err := rt.Status(ctx, h)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("get sandbox status (kind %s): %w", kind, err)
	}
	if st.Phase != sandboxkinds.PhaseFailed {
		clearStaleSandboxFailure(&sess)
	}
	applySandboxStatus(&sess, st)
	sess.Status.ObservedGeneration = sess.Generation

	if err := persistStatus(); err != nil {
		return ctrl.Result{}, err
	}
	logger.V(1).Info("reconciled")

	// Every state a backend reports needs something that ends it, and a watch is
	// not always that something. The owned-object watches registered above are
	// all Owns(...) — controller-owner mapping only — so they cannot see an
	// ADOPTED agent-sandbox Sandbox (controller-owned by its SandboxClaim) or
	// the POD under any Sandbox (controller-owned by that Sandbox). A backend
	// waiting on one of those would otherwise get one free pass, from this
	// status write's own self-trigger, and then stall until the manager's ~10h
	// resync.
	//
	// ONLY the backend can know that, which is why it is asked rather than
	// inferred: Status.RequiresPolling means "no Watches() entry of mine will
	// end this state." Polling on Phase == Pending instead would be a
	// cluster-wide steady-state load increase, not a backstop — with the pod
	// backend, EVERY session is Pending while its pod starts, and every one of
	// them would re-reconcile on a timer for the whole of startup. That
	// regression was caught by the e2e suite, which is why this reads the
	// backend's own answer and nothing else.
	//
	// A bounded requeue rather than a Pod watch on this side: a watch needs a
	// mapper and touches the shared per-backend Owns registration above, where
	// a mis-mapped object is a silent bug affecting every kind; this is local
	// and cannot mis-map. The INTERVAL stays here, not on the seam, so
	// cluster-wide reconcile load has exactly one place to tune.
	if st.RequiresPolling {
		return ctrl.Result{RequeueAfter: sandboxPendingPollInterval}, nil
	}
	return ctrl.Result{}, nil
}

func (r *Reconciler) resolveClass(ctx context.Context, name string) (*spiceboxv1alpha1.SpiceboxClass, error) {
	var class spiceboxv1alpha1.SpiceboxClass
	if err := r.Client.Get(ctx, types.NamespacedName{Name: name}, &class); err != nil {
		return nil, err
	}
	return &class, nil
}

func (r *Reconciler) setProgressing(sess *spiceboxv1alpha1.SpiceboxSession, reason, msg string) {
	conditions.Set(sess, &sess.Status.Conditions, metav1.Condition{
		Type: spiceboxv1alpha1.SpiceboxSessionConditionProgressing, Status: metav1.ConditionTrue,
		Reason: reason, Message: msg,
	})
}

func (r *Reconciler) setClassMissing(sess *spiceboxv1alpha1.SpiceboxSession, err error) {
	conditions.Set(sess, &sess.Status.Conditions, metav1.Condition{
		Type: spiceboxv1alpha1.SpiceboxSessionConditionFailed, Status: metav1.ConditionTrue,
		Reason:  spiceboxv1alpha1.ReasonClassMissing,
		Message: fmt.Sprintf("class %q: %v", sess.Spec.Class, err),
	})
}

func (r *Reconciler) setClassInvalid(sess *spiceboxv1alpha1.SpiceboxSession, err error) {
	conditions.Set(sess, &sess.Status.Conditions, metav1.Condition{
		Type: spiceboxv1alpha1.SpiceboxSessionConditionFailed, Status: metav1.ConditionTrue,
		Reason: spiceboxv1alpha1.ReasonClassInvalid, Message: err.Error(),
	})
}

// pvcBindPollInterval is how often we requeue while waiting for a sandbox's
// precondition (the shared workspace PVC) to exist before creating it.
// Existence is normally sub-second (hostpath / immediate), so a short poll
// keeps startup snappy without a PVC watch here.
const pvcBindPollInterval = 3 * time.Second

// sandboxPendingPollInterval is how often a session is re-reconciled while its
// backend reports a state it has flagged Status.RequiresPolling — never on
// phase alone. Deliberately the same 3s as
// pvcBindPollInterval above rather than a second, differently-chosen number:
// both bound the same class of wait — one more controller hop landing a change
// on an object AP does not own — and the sharpest instance is the agent-sandbox
// adoption window, where the base Sandbox controller patches the session labels
// onto an already-running pooled pod. That is sub-second to a couple of seconds
// in practice, so seconds is the right order of magnitude; minutes would make a
// pre-warmed session slower to become usable than a cold one, defeating the
// point of pre-warming.
const sandboxPendingPollInterval = 3 * time.Second

// computeEffectiveToolspecs returns the resolved set or an error/condition
// hint when spec.toolspecs is not a subset of the class's set.
func computeEffectiveToolspecs(sess *spiceboxv1alpha1.SpiceboxSession, cls *spiceboxv1alpha1.SpiceboxClass) (effective []string, notInClass []string) {
	classSet := map[string]bool{}
	for _, ref := range cls.Spec.Toolspecs {
		classSet[ref.Name] = true
	}
	if len(sess.Spec.Toolspecs) == 0 {
		out := make([]string, 0, len(cls.Spec.Toolspecs))
		for _, ref := range cls.Spec.Toolspecs {
			out = append(out, ref.Name)
		}
		return out, nil
	}
	for _, ref := range sess.Spec.Toolspecs {
		if !classSet[ref.Name] {
			notInClass = append(notInClass, ref.Name)
		} else {
			effective = append(effective, ref.Name)
		}
	}
	return effective, notInClass
}

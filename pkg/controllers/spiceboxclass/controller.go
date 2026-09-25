// Package spiceboxclass implements the SpiceboxClass controller.
//
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=spiceboxclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=spiceboxclasses/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=clusteragentsettings,verbs=get;list;watch
// spiceboxtoolchains get;list;watch is already granted cluster-wide by sibling
// controllers' markers — controller-gen unions and dedupes them into one
// role.yaml — so this line changes nothing generated. It is declared anyway
// because this controller genuinely Gets/Lists/Watches the resource, so its RBAC
// needs are self-documented rather than accidentally covered by a marker that
// could someday be narrowed or removed.
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=spiceboxtoolchains,verbs=get;list;watch
// A SandboxTemplate is immutable by construction: agentsandbox's templateNameFor
// hashes the rendered PodSpec into the object name, so any change mints a NEW
// template and ensureTemplate returns early on a hit without writing. Hence no
// update verb here — the only Client.Update in the pre-warming path rewrites a
// SandboxWarmPool's templateRef and replica count, which is why that resource,
// and only that resource, carries update.
//
// +kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxtemplates,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxwarmpools,verbs=get;list;watch;create;update;delete
package spiceboxclass

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	apreconcile "github.com/authzed/openagentprimitives/pkg/controllers/internal/reconcile"
	"github.com/authzed/openagentprimitives/pkg/platform/settings"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds"
	sandboxregistry "github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/tools/toolchain/resolve"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/registry"
)

type Reconciler struct {
	Client client.Client
	Scheme *apiruntime.Scheme
	// Registry supplies the builtin toolkits whose sensitive env vars
	// form the reserved set for EnvDefaults validation.
	Registry *registry.Registry
	// Runtimes is one Runtime per registered sandbox kind, built once at
	// operator startup (internal/cmd/operator/main.go) and shared with the
	// SpiceboxSession and ToolCall reconcilers. This controller uses it only
	// to type-assert a kind's Runtime against sandboxkinds.Prewarmer — it
	// never calls Ensure/Status/Teardown/Executor itself. A kind absent from
	// this map (e.g. NewRuntime failed because its peer CRD is not installed)
	// type-asserts to "not a Prewarmer" the same as a kind that never
	// implements it, which is the correct outcome either way: this cluster
	// cannot pre-warm that kind right now.
	Runtimes sandboxkinds.Runtimes
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Shared by both watches below: every SpiceboxClass is potentially
	// affected regardless of which object changed, so both map funcs list
	// and re-enqueue the same way.
	enqueueAllClasses := func(ctx context.Context, o client.Object, sourceKind string) []reconcile.Request {
		var list spiceboxv1alpha1.SpiceboxClassList
		if err := r.Client.List(ctx, &list); err != nil {
			log.FromContext(ctx).Info("list SpiceboxClasses for watch failed; dropping re-enqueue (self-heals on next resync)",
				"sourceKind", sourceKind, "name", o.GetName(), "err", err.Error())
			return nil
		}
		out := make([]reconcile.Request, 0, len(list.Items))
		for i := range list.Items {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
		}
		return out
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&spiceboxv1alpha1.SpiceboxClass{}).
		WithOptions(ctrlcontroller.Options{SkipNameValidation: ptr.To(true)}).
		Watches(&spiceboxv1alpha1.SpiceboxToolspec{}, handler.EnqueueRequestsFromMapFunc(
			func(ctx context.Context, o client.Object) []reconcile.Request {
				return enqueueAllClasses(ctx, o, "SpiceboxToolspec")
			})).
		// A ClusterAgentSettings change may re-size (or newly enable/disable)
		// every class's warm pool — see ResolveClassWarmPool's two-tier
		// resolution. Without this watch, a cluster admin's edit would only
		// take effect on each class's next unrelated reconcile or periodic
		// resync, silently stale in between. Mirrors the identical watch on
		// AgentClass/AgentSession (pkg/controllers/agentclass,
		// pkg/controllers/agentsession) for the same underlying settings CRD.
		Watches(&spiceboxv1alpha1.ClusterAgentSettings{}, handler.EnqueueRequestsFromMapFunc(
			func(ctx context.Context, o client.Object) []reconcile.Request {
				return enqueueAllClasses(ctx, o, "ClusterAgentSettings")
			})).
		// Unlike the two watches above, a SpiceboxToolchain edit affects only
		// the classes that actually NAME it in spec.toolchains — not every
		// class — so this map func filters rather than reusing
		// enqueueAllClasses. Without this watch a catalog bump (a re-pinned
		// image, a toolchain flipping Valid=True after being missing) would not
		// reach a naming class until the manager's ~10h default resync, so the
		// warm pool would keep serving a stale shape and every session binding
		// to it would cold-path for hours. SpiceboxSession
		// (pkg/controllers/spiceboxsession) watches this SAME CRD for the
		// identical reason, but with a DIFFERENT filter: it re-enqueues
		// UNBOUND sessions (Status.ResolvedClass == nil) regardless of
		// whether they name the changed toolchain, because an unbound
		// session's own toolchain resolution hasn't happened yet and could be
		// the very thing stuck on this toolchain's absence. This watch
		// filters the other way — by NAME, not by boundness — because a
		// SpiceboxClass has no "unresolved" state to fall back on; naming the
		// toolchain is the only signal available.
		Watches(&spiceboxv1alpha1.SpiceboxToolchain{}, handler.EnqueueRequestsFromMapFunc(r.mapToolchainToClasses)).
		Complete(r)
}

// mapToolchainToClasses returns reconcile requests for every SpiceboxClass
// naming o (a SpiceboxToolchain) in spec.toolchains. Unlike enqueueAllClasses
// above, this filters: a toolchain a class never mentions cannot affect that
// class's resolution, and enqueueing it anyway would just be wasted work.
func (r *Reconciler) mapToolchainToClasses(ctx context.Context, o client.Object) []reconcile.Request {
	tc, ok := o.(*spiceboxv1alpha1.SpiceboxToolchain)
	if !ok {
		return nil
	}
	var list spiceboxv1alpha1.SpiceboxClassList
	if err := r.Client.List(ctx, &list); err != nil {
		log.FromContext(ctx).Info("list SpiceboxClasses for SpiceboxToolchain watch failed; dropping re-enqueue (self-heals on next resync)",
			"spiceboxtoolchain", tc.Name, "err", err.Error())
		return nil
	}
	var out []reconcile.Request
	for i := range list.Items {
		for _, name := range list.Items[i].Spec.Toolchains {
			if name == tc.Name {
				out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
				break
			}
		}
	}
	return out
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var class spiceboxv1alpha1.SpiceboxClass
	if cont, err := apreconcile.LoadInto(ctx, r.Client, req.NamespacedName, &class); !cont {
		return ctrl.Result{}, err
	}

	reservedEnv := sensitiveEnvNames(r.Registry)
	// validateSpec checks only the CLASS'S OWN spec.sandbox.warmPool — never
	// a tier-resolved value — so a cluster/tier default this class never set
	// can never invalidate it. See the WarmPool check's own comment inside
	// validateSpec for why.
	specErr := validateSpec(class.Spec, reservedEnv, r.Runtimes)

	// warmPoolMalformed isolates ONE specific fact from the rest of specErr:
	// the CLASS'S OWN warmPool stanza — never a tier-inherited value,
	// validateSpec never sees that — asks for capacity (replicas > 0) but
	// names nowhere to put it. Reclaiming capacity is NOT a function of
	// overall spec validity: an admin who clears warmPool (or sets
	// replicas: 0) while the class is invalid for some UNRELATED reason (a
	// bad resources block, say) still means it, and reconcilePool's sweep
	// must still run to honor "no list means pre-warming OFF, no spend" —
	// see reconcilePool's own doc comment for the createAllowed/sweep split.
	// But when the malformed part IS the warmPool stanza itself, the desired
	// set cannot be computed honestly: an empty desired set in that case
	// would tear down real capacity over what is more likely a typo mid-edit
	// than an intentional teardown, so the sweep is skipped for this reason
	// alone, the same as pool creation already is.
	warmPoolMalformed := warmPoolNamespacesMissing(class.Spec.Sandbox.WarmPool)

	// Resolve the class's toolchains into concrete, self-contained mounts, using
	// the SAME resolver the SpiceboxSession controller calls at bind time, so a
	// class's status and a bound session's status are comparable by eye. A class
	// naming none resolves and writes nothing; one whose toolchains were cleared
	// has its stale record cleared in the else branch below.
	//
	// Failure splits into two cases that DIFFER IN KIND, the same split the
	// SpiceboxSession controller preserves at bind time:
	//
	//   - ErrToolchainMissing / ErrToolchainNotValid: a REAL fact about the named
	//     toolchain that will not change on its own, so backoff would only waste
	//     cycles. The LAST KNOWN-GOOD status.ResolvedToolchains and
	//     ToolchainSetDigest are left untouched — a stale-but-real record beats
	//     an empty one — ToolchainResolutionMessage records the failure, and no
	//     error is returned: the SpiceboxToolchain watch re-enqueues this class
	//     the moment the referenced toolchain starts existing or flips
	//     Valid=True, which a backoff retry could not improve on.
	//   - Anything else, such as a transient apiserver Get failure: NOT a fact
	//     about the toolchain. Persisting it into ToolchainResolutionMessage
	//     would mislead the moment the apiserver recovers, and the watch cannot
	//     rescue it either, since nothing about the toolchain changed. It folds
	//     into toolchainErr instead, joining the ordinary
	//     requeue-with-backoff path.
	//
	// Either way a false toolchainsOK folds into createAllowed below, skipping
	// pooling for this pass without freezing the sweep, which does not depend on
	// toolchains. Both cases deliberately leave specErr and cond untouched: class
	// validity must not depend on the toolchain catalog, or an unrelated
	// toolchain edit would flip classes red that never asked for a pool.
	var (
		resolvedToolchains []spiceboxv1alpha1.ToolchainMount
		toolchainErr       error
	)
	toolchainsOK := true
	if len(class.Spec.Toolchains) > 0 {
		mounts, setDigest, err := resolve.Resolve(ctx, r.Client, class.Spec.Toolchains)
		switch {
		case err == nil:
			resolvedToolchains = mounts
			class.Status.ResolvedToolchains = mounts
			class.Status.ToolchainSetDigest = setDigest
			class.Status.ToolchainResolutionMessage = ""
		case errors.Is(err, resolve.ErrToolchainMissing), errors.Is(err, resolve.ErrToolchainNotValid):
			toolchainsOK = false
			class.Status.ToolchainResolutionMessage = err.Error()
			logger.Info("class toolchain resolution failed (missing or invalid); pre-warming skipped this pass, class stays valid",
				"class", class.Name, "toolchains", class.Spec.Toolchains, "err", err.Error())
		default:
			// Not classifiable as missing/invalid — treat as an ordinary
			// operational failure. Do NOT persist err into
			// ToolchainResolutionMessage (see the case-block comment above).
			toolchainsOK = false
			toolchainErr = err
		}
	} else {
		class.Status.ResolvedToolchains = nil
		class.Status.ToolchainSetDigest = ""
		class.Status.ToolchainResolutionMessage = ""
	}

	// poolErr carries an OPERATIONAL failure — fetching the cluster settings
	// tier, or reconcilePool itself — separately from specErr. It must NOT
	// suppress the status write below: the class's own spec is still valid,
	// so Valid/observedGeneration must still be persisted. It still causes a
	// requeue, so it is returned AFTER that write, mirroring computeCoverage's
	// own error handling further down for the same "operational failure, not
	// a spec verdict" reason.
	var poolErr error
	if !warmPoolMalformed {
		clusterSettings, err := r.fetchClusterSettings(ctx)
		if err != nil {
			poolErr = err
		} else {
			// Tier-resolved, not class.Spec.Sandbox.WarmPool read directly: a
			// cluster admin's default must actually SIZE a pool for a class
			// that does not override it — this is the sizing/creation path,
			// distinct from the validation check above, which deliberately
			// stays untiered. See settings.ResolveClassWarmPool's doc comment
			// for why this stops at the cluster tier (no namespace tier — a
			// cluster-scoped SpiceboxClass has none to consult).
			resolvedWarmPool := settings.ResolveClassWarmPool(class.Spec.Sandbox, clusterSettings)
			// createAllowed gates only pool CREATION/SIZING on the class's
			// overall spec being valid ("do not act on an invalid spec") —
			// reconcilePool's sweep runs regardless, so an UNRELATED spec
			// error (bad resources, say) cannot itself freeze pool reclamation.
			// See reconcilePool's own doc comment. toolchainsOK folds in here
			// too, for the identical reason: a toolchain resolution failure
			// must not create/resize a pool whose overlay cannot be built this
			// pass, but must not freeze reclamation of capacity nobody wants
			// any more either.
			if err := r.reconcilePool(ctx, &class, resolvedWarmPool, specErr == nil && toolchainsOK, resolvedToolchains); err != nil {
				// A CLASS-OWNED request (class.Spec.Sandbox.WarmPool itself)
				// that ErrPrewarmingUnavailable refuses IS a verdict about the
				// class as configured against this cluster, so it folds into
				// specErr — same Valid=False treatment a statically-detectable
				// WarmPool misconfiguration already gets. A tier-inherited
				// request hitting the same error is handled and logged INSIDE
				// reconcilePool instead (never reaches here) — see its doc
				// comment for why that split exists. Note this can only ever
				// happen when createAllowed was true (ErrPrewarmingUnavailable
				// is only reachable through the create call reconcilePool
				// itself gates on createAllowed), so specErr is guaranteed nil
				// here and this assignment can never clobber an existing,
				// unrelated validation error.
				if errors.Is(err, sandboxkinds.ErrPrewarmingUnavailable) {
					specErr = &validationError{reason: spiceboxv1alpha1.ReasonClassInvalidSandboxWarmPool, msg: err.Error()}
				} else {
					poolErr = err
				}
			}
		}
	}

	cond := metav1.Condition{
		Type:               spiceboxv1alpha1.SpiceboxClassConditionValid,
		ObservedGeneration: class.Generation,
		LastTransitionTime: metav1.Now(),
	}
	if specErr != nil {
		cond.Status = metav1.ConditionFalse
		cond.Reason = reasonFor(specErr)
		cond.Message = specErr.Error()
		logger.Info("class invalid", "reason", cond.Reason, "msg", cond.Message)
	} else {
		cond.Status = metav1.ConditionTrue
		cond.Reason = spiceboxv1alpha1.ReasonClassValid
		cond.Message = "class spec validated"
	}

	conditions.Set(&class, &class.Status.Conditions, cond)

	// Compute toolspec coverage only when the spec itself is valid. A
	// validateSpec failure (InvalidImage, InvalidTool, InvalidResource,
	// InvalidEnvDefaults) must NOT be overwritten by a downstream Resolved
	// stamp — the class is invalid, period. Coverage is only meaningful
	// for a class whose spec passed structural validation.
	// covErr, like poolErr above, is an OPERATIONAL failure that must NOT
	// suppress the status write below: an unreadable SpiceboxToolspec says
	// nothing about whether the class's own spec is valid, and returning here
	// would leave the class with no Valid condition and no observedGeneration
	// at all — an empty status instead of a diagnosis. It still requeues, so
	// it is returned AFTER the write, exactly as poolErr is. Third instance of
	// this shape in this function; the first two (reconcilePool,
	// fetchClusterSettings) were fixed the same way.
	var covErr error
	if specErr == nil && len(class.Spec.Toolspecs) > 0 {
		cov, missing, err := computeCoverage(ctx, r.Client, &class)
		if err != nil {
			covErr = err
		} else {
			class.Status.ToolspecCoverage = cov
			validCond := metav1.Condition{
				Type:               spiceboxv1alpha1.SpiceboxClassConditionValid,
				Status:             metav1.ConditionTrue,
				Reason:             "Resolved",
				ObservedGeneration: class.Generation,
			}
			if len(missing) > 0 {
				validCond.Status = metav1.ConditionFalse
				validCond.Reason = spiceboxv1alpha1.ReasonToolspecCoverageMissing
				validCond.Message = fmt.Sprintf("tools without covering Toolspec: %s", strings.Join(missing, ", "))
			}
			conditions.Set(&class, &class.Status.Conditions, validCond)
		}
	}

	class.Status.ObservedGeneration = class.Generation

	if err := r.Client.Status().Update(ctx, &class); err != nil {
		return ctrl.Result{}, err
	}
	// All three operational failures requeue, after the status write, and ALL
	// THREE are returned: they are independently reachable (covErr needs
	// specErr == nil, which says nothing about poolErr or toolchainErr; a
	// transient toolchainErr says nothing about either of the others), so
	// returning only one would drop the rest on the floor with nothing
	// logging them — the no-silent-errors rule. errors.Join yields nil when
	// all three are nil, so the ordinary path is unchanged.
	return ctrl.Result{}, errors.Join(poolErr, toolchainErr, covErr)
}

// fetchClusterSettings reads the singleton ClusterAgentSettings, tolerating
// its absence (a cluster that has never created one simply has no
// cluster-tier sandbox default — not an error). Only the cluster tier: see
// settings.ResolveClassWarmPool's doc comment for why a cluster-scoped
// SpiceboxClass has no namespace tier to consult.
func (r *Reconciler) fetchClusterSettings(ctx context.Context) (*spiceboxv1alpha1.SettingsSpec, error) {
	var cas spiceboxv1alpha1.ClusterAgentSettings
	switch err := r.Client.Get(ctx, client.ObjectKey{Name: spiceboxv1alpha1.ClusterAgentSettingsName}, &cas); {
	case err == nil:
		return &cas.Spec, nil
	case apierrors.IsNotFound(err):
		return nil, nil
	default:
		return nil, err
	}
}

// warmPoolNamespacesMissing reports whether wp asks for capacity
// (replicas > 0) but names nowhere to place it — the one shape of a warmPool
// request whose desired set cannot be computed honestly. Shared by
// validateSpec (where it is the class's OWN request and therefore a
// validation error) and Reconcile (where it gates the sweep independently of
// the rest of the spec's validity — see Reconcile's warmPoolMalformed
// comment for why the two call sites need the identical, structural check).
func warmPoolNamespacesMissing(wp *spiceboxv1alpha1.WarmPoolConfig) bool {
	return wp != nil && wp.Replicas > 0 && len(wp.Namespaces) == 0
}

// reconcilePool brings class's pre-warmed capacity in line with wp (the
// tier-resolved WarmPool — see settings.ResolveClassWarmPool), for a sandbox
// kind whose Runtime opts into sandboxkinds.Prewarmer. A kind that does not
// implement it, or a class that requests no pool in any namespace, is a
// deliberate no-op — most classes, and the built-in "pod" backend, never
// implement Prewarmer at all, and that is a perfectly ordinary configuration,
// not a degraded one.
//
// createAllowed gates the two verbs this function performs DIFFERENTLY.
// CREATING or SIZING a pool happens only when createAllowed is true — Reconcile
// passes specErr == nil && toolchainsOK — so neither an invalid class nor one
// whose toolchains failed to resolve gets new capacity. The SWEEP always runs
// regardless: reclaiming capacity nobody wants any more is not "acting on an
// invalid spec", and gating it the same way would freeze a class's pool teardown
// for as long as it was invalid for any OTHER, unrelated reason — another door
// onto the exact orphaning defect SweepOrphanedPools exists to close. Reconcile
// skips this function entirely when the class's OWN warmPool stanza is
// malformed, the one case where the desired set cannot be computed honestly.
//
// toolchains is the class's already-resolved mounts, empty when it names none or
// resolution failed this pass. Carried straight into every PoolRequest below;
// this function resolves nothing itself.
//
// classOwned distinguishes WHO asked: a non-nil class.Spec.Sandbox.WarmPool
// means the CLASS set it, rather than inheriting wp from the cluster tier. An
// unsatisfiable wp — no namespaces, or a kind that cannot pre-warm — is the
// CLASS's mistake only when classOwned; a cluster admin's default landing on a
// class that never mentioned warmPool must not invalidate it. validateSpec
// enforces the classOwned case statically, so anything unsatisfiable reaching
// here is tier-inherited, EXCEPT sandboxkinds.ErrPrewarmingUnavailable — a
// RUNTIME fact a static type assertion cannot see, which can therefore arrive
// with classOwned true and is handled explicitly below.
func (r *Reconciler) reconcilePool(
	ctx context.Context, class *spiceboxv1alpha1.SpiceboxClass, wp *spiceboxv1alpha1.WarmPoolConfig, createAllowed bool,
	toolchains []spiceboxv1alpha1.ToolchainMount,
) error {
	sbKind, ok := sandboxregistry.Get(class.Spec.Sandbox.ResolvedKind())
	if !ok {
		// validateSpec already rejects an unregistered kind before this runs.
		// The sweep is deliberately skipped too, NOT just the creation: an
		// unresolvable kind is most likely a typo mid-edit, and a typo must
		// never authorize tearing down real capacity — the same reasoning
		// warmPoolMalformed applies to an uncomputable desired set.
		return nil
	}
	classOwned := class.Spec.Sandbox.WarmPool != nil

	// desiredNamespaces is what SweepOrphanedPools treats as "keep" FOR THE
	// CLASS'S CURRENT KIND: empty means "reclaim every pool this class has
	// anywhere," which is exactly right when wp is nil, replicas is 0, or the
	// request could not be attempted at all (see the branches below). Computed
	// regardless of createAllowed — an unrelated spec error must not change
	// what is currently DESIRED, only whether new capacity may be CREATED to
	// match it. Every OTHER kind desires nothing by construction; see
	// sweepEveryPrewarmer.
	var desiredNamespaces []string

	prewarmer, isPrewarmer := r.Runtimes[sbKind.Name()].(sandboxkinds.Prewarmer)
	switch {
	case !isPrewarmer:
		// classOwned is guaranteed false here: validateSpec already rejects a
		// class-owned wp.Replicas>0 on a non-Prewarmer kind, so this function
		// is never called for that case with createAllowed true. Logged
		// unconditionally regardless — the no-silent-errors rule applies to a
		// future bug that breaks that invariant too, not only to the expected
		// tier-inherited case. Note this is NOT a return: the sweep below still
		// runs, because a class that just SWITCHED to a non-Prewarmer kind is
		// exactly the case whose old kind's pools nothing else would reclaim.
		if wp != nil && wp.Replicas > 0 {
			log.FromContext(ctx).Info(
				"resolved warmPool requests pre-warmed capacity, but this kind does not implement pre-warming; no pool created",
				"class", class.Name, "kind", sbKind.Name(), "replicas", wp.Replicas, "classOwnWarmPool", classOwned)
		}

	case wp != nil:
		switch {
		case wp.Replicas > 0 && len(wp.Namespaces) == 0:
			// classOwned is guaranteed false here for the same reason as above:
			// validateSpec already rejects this combination when the class set
			// it itself, and Reconcile's warmPoolMalformed check keeps a
			// tier-inherited version of this same shape from tearing down real
			// capacity — this function only ever sees it because that check
			// only looks at the class's OWN stanza, so a MALFORMED CLUSTER-TIER
			// default (classOwned false) still reaches here, correctly, with
			// nothing this class asked for to preserve.
			log.FromContext(ctx).Info(
				"resolved warmPool has replicas>0 but no namespaces named; no pool created",
				"class", class.Name, "kind", sbKind.Name(), "replicas", wp.Replicas, "classOwnWarmPool", classOwned)
		case wp.Replicas > 0:
			desiredNamespaces = wp.Namespaces
		}

		if createAllowed {
			for _, ns := range wp.Namespaces {
				err := prewarmer.ReconcilePool(ctx, sandboxkinds.PoolRequest{
					ClassName:  class.Name,
					Namespace:  ns,
					Class:      class.Spec,
					Replicas:   wp.Replicas,
					ClassUID:   class.UID,
					Toolchains: toolchains,
				})
				if err == nil {
					continue
				}
				if errors.Is(err, sandboxkinds.ErrPrewarmingUnavailable) && !classOwned {
					// A tier default this class never asked for, on a cluster
					// that cannot currently pre-warm at all. Not this class's
					// mistake — see the function doc comment. prewarmAvailable
					// is a Runtime-wide fact, not per-namespace, so every
					// remaining namespace would hit the identical error; one log
					// line covers all of them, and desiredNamespaces reverts to
					// empty since nothing was (or will be) created.
					log.FromContext(ctx).Info(
						"resolved warmPool requests pre-warmed capacity, but pre-warming is not currently available "+
							"on this cluster; no pool created",
						"class", class.Name, "kind", sbKind.Name(), "namespace", ns, "err", err.Error())
					desiredNamespaces = nil
					break
				}
				// Either ErrPrewarmingUnavailable on a CLASS-OWNED request (a
				// real verdict about the class — Reconcile folds this into
				// specErr via errors.Is), or an ordinary operational failure
				// (Reconcile requeues via poolErr). Both propagate unchanged.
				return fmt.Errorf("reconcile warm pool for class %s in namespace %s: %w", class.Name, ns, err)
			}
		}
	}

	return r.sweepEveryPrewarmer(ctx, class, sbKind.Name(), desiredNamespaces)
}

// sweepEveryPrewarmer reclaims orphaned pre-warmed capacity for class across
// EVERY registered Prewarmer backend, not merely the kind the class names
// today.
//
// The sweep is the backstop the per-namespace ReconcilePool loop cannot be:
// that loop only ever acts on the CURRENT desired set (and only when
// createAllowed), so a namespace REMOVED from it, the whole warmPool stanza
// cleared, or a class momentarily invalid for an UNRELATED reason are all
// never revisited by it.
//
// It is a function of the CLASS, not of the class's current kind, because
// SweepOrphanedPools is per-kind by construction — a backend can only find
// its own native objects. Editing spec.sandbox.kind away from a
// Prewarmer-capable backend therefore used to strand that backend's pools
// permanently: nothing would ever resolve the OLD kind's Runtime again, the
// ownerReference cannot help (the class still exists), and the idle pods
// bill forever, indistinguishable from a class that was always on the new
// kind. Asking every registered Prewarmer, with an empty desired set for
// every kind but the current one, closes that by construction — a kind the
// class does not name desires nothing, so anything it still holds for this
// class is by definition orphaned.
//
// Iterated in sorted order so a multi-backend failure names the same kind
// every pass rather than whichever one Go's map iteration reached first.
func (r *Reconciler) sweepEveryPrewarmer(
	ctx context.Context, class *spiceboxv1alpha1.SpiceboxClass, currentKind string, desiredNamespaces []string,
) error {
	names := make([]string, 0, len(r.Runtimes))
	for name := range r.Runtimes {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		pw, ok := r.Runtimes[name].(sandboxkinds.Prewarmer)
		if !ok {
			continue
		}
		desired := desiredNamespaces
		if name != currentKind {
			desired = nil
		}
		if err := pw.SweepOrphanedPools(ctx, class.Name, class.UID, desired); err != nil {
			return fmt.Errorf("sweep orphaned warm pools for class %s (kind %s): %w", class.Name, name, err)
		}
	}
	return nil
}

type validationError struct {
	reason string
	msg    string
}

func (e *validationError) Error() string { return e.msg }

func validateSpec(s spiceboxv1alpha1.SpiceboxClassSpec, reservedEnv []string, runtimes sandboxkinds.Runtimes) error {
	if strings.TrimSpace(s.Image) == "" {
		return &validationError{reason: spiceboxv1alpha1.ReasonClassInvalidImage, msg: "spec.image must be set"}
	}
	if len(s.Tools) == 0 {
		return &validationError{reason: spiceboxv1alpha1.ReasonClassInvalidTool, msg: "spec.tools must contain at least one entry"}
	}
	seen := map[string]struct{}{}
	for i, t := range s.Tools {
		if t.Name == "" {
			return &validationError{reason: spiceboxv1alpha1.ReasonClassInvalidTool, msg: fmt.Sprintf("spec.tools[%d].name is required", i)}
		}
		if _, dup := seen[t.Name]; dup {
			return &validationError{reason: spiceboxv1alpha1.ReasonClassInvalidTool, msg: fmt.Sprintf("duplicate tool name: %s", t.Name)}
		}
		seen[t.Name] = struct{}{}
		if len(t.Command) == 0 || t.Command[0] == "" {
			return &validationError{reason: spiceboxv1alpha1.ReasonClassInvalidTool, msg: fmt.Sprintf("spec.tools[%d].command is required", i)}
		}
	}
	if s.Resources.CPU.IsZero() || s.Resources.Memory.IsZero() || s.Resources.EphemeralStorage.IsZero() {
		return &validationError{reason: spiceboxv1alpha1.ReasonClassInvalidResource, msg: "spec.resources.cpu/memory/ephemeralStorage must all be set"}
	}
	// A zero-sized /tmp or /work would produce a pod nothing can run in. The
	// caps are deliberately NOT checked against the memory limit: a sizeLimit is
	// a cap, not a reservation, and shipped classes run 64Mi of memory with the
	// larger default caps.
	if err := spiceboxv1alpha1.ValidateScratchSizes(s.Resources); err != nil {
		return &validationError{reason: spiceboxv1alpha1.ReasonClassInvalidResource, msg: err.Error()}
	}
	// Reject envDefaults keys that exactly match a reserved auth-injected
	// env-var name — otherwise an operator could shadow a binding-controlled
	// credential with a spec-controlled value before the runner attaches it.
	if err := spiceboxv1alpha1.ValidateEnvDefaults(s.EnvDefaults, reservedEnv); err != nil {
		return &validationError{reason: spiceboxv1alpha1.ReasonClassInvalidEnvDefaults, msg: err.Error()}
	}
	// Reject privateVolumes whose name/mountPath would collide with a
	// pod-builder-reserved volume, a ConfigMap mount, or another private
	// volume — caught here so the malformed spec never reaches pod-apply.
	if err := spiceboxv1alpha1.ValidatePrivateVolumes(s.PrivateVolumes, s.Mounts); err != nil {
		return &validationError{reason: spiceboxv1alpha1.ReasonClassInvalidPrivateVolumes, msg: err.Error()}
	}
	// Resolve the sandbox backend and check the class against it. An
	// UNREGISTERED kind is refused rather than downgraded to the built-in
	// backend: silently running on a different substrate than the one
	// requested is worse than refusing to run. An UNSET kind is not the same
	// thing — it means "unspecified", which has a documented default, so
	// ResolvedKind supplies it.
	sbKind, ok := sandboxregistry.Get(s.Sandbox.ResolvedKind())
	if !ok {
		return &validationError{
			reason: spiceboxv1alpha1.ReasonClassInvalidSandbox,
			msg: fmt.Sprintf("spec.sandbox.kind %q is not a registered sandbox backend (known: %s)",
				s.Sandbox.ResolvedKind(), strings.Join(sandboxregistry.Keys(), ", ")),
		}
	}
	if err := sandboxkinds.ValidateClassAgainstKind(sbKind, s); err != nil {
		return &validationError{reason: spiceboxv1alpha1.ReasonClassInvalidSandbox, msg: err.Error()}
	}
	// The backend's own veto, and the second half of the chain. The check above
	// asks only what the seam can ask generically — Supports(Feature) — which
	// covers nothing a backend rejects for its own reasons. Without this call a
	// bring-your-own kind that implements ValidateClass per the interface
	// contract is silently ignored: the class goes Valid=True, a session binds
	// to it, and the backend renders a sandbox missing whatever it meant to
	// reject.
	//
	// The kind is named in the message because a backend's error need not
	// mention itself, and an operator reading the condition has to know which
	// backend refused.
	//
	// Both halves run again in pkg/controllers/spiceboxsession, after the
	// session-level tier override may have replaced the kind this class
	// declared — validating here alone only ever checks the DECLARED kind.
	if err := sbKind.ValidateClass(s); err != nil {
		return &validationError{
			reason: spiceboxv1alpha1.ReasonClassInvalidSandbox,
			msg:    fmt.Sprintf("sandbox kind %q rejected the class: %v", sbKind.Name(), err),
		}
	}
	// s.Sandbox.WarmPool — the CLASS'S OWN request — not the tier-resolved
	// value: this check exists to tell an admin "you asked for something this
	// backend cannot do," and a class that never set its own warmPool has not
	// asked for anything. A cluster/tier default that lands on a kind unable
	// to honor it is NOT this class's error — reconcilePool logs that case
	// instead of creating a pool, without touching this class's Valid
	// condition. (Reused kind-agnostically, the resolved value would flip
	// EVERY class using a non-Prewarmer kind to Invalid the moment any
	// cluster-wide warmPool default was set — the exact bug this comment
	// exists to prevent from being reintroduced.)
	if wp := s.Sandbox.WarmPool; wp != nil && wp.Replicas > 0 {
		// Asking for capacity but naming nowhere to put it is a validation
		// error, not a silent no-op: pre-warming spends real money, so the
		// cluster owner must state exactly where, the same way they state
		// exactly how many (see WarmPoolConfig.Namespaces's doc comment).
		// warmPoolNamespacesMissing, not an inline len() check: Reconcile's
		// warmPoolMalformed gate on the sweep needs the IDENTICAL structural
		// test, and a hand-duplicated copy is exactly the kind of thing that
		// drifts out of sync with this one.
		if warmPoolNamespacesMissing(wp) {
			return &validationError{
				reason: spiceboxv1alpha1.ReasonClassInvalidSandboxWarmPool,
				msg: fmt.Sprintf(
					"spec.sandbox.warmPool.replicas=%d requests pre-warmed capacity but warmPool.namespaces is empty; "+
						"name at least one namespace to pre-warm in", wp.Replicas),
			}
		}
		// Asking for pre-warmed capacity on a backend that cannot provide it is
		// also a validation error, not a silent no-op — a class author who sets
		// warmPool.replicas expects it to do something. runtimes[sbKind.Name()]
		// is a nil interface for both "this kind never implements Prewarmer" and
		// "this cluster has no Runtime for this kind at all" (e.g. NewRuntime
		// failed because a bring-your-own backend's peer CRD is not installed);
		// the type assertion reports false for both, which is the right answer
		// either way — this cluster cannot pre-warm that kind right now.
		if _, ok := runtimes[sbKind.Name()].(sandboxkinds.Prewarmer); !ok {
			return &validationError{
				reason: spiceboxv1alpha1.ReasonClassInvalidSandboxWarmPool,
				msg: fmt.Sprintf(
					"spec.sandbox.warmPool.replicas=%d requests pre-warmed capacity, but sandbox kind %q does not support it",
					wp.Replicas, sbKind.Name()),
			}
		}
	}
	return nil
}

func reasonFor(err error) string {
	if v, ok := err.(*validationError); ok {
		return v.reason
	}
	return "Unknown"
}

// computeCoverage returns (coverage map, missing tool names).
func computeCoverage(ctx context.Context, c client.Client, cls *spiceboxv1alpha1.SpiceboxClass) (map[string][]string, []string, error) {
	cov := map[string][]string{}
	for _, ref := range cls.Spec.Toolspecs {
		var ts spiceboxv1alpha1.SpiceboxToolspec
		if err := c.Get(ctx, client.ObjectKey{Name: ref.Name}, &ts); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, nil, err
		}
		if !meta.IsStatusConditionTrue(ts.Status.Conditions, spiceboxv1alpha1.SpiceboxToolspecConditionValid) {
			continue
		}
		for _, tool := range cls.Spec.Tools {
			if ts.Spec.Toolkit.Name == tool.Name {
				cov[tool.Name] = append(cov[tool.Name], ref.Name)
			}
		}
	}
	var missing []string
	for _, tool := range cls.Spec.Tools {
		if len(cov[tool.Name]) == 0 {
			missing = append(missing, tool.Name)
		}
	}
	return cov, missing, nil
}

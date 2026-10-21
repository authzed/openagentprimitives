// Package browsertoolbox reconciles BrowserToolbox CRs: an identity-less
// toolbox is Valid=True by construction (the CRD's CEL rules already
// guarantee structural sanity -- see BrowserToolbox's doc comment), and a
// toolbox that names spec.access.identity additionally cross-checks that the
// referenced AgentIdentity exists in the same namespace, carries at least
// one directCredentials credential -- the one credential type a browser
// toolbox can actually bind to -- and is itself Valid=True (a missing or
// wrong-keyed backing Secret, or a failed vault verify, makes the identity
// unusable here exactly as it does everywhere else).
package browsertoolbox

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	apreconcile "github.com/authzed/openagentprimitives/pkg/controllers/internal/reconcile"
)

// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=browsertoolboxes,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=browsertoolboxes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=browsertoolboxes/finalizers,verbs=update
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=agentidentities,verbs=get;list;watch

// Reconciler reconciles BrowserToolbox objects.
type Reconciler struct {
	Client client.Client
}

// Reconcile validates one BrowserToolbox. The only failure modes are the
// identity cross-reference: a missing AgentIdentity, one that carries no
// directCredentials credential, or one whose own Valid condition is not
// True. Everything else about the spec is already enforced at admission by
// the CRD's field-level markers and XValidation rules.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var cr spiceboxv1alpha1.BrowserToolbox
	if err := r.Client.Get(ctx, req.NamespacedName, &cr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Stamped unconditionally, before any phase, so an invalid pass still
	// records which generation it observed -- the same convention
	// pkg/controllers/internal/reconcile.SetInvalid follows for every other
	// controller's failure path.
	cr.Status.ObservedGeneration = cr.Generation

	identityCheck := func(ctx context.Context) apreconcile.Outcome {
		name := cr.Spec.Access.Identity
		if name == "" {
			// Allowlist-with-no-identity, or a blocklist toolbox (CEL already
			// refuses Identity on a blocklist) -- nothing to cross-check.
			return apreconcile.Continue()
		}
		var ai spiceboxv1alpha1.AgentIdentity
		err := r.Client.Get(ctx, types.NamespacedName{Namespace: cr.Namespace, Name: name}, &ai)
		if apierrors.IsNotFound(err) {
			// Terminal until the identity appears; the AgentIdentity watch
			// (mapIdentityToToolboxes) re-enqueues this toolbox on create.
			conditions.SetFalse(&cr, &cr.Status.Conditions,
				spiceboxv1alpha1.BrowserToolboxConditionValid,
				spiceboxv1alpha1.ReasonBrowserToolboxIdentityMissing,
				fmt.Sprintf("spec.access.identity: AgentIdentity/%s not found in namespace %s", name, cr.Namespace))
			return apreconcile.StopAfter()
		}
		if err != nil {
			// Transient API error: fail closed WITHOUT stamping a terminal
			// condition -- FailWith makes controller-runtime requeue with
			// backoff, which is the only self-heal this reconciler has.
			// Collapsing this into IdentityMissing (the way sidecartoolbox's
			// classCheck collapses its class Get) would park the toolbox
			// permanently: that reconciler revalidates every 5m, this one sets
			// no RequeueAfter, and the watch cannot fire for an identity that
			// never changed.
			return apreconcile.FailWith(fmt.Errorf("get AgentIdentity %q: %w", name, err))
		}
		// Two unusable modes, checked shape-first: an identity with no
		// directCredentials block can NEVER serve a browser, no matter how
		// healthy it is, so that message leads even when the identity is also
		// Valid=False -- it names the deeper, generation-stable problem the
		// toolbox author must fix. The condition read follows: a right-shaped
		// identity whose own reconciler has it at Valid=False (backing Secret
		// missing/wrong-keyed, vault verify failed) is unusable TODAY, with a
		// message that folds in the identity's own reason. The AgentIdentity
		// watch re-fires on its status flips, so recovery self-heals.
		if !hasDirectCredentials(&ai) {
			conditions.SetFalse(&cr, &cr.Status.Conditions,
				spiceboxv1alpha1.BrowserToolboxConditionValid,
				spiceboxv1alpha1.ReasonBrowserToolboxIdentityUnusable,
				fmt.Sprintf("spec.access.identity: AgentIdentity/%s has no directCredentials credential", name))
			return apreconcile.StopAfter()
		}
		cond := meta.FindStatusCondition(ai.Status.Conditions, spiceboxv1alpha1.AgentIdentityConditionValid)
		if cond == nil {
			conditions.SetFalse(&cr, &cr.Status.Conditions,
				spiceboxv1alpha1.BrowserToolboxConditionValid,
				spiceboxv1alpha1.ReasonBrowserToolboxIdentityUnusable,
				fmt.Sprintf("spec.access.identity: AgentIdentity/%s has no Valid condition yet", name))
			return apreconcile.StopAfter()
		}
		if cond.Status != metav1.ConditionTrue {
			conditions.SetFalse(&cr, &cr.Status.Conditions,
				spiceboxv1alpha1.BrowserToolboxConditionValid,
				spiceboxv1alpha1.ReasonBrowserToolboxIdentityUnusable,
				fmt.Sprintf("spec.access.identity: AgentIdentity/%s is not Valid (%s)", name, cond.Reason))
			return apreconcile.StopAfter()
		}
		return apreconcile.Continue()
	}

	finalize := func(_ context.Context) apreconcile.Outcome {
		conditions.SetTrue(&cr, &cr.Status.Conditions,
			spiceboxv1alpha1.BrowserToolboxConditionValid,
			spiceboxv1alpha1.ReasonBrowserToolboxValid)
		return apreconcile.Continue()
	}

	phases := []apreconcile.Phase{identityCheck, finalize}
	if err := apreconcile.RunPhases(ctx, r.Client, &cr, phases); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// hasDirectCredentials reports whether ai carries at least one credential of
// type=directCredentials -- the one credential type a BrowserToolbox can
// bind to (see AgentCredential.DirectCredentials's doc comment: it never
// flows through the broker, so every other credential type is unusable here
// even if present).
//
// The test is union-block presence, not `c.Type == "directCredentials"`:
// comparing against the type literal outside pkg/platform/identity/credkind
// is exactly the branching TestNoCredentialTypeSwitchesOutsideCredkind
// forbids. Block presence is that kind's own HasBlock definition, it is the
// same read the agentidentity controller's verifyDirectCredential uses, and
// exclusivity (block set iff the declared type owns it) is validated by the
// AgentIdentity reconciler via registry.ValidateExclusive -- so for any
// identity that reconciles Valid the two reads agree.
func hasDirectCredentials(ai *spiceboxv1alpha1.AgentIdentity) bool {
	for _, c := range ai.Spec.Credentials {
		if c.DirectCredentials != nil {
			return true
		}
	}
	return false
}

// SetupWithManager registers the reconciler, watching AgentIdentity so that
// an identity created, updated (e.g. a directCredentials credential added),
// or deleted after the BrowserToolbox re-validates every toolbox that names
// it -- without this watch, a toolbox parked at Valid=False would only
// self-heal on the next unrelated reconcile of the BrowserToolbox itself.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&spiceboxv1alpha1.BrowserToolbox{}).
		Watches(&spiceboxv1alpha1.AgentIdentity{}, handler.EnqueueRequestsFromMapFunc(r.mapIdentityToToolboxes)).
		Complete(r)
}

// mapIdentityToToolboxes re-enqueues every BrowserToolbox whose
// spec.access.identity names the changed AgentIdentity. AgentIdentity is
// namespaced, so list in its namespace only.
func (r *Reconciler) mapIdentityToToolboxes(ctx context.Context, o client.Object) []reconcile.Request {
	var list spiceboxv1alpha1.BrowserToolboxList
	if err := r.Client.List(ctx, &list, client.InNamespace(o.GetNamespace())); err != nil {
		log.FromContext(ctx).Info("list BrowserToolboxes for AgentIdentity watch failed; dropping re-enqueue (self-heals on next resync)",
			"agentidentity", o.GetName(), "namespace", o.GetNamespace(), "err", err.Error())
		return nil
	}
	var out []reconcile.Request
	for i := range list.Items {
		if list.Items[i].Spec.Access.Identity == o.GetName() {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
		}
	}
	return out
}

// Package spiceboxtoolchain reconciles SpiceboxToolchain CRs. It validates the
// spec in isolation, then asks the named delivery Kind to validate its own
// fields, and surfaces Valid=True/False. A Valid=False toolchain is refused at
// session-bind time, so a broken catalog entry fails a session closed rather
// than silently dropping a compiler out of the sandbox.
package spiceboxtoolchain

import (
	"context"
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	apreconcile "github.com/authzed/openagentprimitives/pkg/controllers/internal/reconcile"
	tcregistry "github.com/authzed/openagentprimitives/pkg/tools/toolchain/kinds/registry"
)

// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=spiceboxtoolchains,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=spiceboxtoolchains/status,verbs=get;update;patch

// Reconciler reconciles SpiceboxToolchain objects.
type Reconciler struct {
	Client client.Client
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&spiceboxv1alpha1.SpiceboxToolchain{}).
		Complete(r)
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var tc spiceboxv1alpha1.SpiceboxToolchain
	if cont, err := apreconcile.LoadInto(ctx, r.Client, req.NamespacedName, &tc); !cont {
		return ctrl.Result{}, err
	}
	if tc.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}
	tc.Status.ObservedGeneration = tc.Generation

	validateSpec := func(ctx context.Context) apreconcile.Outcome {
		if err := spiceboxv1alpha1.ValidateToolchainSpec(tc.Name, tc.Spec); err != nil {
			conditions.SetFalse(&tc, &tc.Status.Conditions,
				spiceboxv1alpha1.SpiceboxToolchainConditionValid,
				spiceboxv1alpha1.ReasonToolchainInvalidSpec, err.Error())
			return apreconcile.StopAfter()
		}
		return apreconcile.Continue()
	}

	validateDeliveryKind := func(ctx context.Context) apreconcile.Outcome {
		k, ok := tcregistry.ByKind(tc.Spec.Source.Kind)
		if !ok {
			conditions.SetFalse(&tc, &tc.Status.Conditions,
				spiceboxv1alpha1.SpiceboxToolchainConditionValid,
				spiceboxv1alpha1.ReasonToolchainUnknownKind,
				fmt.Sprintf("source.kind %q is not registered (known: %v)",
					tc.Spec.Source.Kind, tcregistry.Names()))
			return apreconcile.StopAfter()
		}
		if err := k.Validate(toMount(&tc)); err != nil {
			conditions.SetFalse(&tc, &tc.Status.Conditions,
				spiceboxv1alpha1.SpiceboxToolchainConditionValid,
				spiceboxv1alpha1.ReasonToolchainInvalidSource, err.Error())
			return apreconcile.StopAfter()
		}
		return apreconcile.Continue()
	}

	finalSuccess := func(ctx context.Context) apreconcile.Outcome {
		conditions.SetTrue(&tc, &tc.Status.Conditions,
			spiceboxv1alpha1.SpiceboxToolchainConditionValid,
			spiceboxv1alpha1.ReasonToolchainValid)
		return apreconcile.Continue()
	}

	phases := []apreconcile.Phase{validateSpec, validateDeliveryKind, finalSuccess}
	return ctrl.Result{}, apreconcile.RunPhases(ctx, r.Client, &tc, phases)
}

// toMount projects a CR into the resolved shape the delivery Kind validates.
// Env is left unexpanded here: ValidateToolchainSpec already proved it expands,
// and the Kind does not inspect Env.
func toMount(tc *spiceboxv1alpha1.SpiceboxToolchain) spiceboxv1alpha1.ToolchainMount {
	return spiceboxv1alpha1.ToolchainMount{
		Name:       tc.Name,
		SourceKind: tc.Spec.Source.Kind,
		Image:      tc.Spec.Source.Image,
		Prefix:     tc.Spec.Source.Prefix,
		Bin:        tc.Spec.Bin,
		SizeBytes:  tc.Spec.SizeBytes,
	}
}

// ToMount is the exported projection used by the SpiceboxSession reconciler to
// freeze a resolved toolchain. Env is expanded against the toolchain's own root
// and the shared cache path.
func ToMount(tc *spiceboxv1alpha1.SpiceboxToolchain) (spiceboxv1alpha1.ToolchainMount, error) {
	m := toMount(tc)
	env, err := spiceboxv1alpha1.ExpandToolchainEnv(
		tc.Spec.Env,
		spiceboxv1alpha1.ToolchainRootFor(tc.Name),
		spiceboxv1alpha1.ToolchainCachePath,
	)
	if err != nil {
		return spiceboxv1alpha1.ToolchainMount{}, err
	}
	m.Env = env
	return m, nil
}

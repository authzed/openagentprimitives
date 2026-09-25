// pkg/controllers/agentclass/builderclass.go
//
// Two agent-builder rules (spec §1.0, §1.2) that are decidable from the
// class plus the cluster tier's settings alone — no session, no Workshop CR,
// no sidecar involved.
package agentclass

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/settingswiring"
)

// validateBuilderClass enforces:
//
//  1. A class the cluster tier SANCTIONS as a builder
//     (SettingsLimits.BuilderClassFor non-nil for this ns/name) must declare
//     an explicit starter allowlist. "Owner must equal starter" and "no
//     interactPermission on a builder" are deferred to the builder-bundle
//     plan, where the class actually ships with those fields authored —
//     this is the one rule the class + cluster settings alone can decide.
//  2. A class OUTSIDE a workshop namespace must not reference a
//     workshop-labeled cluster-scoped SpiceboxToolspec (carrying
//     spiceboxv1alpha1.LabelWorkshopNamespace). Walks
//     spec.toolBundles[].toolspecs; a class INSIDE the labeled namespace
//     referencing its own workshop's tool is fine — that is the builder
//     testing what it authored. This is the consumer half of the webhook's
//     prefix/label rule (Task 8 stamps the label; this reads it), closing
//     the loop RBAC alone cannot: it cannot scope "create by name", so a
//     workshop-authored tool must instead be refused by every consumer
//     outside its build space.
//
// Same (result, invalid, err) shape as validateStartGate: the caller returns
// immediately whenever invalid || err != nil.
func (r *Reconciler) validateBuilderClass(ctx context.Context, ac *spiceboxv1alpha1.AgentClass) (ctrl.Result, bool, error) {
	cluster, _, err := settingswiring.FetchTiers(ctx, r.Client, ac.Namespace)
	if err != nil {
		return ctrl.Result{}, false, fmt.Errorf("validateBuilderClass: fetch settings tiers: %w", err)
	}
	var limits *spiceboxv1alpha1.SettingsLimits
	if cluster != nil {
		limits = cluster.Limits
	}
	if limits.BuilderClassFor(ac.Namespace, ac.Name) != nil &&
		len(ac.Spec.GetAuthz().GetSession().AllowedStarters) == 0 {
		res, rerr := r.setInvalid(ctx, ac, spiceboxv1alpha1.ReasonBuilderClassInvalid,
			"a sanctioned builder agent must list who may start it")
		return res, true, rerr
	}

	for _, b := range ac.Spec.ToolBundles {
		for _, tsName := range b.Toolspecs {
			var ts spiceboxv1alpha1.SpiceboxToolspec
			if err := r.Client.Get(ctx, types.NamespacedName{Name: tsName}, &ts); err != nil {
				if apierrors.IsNotFound(err) {
					// Not this rule's concern: a missing toolspec is already
					// gated by validateBundles earlier in the reconcile.
					continue
				}
				return ctrl.Result{}, false, fmt.Errorf("validateBuilderClass: get toolspec %q: %w", tsName, err)
			}
			ws, labeled := ts.Labels[spiceboxv1alpha1.LabelWorkshopNamespace]
			if labeled && ws != ac.Namespace {
				res, rerr := r.setInvalid(ctx, ac, spiceboxv1alpha1.ReasonReferencesWorkshopTool,
					"this agent references a tool that belongs to a build space and cannot be used outside it")
				return res, true, rerr
			}
		}
	}
	return ctrl.Result{}, false, nil
}

// Package guardian's spiceboxtoolkit_fragment_validation.go isolates a bad
// SpiceboxToolkit.spec.spicedbSchema fragment to its own SpiceboxToolkit's
// status rather than letting it fail schema composition for the whole
// cluster — the SpiceboxToolkit mirror of mcpserver_fragment_validation.go
// and sidecartoolbox_fragment_validation.go. A SpiceboxToolkit CR is
// installed, so it is tenant input like MCPServer and SidecarToolbox, and
// gets the same two-stage isolation (ValidateFragment +
// PartitionCompatibleFragments); this file stamps the per-CR
// SpiceDBSchemaValid condition for the rejected ones. The //go:embed'ed
// built-in toolkits (toolkits/*.yaml) are a different, compile-time set and
// are not covered here — see agentsessiongrants_controller.go's Reconcile.
package guardian

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
)

// invalidSpiceboxToolkitFragment pairs a SpiceboxToolkit with the reason +
// error for why its spec.spicedbSchema fragment was excluded from the
// compose pass: either FragmentInvalid (invalid on its own) or
// FragmentConflict (valid alone but collides with an already-accepted
// fragment).
type invalidSpiceboxToolkitFragment struct {
	toolkit *spiceboxv1alpha1.SpiceboxToolkit
	reason  string
	err     error
}

// patchSpiceboxToolkitSchemaValidity stamps the guardian-owned
// SpiceDBSchemaValid condition on every SpiceboxToolkit whose fragment
// validation outcome needs to change: False for each entry in bad, and True
// (FragmentValid) for any OTHER toolkit that currently carries a
// (now-stale) condition — a previously-bad fragment that has since been
// fixed. A toolkit whose fragment has always been valid is left untouched
// (no condition added just to say "fine").
//
// As with the MCPServer and SidecarToolbox variants, this condition is
// distinct from SpiceboxToolkitConditionValid (owned by
// pkg/controllers/spiceboxtoolkit); two controllers patching different
// condition types on one Conditions slice is a narrow self-healing
// stale-read race, far better than a bad fragment silently freezing every
// session's schema.
func (r *Reconciler) patchSpiceboxToolkitSchemaValidity(
	ctx context.Context,
	all []spiceboxv1alpha1.SpiceboxToolkit,
	bad []invalidSpiceboxToolkitFragment,
) {
	badByKey := make(map[string]invalidSpiceboxToolkitFragment, len(bad))
	for _, b := range bad {
		badByKey[b.toolkit.Name] = b
	}
	for i := range all {
		tk := &all[i]
		if b, isBad := badByKey[tk.Name]; isBad {
			r.patchSpiceboxToolkitSchemaValidCondition(ctx, tk, false, b.reason, b.err.Error())
			continue
		}
		if conditions.Find(tk.Status.Conditions, spiceboxv1alpha1.SpiceboxToolkitConditionSpiceDBSchemaValid) != nil {
			r.patchSpiceboxToolkitSchemaValidCondition(ctx, tk, true,
				spiceboxv1alpha1.SpiceboxToolkitReasonFragmentValid, "")
		}
	}
}

// patchSpiceboxToolkitSchemaValidCondition patches just the
// SpiceDBSchemaValid condition on one SpiceboxToolkit. conditions.Set is a
// no-op on equal state, so a repeated reconcile finding the same outcome
// produces an empty merge patch rather than churning the object.
func (r *Reconciler) patchSpiceboxToolkitSchemaValidCondition(
	ctx context.Context,
	tk *spiceboxv1alpha1.SpiceboxToolkit,
	valid bool, reason, msg string,
) {
	status := metav1.ConditionTrue
	if !valid {
		status = metav1.ConditionFalse
	}
	cp := tk.DeepCopy()
	conditions.Set(cp, &cp.Status.Conditions, metav1.Condition{
		Type:    spiceboxv1alpha1.SpiceboxToolkitConditionSpiceDBSchemaValid,
		Status:  status,
		Reason:  reason,
		Message: msg,
	})
	if err := r.Client.Status().Patch(ctx, cp, client.MergeFrom(tk)); err != nil {
		log.FromContext(ctx).Info("guardian: patch SpiceboxToolkit SpiceDBSchemaValid failed",
			"name", tk.Name, "err", err.Error())
	}
}

// Package guardian's sidecartoolbox_fragment_validation.go isolates a bad
// SidecarToolbox.spec.spicedbSchema fragment to its own SidecarToolbox's status
// rather than letting it fail schema composition for the whole cluster — the
// SidecarToolbox mirror of mcpserver_fragment_validation.go. SidecarToolbox is
// agent-authored, so its fragments get the same two-stage isolation
// (ValidateFragment + PartitionCompatibleFragments) MCPServer fragments do; this
// file stamps the per-CR SpiceDBSchemaValid condition for the rejected ones.
package guardian

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
)

// invalidSidecarToolboxFragment pairs a SidecarToolbox with the reason + error
// for why its spec.spicedbSchema fragment was excluded from the compose pass:
// either FragmentInvalid (invalid on its own) or FragmentConflict (valid alone
// but collides with an already-accepted fragment).
type invalidSidecarToolboxFragment struct {
	toolbox *spiceboxv1alpha1.SidecarToolbox
	reason  string
	err     error
}

// patchSidecarToolboxSchemaValidity stamps the guardian-owned SpiceDBSchemaValid
// condition on every SidecarToolbox whose fragment validation outcome needs to
// change: False for each entry in bad, and True (FragmentValid) for any OTHER
// toolbox that currently carries a now-stale condition — a previously-bad
// fragment that has since been fixed. A toolbox whose fragment has always been
// valid is left untouched (no condition added just to say "fine").
//
// As with the MCPServer variant, this condition is distinct from
// SidecarToolboxConditionValid/Reachable (owned by pkg/controllers/sidecartoolbox);
// two controllers patching different condition types on one Conditions slice is
// a narrow self-healing stale-read race, far better than a bad fragment silently
// freezing every session's schema.
func (r *Reconciler) patchSidecarToolboxSchemaValidity(
	ctx context.Context,
	all []spiceboxv1alpha1.SidecarToolbox,
	bad []invalidSidecarToolboxFragment,
) {
	badByKey := make(map[string]invalidSidecarToolboxFragment, len(bad))
	for _, b := range bad {
		badByKey[b.toolbox.Namespace+"/"+b.toolbox.Name] = b
	}
	for i := range all {
		tb := &all[i]
		key := tb.Namespace + "/" + tb.Name
		if b, isBad := badByKey[key]; isBad {
			r.patchSidecarToolboxSchemaValidCondition(ctx, tb, false, b.reason, b.err.Error())
			continue
		}
		if conditions.Find(tb.Status.Conditions, spiceboxv1alpha1.SidecarToolboxConditionSpiceDBSchemaValid) != nil {
			r.patchSidecarToolboxSchemaValidCondition(ctx, tb, true,
				spiceboxv1alpha1.SidecarToolboxReasonFragmentValid, "")
		}
	}
}

// patchSidecarToolboxSchemaValidCondition patches just the SpiceDBSchemaValid
// condition on one SidecarToolbox. conditions.Set is a no-op on equal state, so
// a repeated reconcile finding the same outcome produces an empty merge patch
// rather than churning the object.
func (r *Reconciler) patchSidecarToolboxSchemaValidCondition(
	ctx context.Context,
	tb *spiceboxv1alpha1.SidecarToolbox,
	valid bool, reason, msg string,
) {
	status := metav1.ConditionTrue
	if !valid {
		status = metav1.ConditionFalse
	}
	cp := tb.DeepCopy()
	conditions.Set(cp, &cp.Status.Conditions, metav1.Condition{
		Type:    spiceboxv1alpha1.SidecarToolboxConditionSpiceDBSchemaValid,
		Status:  status,
		Reason:  reason,
		Message: msg,
	})
	if err := r.Client.Status().Patch(ctx, cp, client.MergeFrom(tb)); err != nil {
		log.FromContext(ctx).Info("guardian: patch SidecarToolbox SpiceDBSchemaValid failed",
			"name", tb.Namespace+"/"+tb.Name, "err", err.Error())
	}
}

// Package guardian's mcpserver_fragment_validation.go isolates a bad
// MCPServer.spec.spiceDBSchema fragment to its own MCPServer's status
// rather than letting it fail schema composition for the whole cluster.
// See agentsessiongrants_controller.go's Reconcile for how the partition
// into good/bad fragments feeds guardianschema.RunAll.
package guardian

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
)

// invalidMCPServerFragment pairs an MCPServer with the reason + error for
// why its spec.spiceDBSchema fragment was excluded from the compose pass:
// either FragmentInvalid (invalid on its own, from
// guardianschema.ValidateFragment) or FragmentConflict (valid alone but
// collides with an already-accepted fragment, from
// guardianschema.PartitionCompatibleFragments).
type invalidMCPServerFragment struct {
	mcpServer *spiceboxv1alpha1.MCPServer
	reason    string
	err       error
}

// patchMCPServerSchemaValidity stamps the guardian-owned
// SpiceDBSchemaValid condition on every MCPServer whose fragment
// validation outcome needs to change: False/FragmentInvalid for each
// entry in bad, and True/FragmentValid for any OTHER MCPServer that
// currently carries a (now-stale) SpiceDBSchemaValid condition — i.e. a
// previously-bad fragment that has since been fixed. An MCPServer whose
// fragment has always been valid is left untouched entirely (no
// condition is added just to say "fine").
//
// This condition is intentionally distinct from MCPServerConditionValid/
// Reachable, which pkg/controllers/mcpserver owns and patches
// independently. Two controllers patching DIFFERENT condition types on
// the same Conditions slice via client.MergeFrom is still a narrow
// stale-read race (a merge patch replaces the whole array value), but
// it self-heals on the next reconcile of either controller and is far
// better than the alternative this fix replaces: a bad fragment
// silently freezing every tenant's AgentSessionGrants.
func (r *Reconciler) patchMCPServerSchemaValidity(
	ctx context.Context,
	all []spiceboxv1alpha1.MCPServer,
	bad []invalidMCPServerFragment,
) {
	badByKey := make(map[string]invalidMCPServerFragment, len(bad))
	for _, b := range bad {
		badByKey[b.mcpServer.Namespace+"/"+b.mcpServer.Name] = b
	}
	for i := range all {
		ms := &all[i]
		key := ms.Namespace + "/" + ms.Name
		if b, isBad := badByKey[key]; isBad {
			r.patchMCPServerSchemaValidCondition(ctx, ms, false, b.reason, b.err.Error())
			continue
		}
		if conditions.Find(ms.Status.Conditions, spiceboxv1alpha1.MCPServerConditionSpiceDBSchemaValid) != nil {
			r.patchMCPServerSchemaValidCondition(ctx, ms, true,
				spiceboxv1alpha1.MCPServerReasonFragmentValid, "")
		}
	}
}

// patchMCPServerSchemaValidCondition patches just the
// SpiceDBSchemaValid condition on a single MCPServer. conditions.Set is
// a no-op on equal state (same Status/Reason/Message), so a repeated
// reconcile that finds the same outcome produces a byte-identical
// Patch — an empty merge patch — rather than churning the object on
// every pass.
func (r *Reconciler) patchMCPServerSchemaValidCondition(
	ctx context.Context,
	ms *spiceboxv1alpha1.MCPServer,
	valid bool, reason, msg string,
) {
	status := metav1.ConditionTrue
	if !valid {
		status = metav1.ConditionFalse
	}
	cp := ms.DeepCopy()
	conditions.Set(cp, &cp.Status.Conditions, metav1.Condition{
		Type:    spiceboxv1alpha1.MCPServerConditionSpiceDBSchemaValid,
		Status:  status,
		Reason:  reason,
		Message: msg,
	})
	if err := r.Client.Status().Patch(ctx, cp, client.MergeFrom(ms)); err != nil {
		log.FromContext(ctx).Info("guardian: patch MCPServer SpiceDBSchemaValid failed",
			"name", ms.Namespace+"/"+ms.Name, "err", err.Error())
	}
}

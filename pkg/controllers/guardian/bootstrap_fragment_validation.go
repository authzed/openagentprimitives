// Package guardian's bootstrap_fragment_validation.go isolates a bad
// SpiceDBBootstrap.spec.spicedbSchema fragment to its own SpiceDBBootstrap's
// status rather than letting it fail schema composition for the whole
// cluster — the SpiceDBBootstrap mirror of mcpserver_fragment_validation.go,
// sidecartoolbox_fragment_validation.go and spiceboxtoolkit_fragment_validation.go.
// A SpiceDBBootstrap CR gets the same two-stage isolation (ValidateFragment +
// PartitionCompatibleFragments) as those three; see
// agentsessiongrants_controller.go's Reconcile for where its candidates are
// built and the partition run.
//
// Unlike the other three, SpiceDBBootstrap does NOT get a new parallel
// condition. It already has a SchemaIncluded condition (and a
// ReasonSpicedbSchemaConflict reason, previously used only by the blanket
// all-or-nothing check bootstrap_sync.go's validateBootstraps used to run)
// that patchBootstrapStatus stamps every non-debounced reconcile based on
// whether the cluster-wide compose succeeded. This file only stamps the
// REJECTED half, and only False — the accepted/healed-back-to-True half is
// already patchBootstrapStatus's job on the very next non-debounced pass, so
// duplicating a self-heal loop here would just race a second Patch call
// against it. patchBootstrapStatus's own exclusion-set guard is what stops
// it from clobbering the False this file stamps with a misleading True
// before the accepted subset's compose outcome is known.
package guardian

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
)

// invalidBootstrapFragment pairs a SpiceDBBootstrap with the reason + error
// for why its spec.spicedbSchema fragment was excluded from the compose
// pass: either invalid on its own (from guardianschema.ValidateFragment,
// reason ReasonSpicedbSchemaFragmentInvalid) or valid alone but colliding
// with an already-accepted fragment (from
// guardianschema.PartitionCompatibleFragments, reason
// ReasonSpicedbSchemaConflict). Unlike MCPServer, SidecarToolbox and
// SpiceboxToolkit, both reasons land on the SAME condition
// (SchemaIncluded) rather than a dedicated SpiceDBSchemaValid — see the
// package doc comment above for why.
type invalidBootstrapFragment struct {
	boot   *spiceboxv1alpha1.SpiceDBBootstrap
	reason string
	err    error
}

// patchBootstrapFragmentValidity stamps SchemaIncluded=False on every
// SpiceDBBootstrap in bad, landing the isolation outcome promptly —
// regardless of the debounce gate later in Reconcile — so an operator
// fixing a bad or conflicting fragment sees the rejection without waiting
// on an unrelated schema write to happen to fire. Mirrors why
// patchMCPServerSchemaValidity / patchSidecarToolboxSchemaValidity /
// patchSpiceboxToolkitSchemaValidity are called before that same gate.
//
// Called AFTER validateBootstraps (bootstrap_sync.go), which patches the
// SAME CR's Valid condition earlier in the same Reconcile pass. Re-Gets each
// bootstrap immediately before diffing rather than reusing the pre-reconcile
// `b.boot` snapshot: a CRD status patch is a JSON Merge Patch, which
// replaces an array field (Status.Conditions) wholesale rather than merging
// per-element, so diffing against the stale snapshot would resend it
// without validateBootstraps' just-landed Valid condition and silently
// revert it. Mirrors patchBootstrapStatus's own re-Get, same hazard.
func (r *Reconciler) patchBootstrapFragmentValidity(ctx context.Context, bad []invalidBootstrapFragment) {
	for _, b := range bad {
		fresh := &spiceboxv1alpha1.SpiceDBBootstrap{}
		base := b.boot
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(b.boot), fresh); err != nil {
			log.FromContext(ctx).Info("guardian: re-Get before patch SpiceDBBootstrap SchemaIncluded failed; patching from stale snapshot",
				"name", b.boot.Namespace+"/"+b.boot.Name, "err", err.Error())
		} else {
			base = fresh
		}
		cp := base.DeepCopy()
		conditions.Set(cp, &cp.Status.Conditions, metav1.Condition{
			Type:    spiceboxv1alpha1.SpiceDBBootstrapConditionSchemaIncluded,
			Status:  metav1.ConditionFalse,
			Reason:  b.reason,
			Message: b.err.Error(),
		})
		if err := r.Client.Status().Patch(ctx, cp, client.MergeFrom(base)); err != nil {
			log.FromContext(ctx).Info("guardian: patch SpiceDBBootstrap SchemaIncluded failed",
				"name", b.boot.Namespace+"/"+b.boot.Name, "err", err.Error())
		}
	}
}

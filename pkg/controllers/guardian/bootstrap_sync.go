// pkg/controllers/guardian/bootstrap_sync.go
//
// Helpers that wire SpiceDBBootstrap relationship sync into the
// Reconciler. Split out of agentsessiongrants_controller.go so the
// Reconcile method stays scannable; everything in this file is
// receiver methods or free helpers on the same package.
package guardian

import (
	"context"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	guardianschema "github.com/authzed/openagentprimitives/pkg/authz/guardian/schema"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
)

// validateBootstraps stamps Valid on every SpiceDBBootstrap and returns (a)
// the set of CR keys ("ns/name") that failed schema/shape validation (to
// skip entirely when building the desired map) and (b) for every OTHER CR,
// the ownership-refusal messages for any relationship another
// relsource.Source claims (to surface on RelationshipsApplied — see
// patchBootstrapStatus — without skipping the CR's other relationships).
// Idempotent: meta.SetStatusCondition is a no-op on equal state, so
// repeated reconciles do not churn the LastTransitionTime.
//
// This used to also run ValidateSchemaConflict over the union of ALL
// contributing fragments and, on conflict, stamp Valid=False on EVERY
// bootstrap that contributed a fragment — including a good operator-authored
// one sharing the reconcile with a bad or hostile one. That cross-CR check
// is now handled per-CR, before this function runs, by the same
// ValidateFragment + PartitionCompatibleFragments isolation MCPServer,
// SidecarToolbox and SpiceboxToolkit fragments go through (see
// agentsessiongrants_controller.go's Reconcile and
// bootstrap_fragment_validation.go), which excludes only the fragment(s)
// that actually lose a conflict rather than every contributor.
//
// Uses validateShape, not ValidateSpec, for the whole-CR Valid gate:
// ValidateSpec's single verdict also covers ownership, which must NOT
// invalidate the whole CR — see validateShape's doc comment
// (bootstrap_validation.go) for why the write path's own per-tuple refusal
// would otherwise disagree with this gate in EFFECT, not just verdict.
func (r *Reconciler) validateBootstraps(
	ctx context.Context,
	boots []spiceboxv1alpha1.SpiceDBBootstrap,
) (invalid map[string]struct{}, ownershipRefusals map[string][]string) {
	invalid = map[string]struct{}{}
	ownershipRefusals = map[string][]string{}
	logger := log.FromContext(ctx).WithName("guardian.bootstrap.validate")
	for i := range boots {
		b := &boots[i]
		key := b.Namespace + "/" + b.Name
		if reason, msg := validateShape(b); reason != "" {
			r.patchBootstrapValid(ctx, b, false, reason, msg)
			invalid[key] = struct{}{}
			continue
		}
		r.patchBootstrapValid(ctx, b, true, "AllChecksPassed", "")

		if refused := relationshipOwnershipRefusals(b); len(refused) > 0 {
			// Already deterministic (relationship-declaration order) — see
			// relationshipOwnershipRefusals' own doc comment on why no sort is
			// needed here now that it returns a slice instead of a map.
			ownershipRefusals[key] = refused
			logger.Info("validateBootstraps: relationship(s) owned by another source; the write path will refuse only those",
				"cr", key, "refused", len(refused))
		}
	}
	if len(invalid) > 0 {
		logger.Info("validateBootstraps: some CRs invalid", "invalid", len(invalid))
	}
	return invalid, ownershipRefusals
}

// seedPriorClaimsForDeleting recovers, into r.lastDesired, the tuple claims of
// CRs that are mid-deletion but that THIS process never observed alive.
//
// The reap path is a drop-out: buildNewDesired skips deleting CRs, so ComputeDiff
// sees their tuples vanish from `new` while still present in `last`. But `last`
// is r.lastDesired, an IN-PROCESS snapshot, and status persists only
// ObservedRelationships — a count, not the tuple set. An operator restart between
// the delete and the finalizer removal therefore loses the claims entirely:
// ToDelete comes out empty, applyDeletes reports no failures, the finalizer is
// stripped, and the CR is GC'd while its reclaimPolicy=Delete tuples stay in
// SpiceDB with nothing referencing them and nothing left to reap them.
//
// The durable record needed is already present: the finalizer is exactly what
// keeps a deleting CR and its spec readable, and ResolveTuple makes the tuple set
// a pure function of that spec. Only CRs carrying this controller's finalizer are
// seeded, since that finalizer marks the tuples it once claimed.
//
// Best-effort by construction: a spec edited between the last successful apply
// and the delete resolves to the EDITED tuples, so pre-edit ones can still be
// missed — strictly better than reaping nothing. The normal path is untouched:
// when this process saw the CR alive, lastDesired already holds its exact claims
// and seeding is skipped.
//
// A relationship another relsource.Source claims is excluded from the
// recovery, the same way applyTouches never lets a refused TOUCH land in
// lastDesired (see agentsessiongrants_controller.go's Reconcile, step 7):
// this function reconstructs "what this process believes it wrote" from the
// spec ALONE, and a refused relationship was never written by this
// component, restart or not. Without this exclusion, a CR whose sole
// relationship is ownership-refused — which the Major fix keeps out of
// lastDesired on every ordinary pass — looks, from OwnedBy's point of view,
// exactly like one this process never saw alive, so it gets seeded right
// back in on the very reconcile that deletes it, recreating the DELETE
// wedge that fix exists to close (its TestBootstrap_RefusedRelationship_
// DeletionCompletes exercises exactly this).
func (r *Reconciler) seedPriorClaimsForDeleting(ctx context.Context, boots []spiceboxv1alpha1.SpiceDBBootstrap) {
	if r.lastDesired == nil {
		r.lastDesired = NewDesiredMap()
	}
	logger := log.FromContext(ctx).WithName("guardian.bootstrap.reclaim")
	for i := range boots {
		b := &boots[i]
		if b.DeletionTimestamp == nil || len(b.Spec.Relationships) == 0 {
			continue
		}
		if !controllerutil.ContainsFinalizer(b, spiceboxv1alpha1.FinalizerSpiceDBBootstrap) {
			continue
		}
		key := b.Namespace + "/" + b.Name
		if len(r.lastDesired.OwnedBy(key)) > 0 {
			continue // this process already knows what the CR claimed
		}
		policy := b.Spec.ReclaimPolicy
		if policy == "" {
			policy = spiceboxv1alpha1.SpiceDBBootstrapReclaimDelete
		}
		var seeded int
		for j, rel := range b.Spec.Relationships {
			if _, refused := validateRelationshipOwnership(j, rel); refused != "" {
				continue // never written by this component; do not recover it as though it were
			}
			r.lastDesired.Add(ResolveTuple(rel), Owner{CR: key, Policy: policy})
			seeded++
		}
		if seeded > 0 {
			logger.Info("recovered a deleting CR's tuple claims from its spec",
				"cr", key, "relationships", seeded, "reclaimPolicy", policy)
		}
	}
}

// buildNewDesired iterates valid SpiceDBBootstrap CRs and returns the
// union of their relationships as a DesiredMap. invalidCRs is a set of
// CR keys ("ns/name") to skip from claiming *new* tuples — but their
// already-applied tuples are carried forward from r.lastDesired so an
// operator typo that flips a CR Valid=True → False does not silently
// reap live SpiceDB state (spec §4.4: "L2 validation fails → Valid=
// False, no SpiceDB I/O").
func (r *Reconciler) buildNewDesired(
	boots []spiceboxv1alpha1.SpiceDBBootstrap,
	invalidCRs map[string]struct{},
) *DesiredMap {
	d := NewDesiredMap()
	for i := range boots {
		b := &boots[i]
		// Deleting CRs no longer claim their tuples — the finalizer
		// flow relies on this drop-out to let ComputeDiff add their
		// sole-owned Delete-policy tuples to ToDelete on the very
		// reconcile that observes DeletionTimestamp.
		if b.DeletionTimestamp != nil {
			continue
		}
		key := b.Namespace + "/" + b.Name
		if _, bad := invalidCRs[key]; bad {
			// Keep the CR's PREVIOUSLY-applied tuples so the next
			// ComputeDiff sees no transition to to_delete. We look up
			// tuples by owning CR in lastDesired (NOT by re-resolving
			// the current spec) because an invalidating edit may have
			// changed the tuple shape — e.g., flipping canonicalize=true
			// on a non-email subject changes the subject id, so the
			// re-resolved key wouldn't match what's already in lastDesired.
			if r != nil && r.lastDesired != nil {
				for _, prior := range r.lastDesired.OwnedBy(key) {
					d.Add(prior.Tuple, prior.Owner)
				}
			}
			continue
		}
		policy := b.Spec.ReclaimPolicy
		if policy == "" {
			policy = spiceboxv1alpha1.SpiceDBBootstrapReclaimDelete
		}
		for _, rel := range b.Spec.Relationships {
			tup := ResolveTuple(rel)
			d.Add(tup, Owner{CR: key, Policy: policy})
		}
	}
	return d
}

// applyDeletes runs DELETE for each tuple and returns the set of tuple
// keys whose delete failed (so the caller can re-claim them in
// lastDesired for the next reconcile to retry) plus the failure count
// (kept for the existing status-patcher contract). Logs every error at
// INFO with the tuple key (AGENTS.md "never silently drop errors").
// Returns an empty set + 0 when the Writer is nil so the controller
// degrades to a schema-only no-op without panicking.
func (r *Reconciler) applyDeletes(ctx context.Context, tuples []spicedb.Tuple) (map[string]struct{}, int) {
	failedKeys := map[string]struct{}{}
	if r.Writer == nil || len(tuples) == 0 {
		return failedKeys, 0
	}
	logger := log.FromContext(ctx).WithName("guardian.bootstrap.delete")
	for _, t := range tuples {
		if err := spicedb.DeleteBootstrapRelationshipVia(ctx, r.Writer, t); err != nil {
			logger.Info("delete failed", "tuple", t.Key(), "err", err.Error())
			failedKeys[t.Key()] = struct{}{}
		}
	}
	logger.Info("applyDeletes complete", "attempted", len(tuples), "failed", len(failedKeys))
	return failedKeys, len(failedKeys)
}

// applyTouches runs TOUCH for each tuple and returns the set of
// successfully-applied tuple keys plus the count of failures. When
// the Writer is nil every tuple is reported as a failure so the
// per-CR RelationshipsApplied status reflects SpiceDBUnavailable
// rather than "applied".
func (r *Reconciler) applyTouches(
	ctx context.Context,
	tuples []spicedb.Tuple,
) (applied map[string]struct{}, failed int) {
	applied = map[string]struct{}{}
	if r.Writer == nil {
		return applied, len(tuples)
	}
	if len(tuples) == 0 {
		return applied, 0
	}
	logger := log.FromContext(ctx).WithName("guardian.bootstrap.touch")
	for _, t := range tuples {
		if err := spicedb.TouchBootstrapRelationshipVia(ctx, r.Writer, t); err != nil {
			logger.Info("touch failed", "tuple", t.Key(), "err", err.Error())
			failed++
			continue
		}
		applied[t.Key()] = struct{}{}
	}
	logger.Info("applyTouches complete",
		"attempted", len(tuples), "applied", len(applied), "failed", failed)
	return applied, failed
}

// patchBootstrapValid patches just the Valid condition on a single
// SpiceDBBootstrap. Other conditions are left alone. Errors are logged
// at INFO (AGENTS.md "never silently drop errors") rather than
// returned: a status-patch failure here is independent of the rest of
// the reconcile and should not abort the relationship sync for other
// CRs.
func (r *Reconciler) patchBootstrapValid(
	ctx context.Context,
	b *spiceboxv1alpha1.SpiceDBBootstrap,
	valid bool, reason, msg string,
) {
	cp := b.DeepCopy()
	status := metav1.ConditionTrue
	if !valid {
		status = metav1.ConditionFalse
	}
	conditions.Set(cp, &cp.Status.Conditions, metav1.Condition{
		Type:    spiceboxv1alpha1.SpiceDBBootstrapConditionValid,
		Status:  status,
		Reason:  reason,
		Message: msg,
	})
	cp.Status.ObservedGeneration = b.Generation
	if err := r.Client.Status().Patch(ctx, cp, client.MergeFrom(b)); err != nil {
		log.FromContext(ctx).Info("patch SpiceDBBootstrap Valid failed",
			"name", b.Namespace+"/"+b.Name, "err", err.Error())
	}
}

// patchBootstrapStatus stamps SchemaIncluded + RelationshipsApplied on
// every SpiceDBBootstrap, based on the reconcile outcome. Called once
// per Reconcile after the schema write + relationship sync complete.
//
// SchemaIncluded is set only on CRs that actually contributed a
// fragment; aggregate True/False per-CR is sufficient for the slice
// goal (per-CR skipped-pair attribution is out of scope). A CR whose key
// is in fragmentExcluded is skipped entirely: its fragment was rejected by
// the isolation pass earlier this reconcile (patchBootstrapFragmentValidity,
// bootstrap_fragment_validation.go), which already stamped the specific
// SchemaIncluded=False/ReasonSpicedbSchemaConflict for it. RunAll's schemaOK
// here reflects the ACCEPTED subset's compose outcome, which says nothing
// about a fragment that never reached it — stamping True over the
// isolation's False would be wrong, and stamping schemaOK's False with a
// generic SchemaWriteFailed reason would overwrite a more specific one.
//
// RelationshipsApplied is set only on CRs that declared relationships.
// The reason is one of:
//   - AllTouched: every spec relationship landed AND no delete or
//     touch failures across the whole reconcile.
//   - RelationOwnedByAnotherSource: some tuples landed but at least one of
//     the ones that didn't is a relationship validateBootstraps found
//     ownership-refused (ownershipRefusals) — more specific than a bare
//     PartialApply because the cause is known, not merely "not everything
//     landed". The message still carries the count.
//   - PartialApply: some tuples landed but others failed for a reason other
//     than (or in addition to) ownership — e.g. a transient write error.
//   - SpiceDBUnavailable: the Writer is nil (operator misconfigured).
func (r *Reconciler) patchBootstrapStatus(
	ctx context.Context,
	boots []spiceboxv1alpha1.SpiceDBBootstrap,
	fragmentExcluded map[string]struct{},
	ownershipRefusals map[string][]string,
	newDesired *DesiredMap,
	applied map[string]struct{},
	deleteFailures, touchFailures int,
	res guardianschema.Result,
	runErr error,
) {
	_ = newDesired // reserved for per-CR ObservedRelationships refinement
	_ = res        // reserved for future per-CR skipped-pair attribution
	schemaOK := runErr == nil
	for i := range boots {
		b := &boots[i]
		// Skip status churn on CRs about to disappear — their
		// relationships were already skipped from newDesired by
		// buildNewDesired, so they'd flip to PartialApply just before
		// the finalizer is removed. Visible noise in `kubectl get -w`.
		if b.DeletionTimestamp != nil {
			continue
		}
		// Re-Get the CR before computing the MergeFrom diff. The slice
		// of `boots` we were handed was listed BEFORE validateBootstraps
		// stamped Valid, so its Status.Conditions is stale. A
		// MergeFrom(b) diff would compute a delta that REPLACES the
		// conditions list (CRD types use JSON merge patch, not strategic
		// merge patch, so a per-element merge isn't available) — and
		// that wipes the Valid condition we just stamped. Re-Getting
		// pulls the just-patched state so the diff is incremental.
		fresh := &spiceboxv1alpha1.SpiceDBBootstrap{}
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(b), fresh); err != nil {
			log.FromContext(ctx).Info("re-Get before status patch failed",
				"name", b.Namespace+"/"+b.Name, "err", err.Error())
			continue
		}
		cp := fresh.DeepCopy()
		cp.Status.ObservedGeneration = b.Generation
		// Reset ObservedRelationships so a CR that drops all its
		// relationships from spec doesn't keep a stale prior count.
		// Overwritten below if the CR currently declares any.
		cp.Status.ObservedRelationships = 0

		// SchemaIncluded: only stamp on CRs that actually declared a fragment,
		// and only on ones the isolation pass did NOT already reject — a CR
		// in fragmentExcluded already has its False landed by
		// patchBootstrapFragmentValidity this reconcile, and must not be
		// overwritten here (see the doc comment above).
		//
		// "declared a fragment" must be RawZed-aware — matching the candidate-
		// gathering predicate in Reconcile — or a RawZed-only bootstrap that DID
		// compose (it reached RunAll and landed) would still get no
		// SchemaIncluded condition here, the exact silent-drop this stamp
		// exists to prevent.
		_, excluded := fragmentExcluded[b.Namespace+"/"+b.Name]
		hasFragment := b.Spec.SpiceDBSchema != nil &&
			(len(b.Spec.SpiceDBSchema.Resources) > 0 || b.Spec.SpiceDBSchema.RawZed != "")
		if !excluded && hasFragment {
			if schemaOK {
				conditions.SetTrue(cp, &cp.Status.Conditions,
					spiceboxv1alpha1.SpiceDBBootstrapConditionSchemaIncluded,
					"FragmentLanded")
			} else {
				conditions.SetFalse(cp, &cp.Status.Conditions,
					spiceboxv1alpha1.SpiceDBBootstrapConditionSchemaIncluded,
					spiceboxv1alpha1.ReasonSchemaWriteFailed, runErr.Error())
			}
		}

		// RelationshipsApplied: only stamp on CRs that declared
		// relationships. The success/partial decision is per-CR — we
		// look at whether THIS CR's tuples all landed, not the global
		// failure totals. (A CR with all-successful tuples must not be
		// downgraded to PartialApply because some unrelated CR failed.)
		if len(b.Spec.Relationships) > 0 {
			var appliedForCR int32
			for _, rel := range b.Spec.Relationships {
				tup := ResolveTuple(rel)
				if _, ok := applied[tup.Key()]; ok {
					appliedForCR++
				}
			}
			cp.Status.ObservedRelationships = appliedForCR
			total := int32(len(b.Spec.Relationships))
			switch {
			case r.Writer == nil:
				conditions.SetFalse(cp, &cp.Status.Conditions,
					spiceboxv1alpha1.SpiceDBBootstrapConditionRelationshipsApplied,
					spiceboxv1alpha1.ReasonSpiceDBUnavailable,
					"no SpiceDB writer configured")
			case appliedForCR == total:
				conditions.SetTrue(cp, &cp.Status.Conditions,
					spiceboxv1alpha1.SpiceDBBootstrapConditionRelationshipsApplied,
					spiceboxv1alpha1.ReasonAllTouched)
			default:
				if refused := ownershipRefusals[b.Namespace+"/"+b.Name]; len(refused) > 0 {
					conditions.SetFalse(cp, &cp.Status.Conditions,
						spiceboxv1alpha1.SpiceDBBootstrapConditionRelationshipsApplied,
						spiceboxv1alpha1.ReasonRelationOwnedByAnotherSource,
						fmt.Sprintf("%d/%d tuples applied; %s", appliedForCR, total, strings.Join(refused, "; ")))
				} else {
					conditions.SetFalse(cp, &cp.Status.Conditions,
						spiceboxv1alpha1.SpiceDBBootstrapConditionRelationshipsApplied,
						spiceboxv1alpha1.ReasonPartialApply,
						fmt.Sprintf("%d/%d tuples applied", appliedForCR, total))
				}
			}
		}

		if err := r.Client.Status().Patch(ctx, cp, client.MergeFrom(fresh)); err != nil {
			log.FromContext(ctx).Info("patch SpiceDBBootstrap status failed",
				"name", b.Namespace+"/"+b.Name, "err", err.Error())
		}
	}
}

// removeFinalizersOnDeleted removes the SpiceDBBootstrap finalizer from
// every CR with DeletionTimestamp != nil — but only when the reconcile
// had no delete failures. A non-zero deleteFailures count means at
// least one tuple delete (possibly for a different CR) is still
// pending retry; holding the finalizer keeps every deleting CR around
// for that retry pass. Refining to per-CR failure attribution is out
// of scope (per the spec).
func (r *Reconciler) removeFinalizersOnDeleted(
	ctx context.Context,
	boots []spiceboxv1alpha1.SpiceDBBootstrap,
	deleteFailures int,
) {
	logger := log.FromContext(ctx).WithName("guardian.bootstrap.finalizer")
	for i := range boots {
		b := &boots[i]
		if b.DeletionTimestamp == nil {
			continue
		}
		if !controllerutil.ContainsFinalizer(b, spiceboxv1alpha1.FinalizerSpiceDBBootstrap) {
			continue
		}
		// Defensive: only remove the finalizer if the diff actually
		// completed without delete failures for THIS CR's tuples.
		// (If deletes failed we want to retry, so leave the finalizer.)
		if deleteFailures > 0 {
			continue
		}
		cp := b.DeepCopy()
		controllerutil.RemoveFinalizer(cp, spiceboxv1alpha1.FinalizerSpiceDBBootstrap)
		if err := r.Client.Patch(ctx, cp, client.MergeFrom(b)); err != nil {
			logger.Info("remove finalizer failed",
				"cr", b.Namespace+"/"+b.Name, "err", err.Error())
		}
	}
}

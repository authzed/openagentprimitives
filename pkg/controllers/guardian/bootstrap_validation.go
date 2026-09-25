// Package guardian's bootstrap_validation.go implements SpiceDBBootstrap
// layer-2 validation that cannot be expressed via OpenAPI/CEL alone.
package guardian

import (
	"fmt"
	"strings"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
)

// ValidateSpec is a TEST-ONLY convenience: nothing in this package's
// Reconcile path calls it. Production calls validateShape (the whole-CR
// Valid gate) and relationshipOwnershipRefusals (the per-relationship
// ownership surface) SEPARATELY — see bootstrap_sync.go's
// validateBootstraps — precisely because folding ownership into the
// whole-CR verdict would invalidate every OTHER relationship in the CR too,
// which the write path does not do (see validateShape's own doc comment).
// ValidateSpec exists only so a test that wants a single whole-spec verdict
// covering all three checks together (schema shape, subject shape,
// ownership) can make one call instead of two — TestValidateSpec,
// TestValidateSpec_SchemaRedeclaresUser,
// TestValidation_RefusesARelationAnotherSourceOwns and
// TestValidation_AllowsAnUnclaimedRelation want exactly that aggregate view
// today. A test that wants to prove agreement with the write-time guard
// specifically must call relationshipOwnershipRefusals instead (via
// RelationshipOwnershipRefusalsForTest, export_test.go) — see
// TestValidation_AgreesWithTheWriteTimeGuard — because that is what
// production actually calls; ValidateSpec agreeing with itself would prove
// nothing.
//
// Returns ("", "") on success; otherwise (reason, message) in the shape a
// Valid=False condition wants.
//
// The cross-CR schema-conflict check is a DIFFERENT thing and is not
// performed here: a bootstrap's spec.spicedbSchema fragment goes through
// guardianschema.ValidateFragment (invalid on its own) and
// guardianschema.PartitionCompatibleFragments (conflicts with an
// already-accepted fragment) in agentsessiongrants_controller.go's
// Reconcile, alongside every other CRD-contributed fragment, and a
// rejection is stamped on the CR's SchemaIncluded condition — not Valid,
// which this function alone determines.
//
// Checks schema shape, subject shape, and relationship ownership together
// and returns the FIRST failure across all three, in that order.
func ValidateSpec(boot *spiceboxv1alpha1.SpiceDBBootstrap) (reason, message string) {
	if r, m := validateShape(boot); r != "" {
		return r, m
	}
	for i, rel := range boot.Spec.Relationships {
		if r, m := validateRelationshipOwnership(i, rel); r != "" {
			return r, m
		}
	}
	return "", ""
}

// validateShape runs the schema-fragment check plus every relationship's
// SUBJECT-SHAPE check (validateRelationship) — everything ValidateSpec
// checks EXCEPT ownership. bootstrap_sync.go's validateBootstraps uses only
// this for the whole-CR Valid=False gate.
//
// A malformed relationship is a spec-authoring error: the operator must fix
// it before ANY of the CR's relationships converge, so the whole CR stays
// invalid (buildNewDesired skips it, carrying its previously-applied tuples
// forward unchanged) until they do. An ownership conflict is a different
// kind of fact — some OTHER relsource.Source, possibly registered by a
// later, unrelated change, already claims one specific relation — and the
// write path already handles that per-tuple
// (spicedb.TouchBootstrapRelationshipVia via relsource.CheckWrite, refusing
// just the one relationship and continuing with the rest). Folding ownership
// into this whole-CR gate would invalidate every OTHER relationship in the
// CR too, which the write path does not do — the two enforcement points
// would agree on the verdict ("this relationship is refused") but disagree
// on the EFFECT (one relationship refused vs. the whole CR's sync halted).
func validateShape(boot *spiceboxv1alpha1.SpiceDBBootstrap) (reason, message string) {
	if r, m := validateSchemaFragment(boot); r != "" {
		return r, m
	}
	for i, rel := range boot.Spec.Relationships {
		if r, m := validateRelationship(i, rel); r != "" {
			return r, m
		}
	}
	return "", ""
}

// relationshipOwnershipRefusals returns, for EVERY relationship in
// boot.Spec.Relationships whose resourceType#relation another
// relsource.Source claims, its refusal message, in relationship order —
// exhaustive, unlike ValidateSpec's first-failure short-circuit, so a caller
// can act on every refused relationship rather than only the first one
// found.
//
// A plain slice, not map[int]string: nothing has ever read the index — the
// one caller (bootstrap_sync.go's validateBootstraps) used to copy the
// map's values into a slice and sort.Strings it purely to get a
// deterministic order back out of an unordered map. Building the slice in
// the order boot.Spec.Relationships already declares makes the result
// deterministic for free, so that copy-and-sort is gone from the caller too.
//
// bootstrap_sync.go's validateBootstraps calls this (only after
// validateShape has already passed) purely to SURFACE which relationships
// are affected on the CR's status. It deliberately does NOT feed
// buildNewDesired: a refused relationship is left in newDesired exactly like
// every other one, so the write path's own per-tuple guard
// (relsource.CheckWrite) remains the sole place that actually refuses it —
// matching the pre-existing write-path behaviour this fix restores (the
// other relationships keep converging; this one is retried and refused
// every pass, logged each time by applyTouches). seedPriorClaimsForDeleting
// (bootstrap_sync.go) calls validateRelationshipOwnership directly, for the
// same reason but the opposite direction: to EXCLUDE a refused relationship
// from being recovered into lastDesired as though it had been written.
func relationshipOwnershipRefusals(boot *spiceboxv1alpha1.SpiceDBBootstrap) []string {
	var refused []string
	for i, rel := range boot.Spec.Relationships {
		if _, m := validateRelationshipOwnership(i, rel); m != "" {
			refused = append(refused, m)
		}
	}
	return refused
}

func validateSchemaFragment(boot *spiceboxv1alpha1.SpiceDBBootstrap) (string, string) {
	if boot.Spec.SpiceDBSchema == nil {
		return "", ""
	}
	for _, r := range boot.Spec.SpiceDBSchema.Resources {
		if r.Name == "user" {
			return spiceboxv1alpha1.ReasonCannotRedeclareUser,
				"spicedbSchema must not redeclare the implicit resource \"user\""
		}
	}
	return "", ""
}

func validateRelationship(i int, rel spiceboxv1alpha1.SpiceDBBootstrapRelationship) (string, string) {
	s := rel.Subject
	if s.Wildcard {
		if s.Canonicalize {
			return spiceboxv1alpha1.ReasonSubjectWildcardConflict,
				fmt.Sprintf("relationships[%d]: subject.wildcard is incompatible with subject.canonicalize", i)
		}
		if s.Relation != "" {
			return spiceboxv1alpha1.ReasonSubjectWildcardConflict,
				fmt.Sprintf("relationships[%d]: subject.wildcard is incompatible with subject.relation", i)
		}
		return "", ""
	}
	if s.Canonicalize {
		if s.Type != "user" {
			return spiceboxv1alpha1.ReasonCanonicalizeRequiresUserEmail,
				fmt.Sprintf("relationships[%d]: subject.canonicalize=true requires subject.type=\"user\" (got %q)", i, s.Type)
		}
		if !strings.Contains(s.ID, "@") {
			return spiceboxv1alpha1.ReasonCanonicalizeRequiresUserEmail,
				fmt.Sprintf("relationships[%d]: subject.canonicalize=true requires subject.id to be an email (got %q)", i, s.ID)
		}
		if s.Relation != "" {
			return spiceboxv1alpha1.ReasonSubjectSetCannotCanonicalize,
				fmt.Sprintf("relationships[%d]: subject.canonicalize is incompatible with subject.relation", i)
		}
	} else if s.Type == "user" && strings.Contains(s.ID, "@") {
		return spiceboxv1alpha1.ReasonUserSubjectNotCanonical,
			fmt.Sprintf("relationships[%d]: subject.type=\"user\" with id containing \"@\" must set canonicalize=true (or pre-canonicalize via `oap identity canonical-id`)", i)
	}
	return "", ""
}

// validateRelationshipOwnership refuses a relationship whose
// resourceType#relation is claimed by a source other than the bootstrap
// reconciler's own. It is convenience only: the write path
// (spicedb.TouchBootstrapRelationshipVia, via relsource.CheckWrite) stays
// the authoritative refusal, and would refuse the same relationship even if
// this check were skipped. Calling it lets the operator learn about the
// refusal from the CR's own status (relationshipOwnershipRefusals below,
// surfaced via RelationshipsApplied — see bootstrap_sync.go's
// validateBootstraps and patchBootstrapStatus) rather than only from a
// reconcile log — but, unlike a schema/shape failure, it does not flip the
// whole CR's Valid condition: see validateShape's doc comment for why.
//
// It calls relsource.CheckWrite directly rather than re-deriving the
// ownership rule from the claim table: a second implementation of the same
// check is a second thing to keep in sync by hand, and
// TestValidation_AgreesWithTheWriteTimeGuard exists to catch the day it
// drifts. The RelationshipUpdate built here carries only
// Resource.ObjectType and Relation — the two fields CheckWrite reads —
// taken from ResolveTuple, the same resolver the write path uses to build
// the tuple actually sent, so agreement is structural rather than
// coincidental.
func validateRelationshipOwnership(i int, rel spiceboxv1alpha1.SpiceDBBootstrapRelationship) (string, string) {
	t := ResolveTuple(rel)
	update := &v1.RelationshipUpdate{
		Relationship: &v1.Relationship{
			Resource: &v1.ObjectReference{ObjectType: t.ResourceType},
			Relation: t.Relation,
		},
	}
	if err := relsource.CheckWrite(spicedb.BootstrapSource, []*v1.RelationshipUpdate{update}); err != nil {
		return spiceboxv1alpha1.ReasonRelationOwnedByAnotherSource,
			fmt.Sprintf("relationships[%d]: %s", i, err.Error())
	}
	return "", ""
}

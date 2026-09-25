//go:build integration

// Semantic tests for the provenance lattice (Track 0). The lattice is not
// modelled in SpiceDB; it IS the arrow semantics, so these assertions are the
// only thing standing between the schema text and a silent inversion of it.
//
// Shares checkOnCanonicalSchema and the ref parsers with
// schema_semantics_integration_test.go.
//
//	go test -tags=integration -count=1 ./pkg/authz/spicedb/schema/
package schema_test

import (
	"testing"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
)

// TestPtTagReaderIsAnIntersectionOverTheDerivationTree pins the
// confidentiality half of the lattice.
//
// This is the assertion Track 0's spec calls REQUIRED, and it exists because
// the prototype being ported got it backwards: its ParseTags computed the
// UNION of subjects across tags while tag creation computed the INTERSECTION
// across resources. Concatenating a tag readable by A with one readable by B
// yielded a payload deemed viewable by both — a laundering primitive, and
// precisely the operation a parent performs when assembling a handoff.
//
// `derived_from.all(reader)` is the intersection. If it is ever "simplified"
// to `derived_from->reader`, that is a union, and this test is what says so.
func TestPtTagReaderIsAnIntersectionOverTheDerivationTree(t *testing.T) {
	t.Parallel()
	// leaf_a is alice's, leaf_b is bob's. derived combines them, so it belongs
	// to nobody: no user is authorized on BOTH sources.
	rels := []string{
		"pt_tag:leaf_a#direct_reader@user:alice",
		"pt_tag:leaf_b#direct_reader@user:bob",
		"pt_tag:derived#derived_from@pt_tag:leaf_a",
		"pt_tag:derived#derived_from@pt_tag:leaf_b",
	}
	for _, subject := range []string{"user:alice", "user:bob"} {
		got := checkOnCanonicalSchema(t, rels, "pt_tag:derived", "reader", subject)
		assert.Equal(t, v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION, got,
			"%s reads only one of the two sources, so the combination must be readable by nobody", subject)
	}
}

// TestPtTagReaderKeepsTheSharedReaderOfBothSources is the positive half. An
// empty intersection proves nothing on its own — a permission that always
// denied would pass the test above.
func TestPtTagReaderKeepsTheSharedReaderOfBothSources(t *testing.T) {
	t.Parallel()
	// Both sources are readable by alice; only one is also readable by carol.
	rels := []string{
		"pt_tag:src_1#direct_reader@user:alice",
		"pt_tag:src_1#direct_reader@user:carol",
		"pt_tag:src_2#direct_reader@user:alice",
		"pt_tag:both#derived_from@pt_tag:src_1",
		"pt_tag:both#derived_from@pt_tag:src_2",
	}
	assert.Equal(t, v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION,
		checkOnCanonicalSchema(t, rels, "pt_tag:both", "reader", "user:alice"),
		"alice reads both sources, so she must read the combination")
	assert.Equal(t, v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION,
		checkOnCanonicalSchema(t, rels, "pt_tag:both", "reader", "user:carol"),
		"carol reads only one source; the intersection must exclude her")
}

// TestPtTagLeafReaderIsNotVacuouslyEveryone pins the interaction between an
// EMPTY derived_from and the intersection arrow — the failure mode that would
// invert the entire lattice without changing a line of the gate.
//
// `reader = direct_reader + derived_from.all(reader)`. On a leaf tag
// derived_from is empty. If `.all()` over an empty relation were vacuously
// true — the ordinary reading of "for all" in logic — the union would grant
// `reader` to EVERY user on every leaf tag, and a disclosure gate built on it
// would authorize everything while appearing to function normally.
//
// Asserted as behaviour rather than trusted from the arrow's documented
// semantics, because the cost of being wrong here is total and silent.
func TestPtTagLeafReaderIsNotVacuouslyEveryone(t *testing.T) {
	t.Parallel()
	rels := []string{"pt_tag:leaf_only#direct_reader@user:alice"}
	assert.Equal(t, v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION,
		checkOnCanonicalSchema(t, rels, "pt_tag:leaf_only", "reader", "user:alice"),
		"the named direct_reader must read the leaf")
	assert.Equal(t, v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION,
		checkOnCanonicalSchema(t, rels, "pt_tag:leaf_only", "reader", "user:mallory"),
		"a stranger must be refused: if an empty derived_from arrows to vacuous truth, every leaf tag is world-readable")
}

// TestPtTagCarriesUntrustedIsAUnionOverTheDerivationTree pins the integrity
// half, which runs the opposite direction to confidentiality.
//
// Integrity is a minimum, which over a boolean is a union, so it is `->`. One
// untrusted source anywhere in the tree taints everything derived from it, and
// mixing in a trusted input does not launder it clean.
func TestPtTagCarriesUntrustedIsAUnionOverTheDerivationTree(t *testing.T) {
	t.Parallel()
	rels := []string{
		"pt_tag:tainted#untrusted_origin@pt_tag:tainted",
		"pt_tag:mixed#derived_from@pt_tag:tainted",
		"pt_tag:mixed#derived_from@pt_tag:clean",
	}
	assert.Equal(t, v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION,
		checkOnCanonicalSchema(t, rels, "pt_tag:mixed", "carries_untrusted", "pt_tag:tainted"),
		"one untrusted source must taint everything derived from it; a clean sibling cannot launder it")
}

// TestPtTagGrantedToDoesNotArrowFurther pins monotonic attenuation applied to
// information.
//
// A tag minted in one session and granted to a child is held by that child
// alone; a grandchild needs its own explicit binding. If granted_to arrowed
// onward, a single grant would cascade to an entire subtree the granter never
// saw.
func TestPtTagGrantedToDoesNotArrowFurther(t *testing.T) {
	t.Parallel()
	rels := []string{
		// granted_to is `agentsession with expiration`, so the tuple must carry
		// one — a bare subject is refused at write time, not at check time.
		"pt_tag:minted#granted_to@agentsession:default/ptchild|expires:1h",
		"agentsession:default/ptgrandchild#parent@agentsession:default/ptchild",
	}
	assert.Equal(t, v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION,
		checkOnCanonicalSchema(t, rels, "pt_tag:minted", "access", "agentsession:default/ptchild"),
		"the session the tag was granted to must hold it")
	assert.Equal(t, v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION,
		checkOnCanonicalSchema(t, rels, "pt_tag:minted", "access", "agentsession:default/ptgrandchild"),
		"a grandchild needs its own binding; a grant that arrowed onward would reach a subtree the granter never saw")
}

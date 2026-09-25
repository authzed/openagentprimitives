// Package sentinel_test exercises relsource.CheckWrite/CheckDeleteFilter in
// a process with a controlled registry: nothing outside this file ever
// calls relsource.Register or relsource.MarkComplete here, unlike
// pkg/authz/spicedb/relsource's own check_test.go and claims_test.go, whose
// init() marks the table complete and whose many tests accumulate claims
// in the registry over the life of that test binary (registry.go documents
// why it is never reset). That makes two things observable only here:
// the fail-closed gate in its genuinely unmarked state (every init() in a
// Go test binary runs before any Test function, regardless of which
// _test.go file declared it, so a package that shares a binary with an
// init()-calling file can never see "unmarked"), and the empty-ResourceType
// could-match arm's allow case, which needs a source that owns literally
// every claim in the registry — unreachable in a registry other tests keep
// adding to.
package sentinel_test

import (
	"testing"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
)

func touch(resourceType, relation string) *v1.RelationshipUpdate {
	return &v1.RelationshipUpdate{
		Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
		Relationship: &v1.Relationship{
			Resource: &v1.ObjectReference{ObjectType: resourceType, ObjectId: "r1"},
			Relation: relation,
			Subject:  &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: "u1"}},
		},
	}
}

// Before MarkComplete has ever run, CheckWrite and CheckDeleteFilter refuse
// every call — even one naming a relation nothing has ever claimed — and
// the error names the bundle a caller must link. This is the fail-closed
// half of the fix: a binary that forgets
// pkg/authz/spicedb/relsource/imports fails loudly here instead of
// silently allowing everything.
func TestUnmarkedTable_RefusesAndNamesTheBundle(t *testing.T) {
	src := relsource.Source{Name: "sentinel-before"}

	err := relsource.CheckWrite(src, []*v1.RelationshipUpdate{touch("widget", "owner")})
	require.Error(t, err, "CheckWrite must refuse before the claim table is marked complete")
	assert.Contains(t, err.Error(), "relsource/imports", "the refusal must name the bundle a caller links to fix this")

	err = relsource.CheckDeleteFilter(src, &v1.RelationshipFilter{ResourceType: "widget", OptionalRelation: "owner"})
	require.Error(t, err, "CheckDeleteFilter must refuse before the claim table is marked complete")
	assert.Contains(t, err.Error(), "relsource/imports")

	// MarkComplete flips the gate: from here on, CheckWrite/CheckDeleteFilter
	// behave exactly as they did before this fix — an unclaimed relation is
	// allowed.
	relsource.MarkComplete()

	err = relsource.CheckWrite(src, []*v1.RelationshipUpdate{touch("widget", "owner")})
	assert.NoError(t, err, "once marked complete, an unclaimed relation must be allowed again")

	err = relsource.CheckDeleteFilter(src, &v1.RelationshipFilter{ResourceType: "widget", OptionalRelation: "owner"})
	assert.NoError(t, err, "once marked complete, an unclaimed relation must be allowed again")
}

// TestCheckDeleteFilter_EmptyResourceType covers the second could-match
// hole: an empty ResourceType matches every relation on every type, so it
// must be refused unless src owns EVERY claim in the whole registry — not
// just every claim on one type, which the empty-OptionalRelation arm
// already covers.
//
// This runs here, in the isolated sentinel package, rather than in
// check_test.go: check_test.go and claims_test.go share ONE never-reset
// registry across their whole test binary (see registry.go's own doc on
// why), so by the time any one of their tests runs, several other sources
// already own claims — no single source there could plausibly own "every
// claim in the registry", making the allow arm untestable there. This
// package registers nothing except what this file's tests add, and — since
// both tests live in this one file — runs after
// TestUnmarkedTable_RefusesAndNamesTheBundle in declaration order, so the
// table is already marked complete by the time it runs.
func TestCheckDeleteFilter_EmptyResourceType(t *testing.T) {
	relsource.MarkComplete() // idempotent; harmless even though the prior test already called it

	owner := relsource.Source{Name: "everythingowner", Claims: []string{"asset#reader", "asset#writer"}}
	relsource.Register(owner)

	other := relsource.Source{Name: "intruder-no-type"}
	err := relsource.CheckDeleteFilter(other, &v1.RelationshipFilter{}) // ResourceType and OptionalRelation both empty
	require.Error(t, err, "an untyped filter must be refused for a source that doesn't own every claim in the registry")
	assert.Contains(t, err.Error(), "everythingowner")
	assert.Contains(t, err.Error(), "intruder-no-type")

	err = relsource.CheckDeleteFilter(owner, &v1.RelationshipFilter{})
	assert.NoError(t, err, "the source owning every claim in the registry may use an untyped filter")
}

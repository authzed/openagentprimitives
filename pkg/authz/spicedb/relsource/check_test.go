package relsource_test

import (
	"testing"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
)

// This package's own tests cannot blank-import
// pkg/authz/spicedb/relsource/imports — the bundle imports
// pkg/authz/spicedb, which imports this package, so doing so from here
// would be an import cycle. Mark the table complete directly instead, once
// for the whole test binary (this file, claims_test.go and
// registry_internal_test.go all share one process); CheckWrite and
// CheckDeleteFilter otherwise refuse every call regardless of what fixture
// claims a test registers.
func init() {
	relsource.MarkComplete()
}

// touch builds a TOUCH RelationshipUpdate for
// resourceType:resourceID#relation@subjectType:subjectID — the only
// operation these tests need, since CheckWrite cares about which relation an
// update names, not which operation it performs.
func touch(resourceType, resourceID, relation, subjectType, subjectID string) *v1.RelationshipUpdate {
	return &v1.RelationshipUpdate{
		Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
		Relationship: &v1.Relationship{
			Resource: &v1.ObjectReference{ObjectType: resourceType, ObjectId: resourceID},
			Relation: relation,
			Subject: &v1.SubjectReference{
				Object: &v1.ObjectReference{ObjectType: subjectType, ObjectId: subjectID},
			},
		},
	}
}

// A source owns relations; another source writing them is refused. The error
// names both, because "refused" without an owner sends the reader hunting.
func TestCheckWrite_RefusesARelationAnotherSourceOwns(t *testing.T) {
	// widget_channel is a fixture-only placeholder, deliberately NOT
	// slack_channel: claims_test.go registers the real slack directory sync
	// source's claims (slack_channel#member among them) in this same test
	// binary, and a fixture claiming the same relation would collide with it
	// at index-build time — this test is about the REFUSAL mechanism, not
	// about any particular relation name.
	owner := relsource.Source{Name: "syncengine", Claims: []string{"widget_channel#member"}}
	other := relsource.Source{Name: "memoryauthz", Claims: []string{"memory_entry#session"}}
	relsource.Register(owner)
	relsource.Register(other)

	err := relsource.CheckWrite(other, []*v1.RelationshipUpdate{
		touch("widget_channel", "C1", "member", "slack_user", "U1"),
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "memoryauthz")
	assert.Contains(t, err.Error(), "widget_channel#member")
	assert.Contains(t, err.Error(), "syncengine")
}

func TestCheckWrite_AllowsAnOwnedRelation(t *testing.T) {
	src := relsource.Source{Name: "workspacesync", Claims: []string{"workspace_repo#writer"}}
	relsource.Register(src)

	err := relsource.CheckWrite(src, []*v1.RelationshipUpdate{
		touch("workspace_repo", "W1", "writer", "user", "U1"),
	})

	assert.NoError(t, err)
}

func TestCheckWrite_AllowsAnUnclaimedRelation(t *testing.T) {
	// Nothing has claimed audit_log#reader — an unclaimed relation is
	// unrestricted, regardless of who writes it.
	err := relsource.CheckWrite(relsource.Source{Name: "anyone"}, []*v1.RelationshipUpdate{
		touch("audit_log", "A1", "reader", "user", "U1"),
	})

	assert.NoError(t, err)
}

// Owns is the question CheckWrite does not answer, and the gap between the
// two is what a deleting caller has to know about: CheckWrite ALLOWS an
// unclaimed relation (the case directly above), so taking its silence for
// ownership is how a sync ends up deleting a relation somebody else writes.
// Asserted together in one test precisely because the pair is the point.
func TestOwns_AnUnclaimedRelationIsWritableButNotOwned(t *testing.T) {
	src := relsource.Source{Name: "gadgetsync", Claims: []string{"gadget_url#gadget"}}
	relsource.Register(src)

	assert.True(t, relsource.Owns(src, "gadget_url", "gadget"),
		"a source owns the relation it claims")
	assert.False(t, relsource.Owns(src, "gadget_url", "owner"),
		"an unclaimed relation on a type the source DOES write is not owned by it")
	assert.NoError(t, relsource.CheckWrite(src, []*v1.RelationshipUpdate{
		touch("gadget_url", "G1", "owner", "user", "U1"),
	}), "and CheckWrite still permits writing it — unclaimed means unguarded, not reserved")
}

// The case that would otherwise defeat the guard: a filter with no relation
// sweeps every relation on the type, claimed ones included, without naming one.
func TestCheckDeleteFilter_RefusesAnEmptyRelationOnATypeWithAClaim(t *testing.T) {
	relsource.Register(relsource.Source{Name: "channelsync", Claims: []string{"slack_channel#admin"}})
	other := relsource.Source{Name: "memoryauthz"}

	err := relsource.CheckDeleteFilter(other, &v1.RelationshipFilter{
		ResourceType: "slack_channel", // OptionalRelation deliberately empty
	})

	require.Error(t, err, "an unrelation'd filter sweeps claimed relations it never names")
	assert.Contains(t, err.Error(), "slack_channel")
}

func TestCheckDeleteFilter_AllowsAnEmptyRelationWhenTheSourceOwnsEveryClaimOnTheType(t *testing.T) {
	owner := relsource.Source{Name: "assetkeeper", Claims: []string{"asset_bundle#writer", "asset_bundle#admin"}}
	relsource.Register(owner)

	err := relsource.CheckDeleteFilter(owner, &v1.RelationshipFilter{
		ResourceType: "asset_bundle", // OptionalRelation deliberately empty
	})

	assert.NoError(t, err)
}

func TestCheckDeleteFilter_AllowsAnEmptyRelationOnATypeWithNoClaims(t *testing.T) {
	err := relsource.CheckDeleteFilter(relsource.Source{Name: "anyone"}, &v1.RelationshipFilter{
		ResourceType: "unclaimed_widget", // OptionalRelation deliberately empty
	})

	assert.NoError(t, err)
}

func TestCheckDeleteFilter_RefusesANamedRelationAnotherSourceOwns(t *testing.T) {
	owner := relsource.Source{Name: "reposync", Claims: []string{"git_repo#maintainer"}}
	relsource.Register(owner)
	other := relsource.Source{Name: "memoryauthz"}

	err := relsource.CheckDeleteFilter(other, &v1.RelationshipFilter{
		ResourceType:     "git_repo",
		OptionalRelation: "maintainer",
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "memoryauthz")
	assert.Contains(t, err.Error(), "git_repo#maintainer")
	assert.Contains(t, err.Error(), "reposync")
}

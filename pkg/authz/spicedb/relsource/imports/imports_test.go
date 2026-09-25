package imports_test

import (
	"testing"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/relwrites"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
	_ "github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource/imports"
)

// TestBundle_ClaimsPtTagAgainstARunnerShapedLinkSet reproduces the Critical
// this bundle exists to close. internal/cmd/runner links relwrites (the
// writer that evaluates a toolspec's arbitrary CEL-resolved
// writesRelationships) but never linked pkg/memory/pttagmint directly, so
// pt_tag#direct_reader — which grants disclosure — went unclaimed there and
// a toolspec could write it. This test's only production imports are
// relwrites and this bundle (mirroring the runner's link set for the
// relevant packages, not the whole binary), so a pass here is a pass in
// that binary's actual link graph, not a fixture standing in for it.
func TestBundle_ClaimsPtTagAgainstARunnerShapedLinkSet(t *testing.T) {
	update := &v1.RelationshipUpdate{
		Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
		Relationship: &v1.Relationship{
			Resource: &v1.ObjectReference{ObjectType: "pt_tag", ObjectId: "t1"},
			Relation: "direct_reader",
			Subject:  &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: "whoever"}},
		},
	}

	err := relsource.CheckWrite(relwrites.Source, []*v1.RelationshipUpdate{update})
	require.Error(t, err, "relwrites (which claims nothing itself) must be refused on pt_tag#direct_reader once this bundle is linked")
	assert.Contains(t, err.Error(), "relwrites")
	assert.Contains(t, err.Error(), "pt_tag#direct_reader")
	assert.Contains(t, err.Error(), "pttagmint")
}

// group#member must stay UNCLAIMED, and this is the test that says so out
// loud, because something invisible depends on it.
//
// A 1Password sync writes onepassword_group:<id>#member@user:…, and the
// scaffold's `group#member` unions onepassword_group#member — but the join
// between a named `group` and a synced directory group,
// group:<name>#member@onepassword_group:<id>#member, is written by no
// controller and no command. An operator writes it declaratively, with a
// SpiceDBBootstrap CR (docs/relationshipsource.md, "Joining a synced group to
// a `group`"), and that CR's writer claims nothing.
//
// So the day any source claims group#member, relsource.CheckWrite starts
// refusing that bootstrap write, and every operator who followed those
// instructions gets a CR that silently stops applying — for a relation
// deliberately left to nobody so a hand-made group stays writable. Failing
// here instead makes the consequence visible to whoever adds the claim.
//
// Asserted from the COMPLETE bundle, not from any single source's Claims: the
// question is whether anything in the whole linked table owns it.
func TestBundle_LeavesGroupMemberUnclaimedForTheBootstrapJoin(t *testing.T) {
	join := &v1.RelationshipUpdate{
		Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
		Relationship: &v1.Relationship{
			Resource: &v1.ObjectReference{ObjectType: "group", ObjectId: "engineering"},
			Relation: "member",
			Subject: &v1.SubjectReference{
				Object:           &v1.ObjectReference{ObjectType: "onepassword_group", ObjectId: "abc123"},
				OptionalRelation: "member",
			},
		},
	}

	assert.NoError(t, relsource.CheckWrite(spicedb.BootstrapSource, []*v1.RelationshipUpdate{join}),
		"a SpiceDBBootstrap CR must be able to write the documented group->onepassword_group join")

	for _, src := range relsource.All() {
		assert.NotContains(t, src.Claims, "group#member",
			"source %q claims group#member; the documented bootstrap join, and every hand-made group, "+
				"depend on that relation belonging to nobody", src.Name)
	}
}

//go:build integration

package spicedb

import (
	"context"
	"testing"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// fixtureClaims registers one directory source for this file's tests.
// relsource.Register panics on a duplicate NAME, so registration happens once
// here rather than per-test.
//
// It must happen at all because pkg/authz/spicedb cannot link
// relsource/imports (the bundle imports back through this package — see
// complete.go's own note naming this package). relsource.All() is therefore
// empty here by default, and a test that did not register would assert
// against an empty probe set and pass while proving nothing.
//
// Claims here are "memory_entry#creator" and "artifact#creator" rather than
// the more directory-sync-flavored "github_user#user" / "github_org#member":
// "github_user#user" is already claimed for real by this package's own
// TypedWritesSource (typed_writes_source.go), and "github_org" is not part
// of the bare scaffold newIntegrationClient writes (it exists only in the
// github channel kind's schema fragment, composed in by the operator, never
// by test/testspicedb.WriteSchema). Claiming an already-claimed relation
// would panic the first time anything in this binary calls
// CheckWrite/CheckDeleteFilter (relsource.buildIndex refuses two different
// sources claiming one relation), and writing to an undeclared definition
// would be refused by SpiceDB's own schema validation. memory_entry#creator
// and artifact#creator are real, unclaimed-elsewhere-in-this-binary
// relations already declared in the bare scaffold (schema.zed), so both
// problems are avoided while still exercising a real (definition, relation,
// subject) shape.
//
// Both claims are also declared as SubjectIdentityClaims: the reader probes
// only what a source declares as a user-subject link, and writeUserLink below
// writes exactly that shape (@user:<canonical>), so a fixture that claimed them
// without declaring them would register a source the reader correctly ignores
// and these tests would assert against an empty probe set.
func init() {
	relsource.Register(relsource.Source{
		Name: "fixture-directory",
		Claims: []string{
			"memory_entry#creator",
			"artifact#creator",
		},
		SubjectIdentityClaims: []string{
			"memory_entry#creator",
			"artifact#creator",
		},
	})
	relsource.Register(absentDefinitionSource)
}

// absentDefinitionSource declares a probe against a definition the bare
// scaffold does not carry, so every read of it fails. It stands in for the
// real case this reader has to survive: github_org / github_team / github_repo
// are declared only in the gh toolkit fragment, composed into the live schema
// by the guardian reconciler, so on a fresh cluster they are simply not there.
// The real github source cannot be used here — that package imports this one.
var absentDefinitionSource = relsource.Source{
	Name:                  "absent-definition-fixture",
	Claims:                []string{"no_such_definition#member"},
	SubjectIdentityClaims: []string{"no_such_definition#member"},
}

// writeUserLink writes <definition>:<id>#<relation>@user:<subject>.
func writeUserLink(t *testing.T, c *Client, definition, id, relation, subject string) {
	t.Helper()
	_, err := c.cl.WriteRelationships(context.Background(), &v1.WriteRelationshipsRequest{
		Updates: []*v1.RelationshipUpdate{{
			Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: &v1.Relationship{
				Resource: &v1.ObjectReference{ObjectType: definition, ObjectId: id},
				Relation: relation,
				Subject: &v1.SubjectReference{
					Object: &v1.ObjectReference{ObjectType: "user", ObjectId: subject},
				},
			},
		}},
	})
	require.NoError(t, err, "write %s:%s#%s", definition, id, relation)
}

// A directory sync links upstream objects to a platform user. The console's
// user page asks this question, and it must answer with the linked objects and
// attribute each to the sync that asserted it.
func TestListSubjectIdentities_FindsDirectoryLinkedObjects(t *testing.T) {
	ctx := context.Background()
	c := newIntegrationClient(t)
	casey := identity.CanonicalFromTrusted("Y2FzZXlAZXhhbXBsZS50ZXN0", "test fixture")

	writeUserLink(t, c, "memory_entry", "m-fixture-1", "creator", casey.String())
	writeUserLink(t, c, "artifact", "a-fixture-1", "creator", casey.String())

	got, err := c.ListSubjectIdentities(ctx, casey)
	require.NoError(t, err)

	// Unavailable is deliberately not asserted here: this file also registers
	// absentDefinitionSource, whose probe always fails against the bare
	// scaffold, and that partial-failure behavior is the NEXT test's subject.
	require.Len(t, got.Identities, 2, "both linked objects, and nothing else")
	assert.Contains(t, got.Identities, SubjectIdentity{
		Definition: "memory_entry", Relation: "creator", ObjectID: "m-fixture-1", Source: "fixture-directory",
	})
	assert.Contains(t, got.Identities, SubjectIdentity{
		Definition: "artifact", Relation: "creator", ObjectID: "a-fixture-1", Source: "fixture-directory",
	})
}

// A probe against a definition the live schema does not declare must SHORTEN
// the answer, not destroy it. errgroup's first-error-wins made one such probe
// abort every other, so on a fresh cluster — where github_org/github_team/
// github_repo exist only in the gh toolkit fragment the guardian reconciler
// composes in — every user's panel read "unavailable:".
//
// The absent definition is registered here rather than relied on from the real
// github source, which this package cannot import (that package imports this
// one).
func TestListSubjectIdentities_AnAbsentDefinitionShortensRatherThanBlanks(t *testing.T) {
	ctx := context.Background()
	c := newIntegrationClient(t)
	drew := identity.CanonicalFromTrusted("ZHJld0BleGFtcGxlLnRlc3Q", "test fixture")

	writeUserLink(t, c, "memory_entry", "m-fixture-2", "creator", drew.String())

	got, err := c.ListSubjectIdentities(ctx, drew)
	require.NoError(t, err, "a probe failure must not fail the whole read")

	assert.Contains(t, got.Identities, SubjectIdentity{
		Definition: "memory_entry", Relation: "creator", ObjectID: "m-fixture-2", Source: "fixture-directory",
	}, "the links that COULD be read are still returned")

	// Exactly one, and the count is a real claim about this binary's whole
	// probe set: absentDefinitionSource is the ONLY registered identity claim
	// naming a definition the bare scaffold lacks. fixture-directory's two
	// (memory_entry, artifact) and TypedWritesSource's two (github_user, linked
	// from this package's own non-test file) are all in schema.zed and read
	// cleanly. A count above one therefore means a fixture leaked in from a
	// unit-only test file — see subject_probes_test.go's build constraint.
	require.Len(t, got.Unavailable, 1, "the probe that could not be read is reported, not swallowed — and nothing else failed")
	assert.Equal(t, "absent-definition-fixture", got.Unavailable[0].Source)
	assert.Equal(t, "no_such_definition", got.Unavailable[0].Definition)
	assert.Equal(t, "member", got.Unavailable[0].Relation)
	assert.NotEmpty(t, got.Unavailable[0].Err, "the reason must survive to the caller")
}

// A user with no directory links gets an empty answer, not an error.
//
// Asserted on got.Identities, never on got itself: ListSubjectIdentities
// returns a STRUCT, and testify's isEmptyValue does not treat a non-zero struct
// as empty — so `assert.Empty(t, got)` could neither pass for the right reason
// nor fail the way this test intends. (It would in fact fail here for an
// unrelated reason: absentDefinitionSource puts one entry in Unavailable on
// every call.)
func TestListSubjectIdentities_NoLinksIsEmptyNotAnError(t *testing.T) {
	ctx := context.Background()
	c := newIntegrationClient(t)
	riley := identity.CanonicalFromTrusted("cmlsZXlAZXhhbXBsZS50ZXN0", "test fixture")

	got, err := c.ListSubjectIdentities(ctx, riley)

	require.NoError(t, err)
	assert.Empty(t, got.Identities, "no links found is an empty list, not an error")
}

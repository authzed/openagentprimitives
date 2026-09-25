package steelthread_test

import (
	"context"
	"testing"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/authzdecision"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/relwritesaudit"
	"github.com/authzed/openagentprimitives/pkg/steelthread"
)

// fakeExpandAPI answers ExpandPermissionTree with a canned tree. It stands in
// for SpiceDB so the flattening and, above all, the SUBJECT RENDERING are
// testable without a container.
type fakeExpandAPI struct {
	tree *v1.PermissionRelationshipTree
	err  error
	reqs []*v1.ExpandPermissionTreeRequest
}

func (f *fakeExpandAPI) ExpandPermissionTree(
	_ context.Context, in *v1.ExpandPermissionTreeRequest, _ ...grpc.CallOption,
) (*v1.ExpandPermissionTreeResponse, error) {
	f.reqs = append(f.reqs, in)
	if f.err != nil {
		return nil, f.err
	}
	return &v1.ExpandPermissionTreeResponse{TreeRoot: f.tree}, nil
}

// leaf builds one expansion leaf: the object and relation on the left, the
// given subjects on the right.
func leaf(objType, objID, relation string, subjects ...*v1.SubjectReference) *v1.PermissionRelationshipTree {
	return &v1.PermissionRelationshipTree{
		ExpandedObject:   &v1.ObjectReference{ObjectType: objType, ObjectId: objID},
		ExpandedRelation: relation,
		TreeType:         &v1.PermissionRelationshipTree_Leaf{Leaf: &v1.DirectSubjectSet{Subjects: subjects}},
	}
}

func intermediate(children ...*v1.PermissionRelationshipTree) *v1.PermissionRelationshipTree {
	return &v1.PermissionRelationshipTree{
		TreeType: &v1.PermissionRelationshipTree_Intermediate{
			Intermediate: &v1.AlgebraicSubjectSet{Children: children},
		},
	}
}

// TestSpiceDBExpander_RendersSubjectsExactlyAsRelwritesAuditRecordsThem is R1,
// and it is a JOIN test rather than an assertion about either side alone.
//
// DeriveSeed subtracts a run-time-written tuple by matching the EXACT triple
// {Resource, Relation, Subject} against Records.RelWrites. Nothing enforces
// that the expander renders Subject the way relwrites_audit records it — the
// optional "#relation" suffix in particular — and if the two ever disagree the
// subtraction silently misses. A run-time-written tuple then gets SEEDED, which
// lets a replayed bundle pass with the relationship-writing code broken: the
// grant is already there, so the writer never runs and its absence never shows.
//
// Each case starts from the audit-recorded string, parses it with the SAME
// parsers the write path uses (spicedb.ParseSubject / SplitObject), builds the
// SpiceDB subject those coordinates describe, and requires the expander to
// render the original string back. Neither side is hand-transcribed.
func TestSpiceDBExpander_RendersSubjectsExactlyAsRelwritesAuditRecordsThem(t *testing.T) {
	cases := []struct {
		name string
		// recorded is the Subject string relwritesaudit.Tuple carries.
		recorded string
		// ellipsis makes SpiceDB report the default relation explicitly as
		// "...", which some expansions do and which must render as no suffix.
		ellipsis bool
	}{
		{name: "a direct user subject renders with no relation suffix", recorded: "user:alice"},
		{name: "a subject SET keeps its #relation suffix", recorded: "group:eng#member"},
		{name: "an explicit ellipsis relation renders as no suffix", recorded: "user:alice", ellipsis: true},
		{name: "a canonical id with base64url characters survives verbatim", recorded: "user:YWxpY2VAZXhhbXBsZS5jb20"},
		{name: "a wildcard subject renders verbatim", recorded: "user:*"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			subject := subjectRefFor(t, tc.recorded)
			if tc.ellipsis {
				subject.OptionalRelation = "..."
			}
			api := &fakeExpandAPI{tree: leaf("widget_catalog", "wc1", "reader", subject)}

			got, err := steelthread.NewSpiceDBExpander(api)(
				context.Background(), "widget_catalog", "wc1", "list")
			require.NoError(t, err)
			require.Len(t, got, 1)
			assert.Equal(t, tc.recorded, got[0].Subject,
				"the expander's rendering must equal what relwrites_audit records, or DeriveSeed's subtraction misses")
			assert.Equal(t, "widget_catalog:wc1", got[0].Resource)
			assert.Equal(t, "reader", got[0].Relation)
		})
	}
}

// subjectRefFor turns an audit-recorded subject string into the SpiceDB
// coordinates it denotes, using the production parsers rather than a split of
// this test's own. "type:id#relation" goes through spicedb.ParseSubject;
// "type:id" through spicedb.SplitObject.
func subjectRefFor(t *testing.T, recorded string) *v1.SubjectReference {
	t.Helper()
	if objType, objID, relation, err := spicedb.ParseSubject(recorded); err == nil {
		return &v1.SubjectReference{
			Object:           &v1.ObjectReference{ObjectType: objType, ObjectId: objID},
			OptionalRelation: relation,
		}
	}
	objType, objID, err := spicedb.SplitObject(recorded)
	require.NoError(t, err, "the fixture must be a well-formed recorded subject")
	return &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: objType, ObjectId: objID}}
}

// TestDeriveSeed_SubtractsASubjectSetTupleTheSessionWrote is R1 measured where
// it actually bites: end to end through DeriveSeed, with a real expander on one
// side and a real relwrites_audit record on the other.
//
// A subject-set tuple is the case that separates the two renderings — a
// suffix-dropping expander would produce "group:eng" against a recorded
// "group:eng#member", the equality would fail, and the tuple the SESSION wrote
// would be seeded into the fixture.
func TestDeriveSeed_SubtractsASubjectSetTupleTheSessionWrote(t *testing.T) {
	api := &fakeExpandAPI{tree: intermediate(
		leaf("widget_catalog", "wc1", "reader", subjectRefFor(t, "user:alice")),
		leaf("widget_catalog", "wc1", "editor", subjectRefFor(t, "group:eng#member")),
	)}
	recs := steelthread.Records{
		Decisions: []authzdecision.Decision{
			{ResourceType: "widget_catalog", ResourceID: "wc1", Permission: "list", Outcome: authzdecision.OutcomeAllowed},
		},
		RelWrites: []relwritesaudit.Audit{{Tuples: []relwritesaudit.Tuple{
			{Resource: "widget_catalog:wc1", Relation: "editor", Subject: "group:eng#member"},
		}}},
	}

	got, err := steelthread.DeriveSeed(recs, steelthread.NewSpiceDBExpander(api), nil)
	require.NoError(t, err)
	assert.Equal(t, []steelthread.Tuple{
		{Resource: "widget_catalog:wc1", Relation: "reader", Subject: "user:alice"},
	}, got.Tuples, "the tuple the SESSION wrote must not be seeded, or the writer is never exercised")
}

// TestSpiceDBExpander_AsksSpiceDBForTheRightPermission pins the request shape:
// an expander that asked about the wrong object would silently seed a tree
// belonging to something else.
func TestSpiceDBExpander_AsksSpiceDBForTheRightPermission(t *testing.T) {
	api := &fakeExpandAPI{tree: leaf("widget_catalog", "wc1", "reader", subjectRefFor(t, "user:alice"))}

	_, err := steelthread.NewSpiceDBExpander(api)(context.Background(), "widget_catalog", "wc1", "list")
	require.NoError(t, err)
	require.Len(t, api.reqs, 1)
	assert.Equal(t, "widget_catalog", api.reqs[0].GetResource().GetObjectType())
	assert.Equal(t, "wc1", api.reqs[0].GetResource().GetObjectId())
	assert.Equal(t, "list", api.reqs[0].GetPermission())
	assert.NotNil(t, api.reqs[0].GetConsistency().GetFullyConsistent(),
		"a capture reads state the session just wrote; anything weaker can miss it")
}

// TestSpiceDBExpander_ReturnsTheErrorRatherThanAnEmptyTree pins that a failed
// expansion is not reported as "this permission expands to nothing". The latter
// reads as an unseeded allow at worst and as a clean capture at best, and both
// are wrong for the same reason: nobody looked.
func TestSpiceDBExpander_ReturnsTheErrorRatherThanAnEmptyTree(t *testing.T) {
	api := &fakeExpandAPI{err: assertAnError{}}

	got, err := steelthread.NewSpiceDBExpander(api)(context.Background(), "widget_catalog", "wc1", "list")
	require.Error(t, err)
	assert.Empty(t, got)
}

type assertAnError struct{}

func (assertAnError) Error() string { return "spicedb is unreachable" }

// TestSpiceDBExpander_RefusesAMalformedNode pins that a leaf SpiceDB returned
// without the coordinates a tuple needs is an error, not a skipped row. A
// silently dropped leaf is a tuple missing from the seed, and the replay denies
// where the run allowed.
func TestSpiceDBExpander_RefusesAMalformedNode(t *testing.T) {
	api := &fakeExpandAPI{tree: leaf("widget_catalog", "wc1", "" /* no relation */, subjectRefFor(t, "user:alice"))}

	_, err := steelthread.NewSpiceDBExpander(api)(context.Background(), "widget_catalog", "wc1", "list")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "relation")
}

package approval

import (
	"context"
	"testing"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeGrantWriter records WriteRelationships calls for assertion in tests.
// Implements the Writer interface without touching the real SpiceDB client.
type fakeGrantWriter struct {
	writes []*v1.WriteRelationshipsRequest
}

func (f *fakeGrantWriter) WriteRelationships(_ context.Context, req *v1.WriteRelationshipsRequest) (*v1.WriteRelationshipsResponse, error) {
	f.writes = append(f.writes, req)
	return &v1.WriteRelationshipsResponse{}, nil
}

func TestWriteInfoLeakageGrants_OneSubjectOneResource(t *testing.T) {
	fw := &fakeGrantWriter{}
	ttl := 10 * time.Minute
	before := time.Now()

	grantID, err := WriteInfoLeakageGrants(
		context.Background(),
		fw,
		"default", "sess-1",
		[]string{"alice"},
		[]LeakageGrantResource{{Type: "linear_issue", ID: "ABC-123"}},
		ttl,
	)
	require.NoError(t, err)
	assert.NotEmpty(t, grantID, "grantID must be non-empty")

	// One WriteRelationships call with all updates batched.
	require.Len(t, fw.writes, 1)
	updates := fw.writes[0].Updates

	// 1 session + 1×1 audience_subject = 2 updates total.
	require.Len(t, updates, 2, "expected 1 session + 1 audience_subject update")

	// Verify session relationship.
	sessUpd := updates[0]
	assert.Equal(t, v1.RelationshipUpdate_OPERATION_TOUCH, sessUpd.Operation)
	sessRel := sessUpd.Relationship
	assert.Equal(t, InfoLeakageGrantDefinition, sessRel.Resource.ObjectType)
	assert.Equal(t, grantID, sessRel.Resource.ObjectId)
	assert.Equal(t, "session", sessRel.Relation)
	assert.Equal(t, "agentsession", sessRel.Subject.Object.ObjectType)
	assert.Equal(t, "default/sess-1", sessRel.Subject.Object.ObjectId)
	assert.Nil(t, sessRel.OptionalCaveat, "session relation must not carry a caveat")
	assert.Nil(t, sessRel.OptionalExpiresAt, "session relation must not have an expiration")

	// Verify audience_subject relationship.
	audUpd := updates[1]
	assert.Equal(t, v1.RelationshipUpdate_OPERATION_TOUCH, audUpd.Operation)
	audRel := audUpd.Relationship
	assert.Equal(t, InfoLeakageGrantDefinition, audRel.Resource.ObjectType)
	assert.Equal(t, grantID, audRel.Resource.ObjectId)
	assert.Equal(t, "audience_subject", audRel.Relation)
	assert.Equal(t, "user", audRel.Subject.Object.ObjectType)
	assert.Equal(t, "alice", audRel.Subject.Object.ObjectId)

	// Caveat: infoleakage_resource_match with correct type+id.
	require.NotNil(t, audRel.OptionalCaveat, "audience_subject must carry a caveat")
	assert.Equal(t, InfoLeakageResourceMatchCaveat, audRel.OptionalCaveat.CaveatName)
	assert.Equal(t, "linear_issue", audRel.OptionalCaveat.Context.Fields["resource_type"].GetStringValue())
	assert.Equal(t, "ABC-123", audRel.OptionalCaveat.Context.Fields["resource_id"].GetStringValue())

	// Expiration: approximately now+ttl.
	require.NotNil(t, audRel.OptionalExpiresAt, "audience_subject must have an expiration")
	gotExp := audRel.OptionalExpiresAt.AsTime()
	wantMin := before.Add(ttl).Add(-2 * time.Second)
	wantMax := time.Now().Add(ttl).Add(2 * time.Second)
	assert.WithinRange(t, gotExp, wantMin, wantMax, "expiration should be roughly now+ttl")
}

func TestWriteInfoLeakageGrants_MultiSubjectMultiResource(t *testing.T) {
	fw := &fakeGrantWriter{}
	ttl := 30 * time.Minute

	subjects := []string{"alice", "bob"}
	resources := []LeakageGrantResource{
		{Type: "linear_issue", ID: "ABC-123"},
		{Type: "github_pr", ID: "myrepo/42"},
	}

	grantID, err := WriteInfoLeakageGrants(
		context.Background(),
		fw,
		"ns1", "sess-2",
		subjects,
		resources,
		ttl,
	)
	require.NoError(t, err)
	assert.NotEmpty(t, grantID)

	require.Len(t, fw.writes, 1)
	updates := fw.writes[0].Updates

	// 1 session + 2 subjects × 2 resources = 5 updates.
	require.Len(t, updates, 5, "expected 1 session + N×M audience_subject updates")

	// Session update is first.
	assert.Equal(t, "session", updates[0].Relationship.Relation)
	assert.Equal(t, "ns1/sess-2", updates[0].Relationship.Subject.Object.ObjectId)

	// All remaining updates are audience_subject, one per (subject, resource) pair.
	// Collect (user, resource_type, resource_id) triples from caveat contexts.
	type triple struct{ user, resType, resID string }
	got := make([]triple, 0, 4)
	for _, upd := range updates[1:] {
		rel := upd.Relationship
		require.Equal(t, "audience_subject", rel.Relation)
		require.NotNil(t, rel.OptionalCaveat)
		require.NotNil(t, rel.OptionalExpiresAt)
		got = append(got, triple{
			user:    rel.Subject.Object.ObjectId,
			resType: rel.OptionalCaveat.Context.Fields["resource_type"].GetStringValue(),
			resID:   rel.OptionalCaveat.Context.Fields["resource_id"].GetStringValue(),
		})
	}

	// Verify all expected combinations are present (order is deterministic:
	// outer=subjects, inner=resources).
	want := []triple{
		{"alice", "linear_issue", "ABC-123"},
		{"alice", "github_pr", "myrepo/42"},
		{"bob", "linear_issue", "ABC-123"},
		{"bob", "github_pr", "myrepo/42"},
	}
	assert.Equal(t, want, got)
}

func TestWriteInfoLeakageGrants_EmptySubjectsErrors(t *testing.T) {
	fw := &fakeGrantWriter{}
	_, err := WriteInfoLeakageGrants(
		context.Background(),
		fw,
		"default", "sess-1",
		nil,
		[]LeakageGrantResource{{Type: "t", ID: "id"}},
		time.Minute,
	)
	require.Error(t, err, "empty audienceSubjects must return an error")
	assert.Empty(t, fw.writes, "no writes expected on validation error")
}

func TestWriteInfoLeakageGrants_EmptyResourcesErrors(t *testing.T) {
	fw := &fakeGrantWriter{}
	_, err := WriteInfoLeakageGrants(
		context.Background(),
		fw,
		"default", "sess-1",
		[]string{"alice"},
		nil,
		time.Minute,
	)
	require.Error(t, err, "empty resources must return an error")
	assert.Empty(t, fw.writes, "no writes expected on validation error")
}

func TestWriteInfoLeakageGrants_GrantIDIsUnique(t *testing.T) {
	fw := &fakeGrantWriter{}
	ttl := time.Minute
	id1, err := WriteInfoLeakageGrants(context.Background(), fw, "default", "sess-1",
		[]string{"alice"}, []LeakageGrantResource{{Type: "t", ID: "id"}}, ttl)
	require.NoError(t, err)
	id2, err := WriteInfoLeakageGrants(context.Background(), fw, "default", "sess-1",
		[]string{"alice"}, []LeakageGrantResource{{Type: "t", ID: "id"}}, ttl)
	require.NoError(t, err)
	assert.NotEqual(t, id1, id2, "each call must produce a distinct grant ID")
}

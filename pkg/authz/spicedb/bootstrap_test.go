package spicedb_test

import (
	"context"
	"testing"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
)

// recorder is a fake guardian/grants.Writer.
type recorder struct {
	writes  []*v1.WriteRelationshipsRequest
	deletes []*v1.DeleteRelationshipsRequest
}

func (r *recorder) WriteRelationships(ctx context.Context, req *v1.WriteRelationshipsRequest) (*v1.WriteRelationshipsResponse, error) {
	r.writes = append(r.writes, req)
	return &v1.WriteRelationshipsResponse{}, nil
}

func (r *recorder) DeleteRelationships(ctx context.Context, req *v1.DeleteRelationshipsRequest) (*v1.DeleteRelationshipsResponse, error) {
	r.deletes = append(r.deletes, req)
	return &v1.DeleteRelationshipsResponse{}, nil
}

func TestTouchBootstrapRelationship_DirectSubject(t *testing.T) {
	rec := &recorder{}
	tup := spicedb.Tuple{
		ResourceType: "group", ResourceID: "engineering",
		Relation:    "member",
		SubjectType: "user", SubjectID: "abc123",
	}
	require.NoError(t, spicedb.TouchBootstrapRelationshipVia(context.Background(), rec, tup))

	require.Len(t, rec.writes, 1, "exactly one write")
	upd := rec.writes[0].Updates
	require.Len(t, upd, 1, "one update")
	assert.Equal(t, v1.RelationshipUpdate_OPERATION_TOUCH, upd[0].Operation)
	rel := upd[0].Relationship
	assert.Equal(t, "group", rel.Resource.ObjectType)
	assert.Equal(t, "engineering", rel.Resource.ObjectId)
	assert.Equal(t, "member", rel.Relation)
	assert.Equal(t, "user", rel.Subject.Object.ObjectType)
	assert.Equal(t, "abc123", rel.Subject.Object.ObjectId)
	assert.Equal(t, "", rel.Subject.OptionalRelation)
}

func TestTouchBootstrapRelationship_SubjectSet(t *testing.T) {
	rec := &recorder{}
	tup := spicedb.Tuple{
		ResourceType: "agentsession", ResourceID: "default/foo",
		Relation:    "participant",
		SubjectType: "group", SubjectID: "eng", SubjectRelation: "member",
	}
	require.NoError(t, spicedb.TouchBootstrapRelationshipVia(context.Background(), rec, tup))

	rel := rec.writes[0].Updates[0].Relationship
	assert.Equal(t, "group", rel.Subject.Object.ObjectType)
	assert.Equal(t, "eng", rel.Subject.Object.ObjectId)
	assert.Equal(t, "member", rel.Subject.OptionalRelation)
}

func TestDeleteBootstrapRelationship_FilterShape(t *testing.T) {
	rec := &recorder{}
	tup := spicedb.Tuple{
		ResourceType: "group", ResourceID: "engineering",
		Relation:    "member",
		SubjectType: "user", SubjectID: "abc123",
	}
	require.NoError(t, spicedb.DeleteBootstrapRelationshipVia(context.Background(), rec, tup))

	require.Len(t, rec.deletes, 1)
	f := rec.deletes[0].RelationshipFilter
	assert.Equal(t, "group", f.ResourceType)
	assert.Equal(t, "engineering", f.OptionalResourceId)
	assert.Equal(t, "member", f.OptionalRelation)
	require.NotNil(t, f.OptionalSubjectFilter)
	assert.Equal(t, "user", f.OptionalSubjectFilter.SubjectType)
	assert.Equal(t, "abc123", f.OptionalSubjectFilter.OptionalSubjectId)
	assert.Nil(t, f.OptionalSubjectFilter.OptionalRelation)
}

func TestDeleteBootstrapRelationship_SubjectSetFilter(t *testing.T) {
	rec := &recorder{}
	tup := spicedb.Tuple{
		ResourceType: "agentsession", ResourceID: "default/foo",
		Relation:    "participant",
		SubjectType: "group", SubjectID: "eng", SubjectRelation: "member",
	}
	require.NoError(t, spicedb.DeleteBootstrapRelationshipVia(context.Background(), rec, tup))

	sf := rec.deletes[0].RelationshipFilter.OptionalSubjectFilter
	require.NotNil(t, sf.OptionalRelation)
	assert.Equal(t, "member", sf.OptionalRelation.Relation)
}

func TestTuple_Key_Stable(t *testing.T) {
	a := spicedb.Tuple{
		ResourceType: "group", ResourceID: "eng",
		Relation:    "member",
		SubjectType: "user", SubjectID: "u1",
	}
	b := a
	assert.Equal(t, a.Key(), b.Key())
	c := a
	c.SubjectID = "u2"
	assert.NotEqual(t, a.Key(), c.Key())
}

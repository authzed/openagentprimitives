package relwrites

import (
	"context"
	"errors"
	"fmt"
	"testing"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeSpiceDBClient records the requests it receives and optionally
// returns a canned error. Same pattern as pkg/authz/guardian/grants's
// recorder double — minimal surface, no gRPC option plumbing.
type fakeSpiceDBClient struct {
	got []*v1.WriteRelationshipsRequest
	err error
}

func (f *fakeSpiceDBClient) WriteRelationships(_ context.Context, req *v1.WriteRelationshipsRequest) (*v1.WriteRelationshipsResponse, error) {
	f.got = append(f.got, req)
	return &v1.WriteRelationshipsResponse{}, f.err
}

func TestSpiceDBWriter_TranslatesTuples(t *testing.T) {
	c := &fakeSpiceDBClient{}
	w := &SpiceDBWriter{Client: c}
	err := w.WriteRelationships(context.Background(), []ResolvedTuple{
		{Resource: "crm_company:5083", Relation: "owner", Subject: "user:alice"},
	})
	require.NoError(t, err)
	require.Len(t, c.got, 1, "want one update request")
	require.Len(t, c.got[0].Updates, 1)
	u := c.got[0].Updates[0]
	assert.Equal(t, v1.RelationshipUpdate_OPERATION_TOUCH, u.Operation)
	assert.Equal(t, "crm_company", u.Relationship.Resource.ObjectType)
	assert.Equal(t, "5083", u.Relationship.Resource.ObjectId)
	assert.Equal(t, "owner", u.Relationship.Relation)
	assert.Equal(t, "user", u.Relationship.Subject.Object.ObjectType)
	assert.Equal(t, "alice", u.Relationship.Subject.Object.ObjectId)
}

func TestSpiceDBWriter_BatchesMultipleTuples(t *testing.T) {
	c := &fakeSpiceDBClient{}
	w := &SpiceDBWriter{Client: c}
	err := w.WriteRelationships(context.Background(), []ResolvedTuple{
		{Resource: "crm_company:1", Relation: "owner", Subject: "user:a"},
		{Resource: "crm_company:2", Relation: "owner", Subject: "user:b"},
		{Resource: "crm_company:3", Relation: "owner", Subject: "user:c"},
	})
	require.NoError(t, err)
	require.Len(t, c.got, 1, "want one batched RPC")
	assert.Len(t, c.got[0].Updates, 3)
}

func TestSpiceDBWriter_EmptyInputNoRPC(t *testing.T) {
	c := &fakeSpiceDBClient{}
	w := &SpiceDBWriter{Client: c}
	require.NoError(t, w.WriteRelationships(context.Background(), nil))
	assert.Empty(t, c.got, "want zero RPCs for empty input")
}

func TestSpiceDBWriter_Errors(t *testing.T) {
	cases := []struct {
		name      string
		clientErr error
		tuples    []ResolvedTuple
		errSubstr string
	}{
		{
			name:      "malformed resource (no colon): bad resource",
			tuples:    []ResolvedTuple{{Resource: "no-colon", Relation: "r", Subject: "user:alice"}},
			errSubstr: "bad resource",
		},
		{
			name:      "malformed subject (no colon): bad subject",
			tuples:    []ResolvedTuple{{Resource: "a:1", Relation: "r", Subject: "no-colon"}},
			errSubstr: "bad subject",
		},
		{
			name:      "RPC error propagated to caller",
			clientErr: fmt.Errorf("rpc broken"),
			tuples:    []ResolvedTuple{{Resource: "a:1", Relation: "r", Subject: "b:1"}},
			errSubstr: "rpc broken",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := &SpiceDBWriter{Client: &fakeSpiceDBClient{err: tc.clientErr}}
			err := w.WriteRelationships(context.Background(), tc.tuples)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.errSubstr)
		})
	}
}

// TestSpiceDBWriter_ExclusiveAttachesMustNotMatchPrecondition proves an
// Exclusive tuple adds a MUST_NOT_MATCH precondition (subject holds `relation`
// on ANY resource id of that type → resource id left unset) while a
// non-exclusive tuple in the same batch adds none. This is the atomic
// write-once source of truth for the session-pin.
func TestSpiceDBWriter_ExclusiveAttachesMustNotMatchPrecondition(t *testing.T) {
	c := &fakeSpiceDBClient{}
	w := &SpiceDBWriter{Client: c}
	err := w.WriteRelationships(context.Background(), []ResolvedTuple{
		{Resource: "cluster:a", Relation: "debug_target", Subject: "agentsession:ns/s", Exclusive: true},
	})
	require.NoError(t, err)
	require.Len(t, c.got, 1, "one RPC")
	require.Len(t, c.got[0].OptionalPreconditions, 1, "exclusive tuple → one precondition")
	pc := c.got[0].OptionalPreconditions[0]
	assert.Equal(t, v1.Precondition_OPERATION_MUST_NOT_MATCH, pc.Operation)
	require.NotNil(t, pc.Filter)
	assert.Equal(t, "cluster", pc.Filter.ResourceType)
	assert.Equal(t, "debug_target", pc.Filter.OptionalRelation)
	assert.Empty(t, pc.Filter.OptionalResourceId, "resource id must be UNSET so the filter matches ANY resource of that type")
	require.NotNil(t, pc.Filter.OptionalSubjectFilter)
	assert.Equal(t, "agentsession", pc.Filter.OptionalSubjectFilter.SubjectType)
	assert.Equal(t, "ns/s", pc.Filter.OptionalSubjectFilter.OptionalSubjectId)
	// The update itself is still a TOUCH of the requested tuple.
	require.Len(t, c.got[0].Updates, 1)
	assert.Equal(t, v1.RelationshipUpdate_OPERATION_TOUCH, c.got[0].Updates[0].Operation)
}

func TestSpiceDBWriter_NonExclusiveHasNoPrecondition(t *testing.T) {
	c := &fakeSpiceDBClient{}
	w := &SpiceDBWriter{Client: c}
	err := w.WriteRelationships(context.Background(), []ResolvedTuple{
		{Resource: "cluster:a", Relation: "debug_target", Subject: "agentsession:ns/s"},
	})
	require.NoError(t, err)
	require.Len(t, c.got, 1)
	assert.Empty(t, c.got[0].OptionalPreconditions, "non-exclusive tuple → no precondition (plain TOUCH-upsert)")
}

// TestSpiceDBWriter_FailedPreconditionIsWriteOnceConflict proves a
// FAILED_PRECONDITION from SpiceDB (a MUST_NOT_MATCH matched → subject already
// pinned) maps to the distinguishable ErrWriteOnceConflict sentinel, while any
// other gRPC error stays a plain error (NOT a write-once conflict).
func TestSpiceDBWriter_FailedPreconditionIsWriteOnceConflict(t *testing.T) {
	t.Run("FAILED_PRECONDITION → ErrWriteOnceConflict", func(t *testing.T) {
		c := &fakeSpiceDBClient{err: status.Error(codes.FailedPrecondition, "rel to write found matching")}
		w := &SpiceDBWriter{Client: c}
		err := w.WriteRelationships(context.Background(), []ResolvedTuple{
			{Resource: "cluster:b", Relation: "debug_target", Subject: "agentsession:ns/s", Exclusive: true},
		})
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrWriteOnceConflict), "want ErrWriteOnceConflict, got %v", err)
		assert.Contains(t, err.Error(), "cluster:b", "conflict error carries tuple context")
	})
	t.Run("other gRPC error is NOT a write-once conflict", func(t *testing.T) {
		c := &fakeSpiceDBClient{err: status.Error(codes.Unavailable, "backend down")}
		w := &SpiceDBWriter{Client: c}
		err := w.WriteRelationships(context.Background(), []ResolvedTuple{
			{Resource: "cluster:b", Relation: "debug_target", Subject: "agentsession:ns/s", Exclusive: true},
		})
		require.Error(t, err)
		assert.False(t, errors.Is(err, ErrWriteOnceConflict), "non-precondition error must not masquerade as write-once")
	})
}

// Compile-time check that SpiceDBWriter implements Writer.
var _ Writer = (*SpiceDBWriter)(nil)

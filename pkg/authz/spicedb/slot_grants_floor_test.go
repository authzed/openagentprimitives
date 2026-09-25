package spicedb

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
)

// A grant written through RelationsWithFloor must hand the caller the write's
// own WrittenAt ZedToken, keyed by the relations written. This is the seam the
// plan-gate amendment race is fixed at: without it, the in-process slot-grant
// write leaves the session's freshness floor stale, and the very next
// ToolCallAuthz check can read a snapshot that predates the grant and deny the
// call the approval just authorized.
func TestRelationsWithFloor_HandsBackWrittenAtTokenForTheGrant(t *testing.T) {
	cl, rec := newRecordingWriterClient(t)
	rec.writtenAt = "zt-after-grant"
	c := &Client{cl: cl}

	var gotRels []authz.Relation
	var gotTok string
	w := c.RelationsWithFloor(func(rels []authz.Relation, token string) {
		gotRels, gotTok = rels, token
	})

	rels := []authz.Relation{{
		ResourceType: "workshop_draft", ResourceID: "draft", Relation: "slot_grant_remove",
		SubjectType: "agentsession", SubjectID: "default/s1",
	}}
	require.NoError(t, w.WriteRelationships(context.Background(), rels))

	assert.Equal(t, "zt-after-grant", gotTok, "floor hook gets the write's WrittenAt token")
	require.Len(t, gotRels, 1, "floor hook gets the relations written")
	assert.Equal(t, "workshop_draft", gotRels[0].ResourceType)
	assert.Equal(t, "draft", gotRels[0].ResourceID)
}

// The hook must not fire when the write failed or returned no token, and a
// plain Relations() writer (no floor) must keep working untouched.
func TestRelationsWithFloor_NoHookWhenServerReturnsNoToken(t *testing.T) {
	cl, rec := newRecordingWriterClient(t)
	rec.writtenAt = "" // server returns no WrittenAt
	c := &Client{cl: cl}

	called := false
	w := c.RelationsWithFloor(func([]authz.Relation, string) { called = true })
	require.NoError(t, w.WriteRelationships(context.Background(), []authz.Relation{{
		ResourceType: "workshop_draft", ResourceID: "draft", Relation: "slot_grant_remove",
		SubjectType: "agentsession", SubjectID: "default/s1",
	}}))
	assert.False(t, called, "no token means nothing to advance the floor to")
}

func TestRelations_PlainWriterStillWrites(t *testing.T) {
	cl, rec := newRecordingWriterClient(t)
	c := &Client{cl: cl}
	require.NoError(t, c.Relations().WriteRelationships(context.Background(), []authz.Relation{{
		ResourceType: "workshop_draft", ResourceID: "draft", Relation: "slot_grant_remove",
		SubjectType: "agentsession", SubjectID: "default/s1",
	}}))
	require.Len(t, rec.writeReqs, 1, "plain Relations() writer still reaches SpiceDB")
}

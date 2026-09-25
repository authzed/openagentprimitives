// Pure-Go unit test (no build tag, no real SpiceDB) pinning the wire SHAPE of
// SyncArtifactOrgViewer, the level-triggered writer for the opt-in org-wide
// artifact audience:
//   - enabled=true  → WriteRelationships TOUCH
//     agentsession:<ns>/<name>#artifact_org_viewer@user:*
//   - enabled=false → DeleteRelationships filtered to exactly that resource +
//     relation (idempotent: deleting an absent tuple succeeds, which is the
//     hot path — every session whose class never opted in levels to "absent"
//     on each reconcile).
//
// The wildcard subject is the whole point: the relation admits ONLY user:*,
// and webd's IdP-gated login is what scopes "any user" to the corp directory.
package spicedb

import (
	"context"
	"testing"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	authzed "github.com/authzed/authzed-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// fakeOrgViewerPermClient captures the write and delete requests; any other
// method call panics on the nil embed — this test must only exercise the two
// sync paths.
type fakeOrgViewerPermClient struct {
	v1.PermissionsServiceClient
	capturedWrite  *v1.WriteRelationshipsRequest
	capturedDelete *v1.DeleteRelationshipsRequest
}

func (f *fakeOrgViewerPermClient) WriteRelationships(_ context.Context, in *v1.WriteRelationshipsRequest, _ ...grpc.CallOption) (*v1.WriteRelationshipsResponse, error) {
	f.capturedWrite = in
	return &v1.WriteRelationshipsResponse{}, nil
}

func (f *fakeOrgViewerPermClient) DeleteRelationships(_ context.Context, in *v1.DeleteRelationshipsRequest, _ ...grpc.CallOption) (*v1.DeleteRelationshipsResponse, error) {
	f.capturedDelete = in
	return &v1.DeleteRelationshipsResponse{}, nil
}

func TestSyncArtifactOrgViewer_EnabledTouchesWildcard(t *testing.T) {
	fake := &fakeOrgViewerPermClient{}
	c := &Client{cl: &authzed.Client{PermissionsServiceClient: fake}}

	require.NoError(t, c.SyncArtifactOrgViewer(context.Background(), "integration-test", "sess-1", true))
	require.Nil(t, fake.capturedDelete, "enabling must not issue a delete")
	require.NotNil(t, fake.capturedWrite, "enabling must call WriteRelationships")

	updates := fake.capturedWrite.GetUpdates()
	require.Len(t, updates, 1, "enabling must issue exactly one update")

	u := updates[0]
	assert.Equal(t, v1.RelationshipUpdate_OPERATION_TOUCH, u.GetOperation(), "the org-viewer write must be a TOUCH (idempotent per reconcile)")
	rel := u.GetRelationship()
	assert.Equal(t, "agentsession", rel.GetResource().GetObjectType())
	assert.Equal(t, "integration-test/sess-1", rel.GetResource().GetObjectId())
	assert.Equal(t, "artifact_org_viewer", rel.GetRelation())
	assert.Equal(t, "user", rel.GetSubject().GetObject().GetObjectType())
	assert.Equal(t, "*", rel.GetSubject().GetObject().GetObjectId(), "the subject must be the wildcard — org-wide is session-wide, not per-user")
}

func TestSyncArtifactOrgViewer_DisabledDeletesByFilter(t *testing.T) {
	fake := &fakeOrgViewerPermClient{}
	c := &Client{cl: &authzed.Client{PermissionsServiceClient: fake}}

	require.NoError(t, c.SyncArtifactOrgViewer(context.Background(), "integration-test", "sess-1", false))
	require.Nil(t, fake.capturedWrite, "disabling must not issue a write")
	require.NotNil(t, fake.capturedDelete, "disabling must call DeleteRelationships")

	f := fake.capturedDelete.GetRelationshipFilter()
	require.NotNil(t, f)
	assert.Equal(t, "agentsession", f.GetResourceType())
	assert.Equal(t, "integration-test/sess-1", f.GetOptionalResourceId())
	assert.Equal(t, "artifact_org_viewer", f.GetOptionalRelation(),
		"the delete must be scoped to the org-viewer relation only — a wider filter would sweep owner/participant tuples")
}

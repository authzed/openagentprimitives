// Pure-Go unit test (no build tag, no real SpiceDB) pinning the tuple SHAPE
// written by TouchArtifactParent. It captures the WriteRelationshipsRequest via
// a fake PermissionsServiceClient and asserts BOTH updates are present:
//   - artifact:<id>#parent@agentsession:<ns>/<name>
//   - artifact:<id>#platform@platform:platform
//
// The #platform update is what ties every artifact to the singleton platform
// object so platform admins can view any artifact (artifact#view =
// parent->interact + parent->artifact_org_view + platform->view_audit). A
// regression that drops it is
// silent in the schema and only surfaces as "admin can't see this artifact",
// so we pin it here at the single write funnel.
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

// fakePermClient embeds the PermissionsServiceClient interface (left nil) and
// overrides only WriteRelationships, capturing the request. Any other method
// call would panic on the nil embed — intentional: this test must only exercise
// the write path.
type fakePermClient struct {
	v1.PermissionsServiceClient
	captured *v1.WriteRelationshipsRequest
}

func (f *fakePermClient) WriteRelationships(_ context.Context, in *v1.WriteRelationshipsRequest, _ ...grpc.CallOption) (*v1.WriteRelationshipsResponse, error) {
	f.captured = in
	return &v1.WriteRelationshipsResponse{}, nil
}

func TestTouchArtifactParent_WritesParentAndPlatform(t *testing.T) {
	fake := &fakePermClient{}
	c := &Client{cl: &authzed.Client{PermissionsServiceClient: fake}}

	require.NoError(t, c.TouchArtifactParent(context.Background(), "artifact-abc123", "integration-test", "sess-1"))
	require.NotNil(t, fake.captured, "WriteRelationships must have been called")

	updates := fake.captured.GetUpdates()
	require.Len(t, updates, 2, "TouchArtifactParent must issue exactly two updates (#parent and #platform)")

	// Index the updates by (resource, relation, subject) so order is not pinned.
	type tuple struct{ res, rel, sub string }
	got := map[tuple]v1.RelationshipUpdate_Operation{}
	for _, u := range updates {
		rel := u.GetRelationship()
		res := rel.GetResource()
		sub := rel.GetSubject().GetObject()
		got[tuple{
			res: res.GetObjectType() + ":" + res.GetObjectId(),
			rel: rel.GetRelation(),
			sub: sub.GetObjectType() + ":" + sub.GetObjectId(),
		}] = u.GetOperation()
	}

	parentKey := tuple{"artifact:artifact-abc123", "parent", "agentsession:integration-test/sess-1"}
	platformKey := tuple{"artifact:artifact-abc123", "platform", "platform:platform"}

	op, ok := got[parentKey]
	require.True(t, ok, "must write artifact#parent@agentsession:<ns>/<name>; got %+v", got)
	assert.Equal(t, v1.RelationshipUpdate_OPERATION_TOUCH, op, "artifact#parent must be a TOUCH")

	op, ok = got[platformKey]
	require.True(t, ok, "must write artifact#platform@platform:platform; got %+v", got)
	assert.Equal(t, v1.RelationshipUpdate_OPERATION_TOUCH, op, "artifact#platform must be a TOUCH")
}

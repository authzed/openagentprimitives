//go:build integration

package bootstrap_test

import (
	"context"
	"testing"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/authzed/grpcutil"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	spicedbschema "github.com/authzed/openagentprimitives/pkg/authz/spicedb/schema"
	"github.com/authzed/openagentprimitives/pkg/platform/bootstrap"
	"github.com/authzed/openagentprimitives/test/testspicedb"
)

// fragment stands in for a guardian-composed definition (an MCPServer or
// toolkit fragment): present in the live schema, absent from the scaffold.
const fragment = "definition fixture_fragment {\n\trelation viewer: user\n}"

func newSchemaIO(t *testing.T, endpoint, token string) spicedb.SchemaIOAdapter {
	t.Helper()
	c, err := spicedb.NewClient(endpoint, token, true)
	require.NoError(t, err, "NewClient")
	t.Cleanup(func() { _ = c.Close() })
	return spicedb.SchemaIOFor(c)
}

func permsClient(t *testing.T, endpoint, token string) v1.PermissionsServiceClient {
	t.Helper()
	conn, err := grpc.NewClient(endpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpcutil.WithInsecureBearerToken(token),
	)
	require.NoError(t, err, "dial spicedb")
	t.Cleanup(func() { _ = conn.Close() })
	return v1.NewPermissionsServiceClient(conn)
}

func TestRunBootstrapSpiceDB_LeavesComposedSchemaAndRelationships(t *testing.T) {
	ctx := context.Background()
	endpoint := testspicedb.Endpoint(t)
	token := testspicedb.UniqueToken(t)
	testspicedb.WriteSchemaWithExtra(t, endpoint, token, fragment)

	rel := &v1.Relationship{
		Resource: &v1.ObjectReference{ObjectType: "fixture_fragment", ObjectId: "one"},
		Relation: "viewer",
		Subject:  &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: "alice"}},
	}
	perms := permsClient(t, endpoint, token)
	_, err := perms.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{
		Updates: []*v1.RelationshipUpdate{{Operation: v1.RelationshipUpdate_OPERATION_TOUCH, Relationship: rel}},
	})
	require.NoError(t, err, "write relationship on the fragment definition")

	io := newSchemaIO(t, endpoint, token)
	require.NoError(t, bootstrap.RunBootstrap(ctx, logr.Discard(), io, spicedbschema.Schema))

	live, err := io.ReadSchema(ctx)
	require.NoError(t, err, "ReadSchema after bootstrap")
	assert.Contains(t, live, "definition fixture_fragment", "guardian-composed definition survives the bootstrap")

	stream, err := perms.ReadRelationships(ctx, &v1.ReadRelationshipsRequest{
		Consistency:        &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}},
		RelationshipFilter: &v1.RelationshipFilter{ResourceType: "fixture_fragment"},
	})
	require.NoError(t, err, "ReadRelationships")
	got, err := stream.Recv()
	require.NoError(t, err, "relationship on the fragment definition survives the bootstrap")
	assert.Equal(t, "alice", got.GetRelationship().GetSubject().GetObject().GetObjectId())
}

func TestRunBootstrapSpiceDB_SeedsEmptyDatastore(t *testing.T) {
	ctx := context.Background()
	endpoint := testspicedb.Endpoint(t)
	io := newSchemaIO(t, endpoint, testspicedb.UniqueToken(t))

	require.NoError(t, bootstrap.RunBootstrap(ctx, logr.Discard(), io, spicedbschema.Schema))

	live, err := io.ReadSchema(ctx)
	require.NoError(t, err, "an empty datastore is seeded")
	assert.Contains(t, live, "definition user", "scaffold written")
}

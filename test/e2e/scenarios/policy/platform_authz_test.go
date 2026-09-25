//go:build e2e

package policy_test

import (
	"context"
	"testing"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	authzed "github.com/authzed/authzed-go/v1"
	"github.com/authzed/grpcutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	insecuregrpc "google.golang.org/grpc/credentials/insecure"

	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	apschema "github.com/authzed/openagentprimitives/pkg/authz/spicedb/schema"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/test/testspicedb"
)

// TestPlatformSchema_PerAreaPermissionsAliasAdmin: writing
// platform:platform#admin@user:<id> grants every per-area permission;
// a non-admin user has none of them.
func TestPlatformSchema_PerAreaPermissionsAliasAdmin(t *testing.T) {
	ctx := context.Background()
	endpoint := testspicedb.SharedEndpoint(t)
	token := testspicedb.UniqueToken(t)

	cl, err := authzed.NewClient(endpoint,
		grpcutil.WithInsecureBearerToken(token),
		grpc.WithTransportCredentials(insecuregrpc.NewCredentials()))
	require.NoError(t, err, "dial spicedb")
	t.Cleanup(func() { _ = cl.Close() })

	_, err = cl.WriteSchema(ctx, &v1.WriteSchemaRequest{Schema: apschema.Schema})
	require.NoError(t, err, "embedded schema must be standalone-valid incl. platform")

	_, err = cl.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{
		Updates: []*v1.RelationshipUpdate{{
			Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: &v1.Relationship{
				Resource: &v1.ObjectReference{ObjectType: "platform", ObjectId: "platform"},
				Relation: "admin",
				Subject: &v1.SubjectReference{Object: &v1.ObjectReference{
					ObjectType: "user", ObjectId: "YWxpY2VAZXhhbXBsZS5jb20"}},
			},
		}},
	})
	require.NoError(t, err, "write platform admin relationship")

	check := func(permission, userID string) bool {
		t.Helper()
		resp, err := cl.CheckPermission(ctx, &v1.CheckPermissionRequest{
			Resource:    &v1.ObjectReference{ObjectType: "platform", ObjectId: "platform"},
			Permission:  permission,
			Subject:     &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: userID}},
			Consistency: &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}},
		})
		require.NoError(t, err, "check %s", permission)
		return resp.GetPermissionship() == v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION
	}

	for _, perm := range []string{"can_admin", "view_overview", "view_live", "view_sessions", "view_audit", "view_config", "kill_session"} {
		assert.True(t, check(perm, "YWxpY2VAZXhhbXBsZS5jb20"), "admin must have %s", perm)
		assert.False(t, check(perm, "Ym9iQGV4YW1wbGUuY29t"), "non-admin must NOT have %s", perm)
	}
}

// TestPlatformClientHelpers: the pkg/authz/spicedb wrappers round-trip —
// Touch → Check true + List contains; Delete → Check false + List empty.
func TestPlatformClientHelpers(t *testing.T) {
	ctx := context.Background()
	endpoint := testspicedb.SharedEndpoint(t)
	token := testspicedb.UniqueToken(t)

	// Raw client to install the schema into this test's datastore.
	raw, err := authzed.NewClient(endpoint,
		grpcutil.WithInsecureBearerToken(token),
		grpc.WithTransportCredentials(insecuregrpc.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = raw.Close() })
	_, err = raw.WriteSchema(ctx, &v1.WriteSchemaRequest{Schema: apschema.Schema})
	require.NoError(t, err)

	cl, err := spicedb.NewClient(endpoint, token, true)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cl.Close() })

	const alice = "YWxpY2VAZXhhbXBsZS5jb20"

	require.NoError(t, cl.TouchPlatformAdmin(ctx, identity.CanonicalFromTrusted(alice, "test fixture")))

	ok, err := cl.CheckPlatformPermission(ctx, "view_sessions", identity.CanonicalFromTrusted(alice, "test fixture"), true)
	require.NoError(t, err)
	assert.True(t, ok, "granted admin must pass view_sessions")

	admins, err := cl.ListPlatformAdmins(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"user:" + alice}, admins)

	require.NoError(t, cl.DeletePlatformAdmin(ctx, identity.CanonicalFromTrusted(alice, "test fixture")))
	ok, err = cl.CheckPlatformPermission(ctx, "view_sessions", identity.CanonicalFromTrusted(alice, "test fixture"), true)
	require.NoError(t, err)
	assert.False(t, ok, "revoked admin must fail view_sessions")
	admins, err = cl.ListPlatformAdmins(ctx)
	require.NoError(t, err)
	assert.Empty(t, admins)
}

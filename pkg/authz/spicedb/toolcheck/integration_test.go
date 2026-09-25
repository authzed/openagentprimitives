//go:build integration

// Integration tests for toolcheck.Checker.CheckToolCall against a real SpiceDB instance.
// Requires Docker to spin up an in-process SpiceDB container via
// testspicedb. Run with:
//
//	go test -tags=integration -count=1 ./pkg/authz/spicedb/toolcheck/
//
// Each test gets its own isolated datastore via a unique bearer token
// (serve-testing keys datastores by token).
package toolcheck_test

import (
	"context"
	"testing"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/authzed/grpcutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/toolcheck"
	"github.com/authzed/openagentprimitives/test/testspicedb"
)

const integrationSchema = `
definition user {}

definition github_repo {
    relation reader: user
    relation writer: user
    permission read = reader + writer
    permission write = writer
}
`

// newAuthzClient creates a *spicedb.Client that satisfies toolcheck.Client
// for the given endpoint and bearer token.
func newAuthzClient(t *testing.T, endpoint, token string) *spicedb.Client {
	t.Helper()
	c, err := spicedb.NewClient(endpoint, token, true /* insecure */)
	require.NoError(t, err, "spicedb.NewClient")
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// permServiceClient returns a raw PermissionsServiceClient for seeding
// test relationships. Uses the same token as the authz client so both
// see the same serve-testing datastore.
func permServiceClient(t *testing.T, endpoint, token string) v1.PermissionsServiceClient {
	t.Helper()
	conn, err := grpc.NewClient(endpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpcutil.WithInsecureBearerToken(token),
	)
	require.NoError(t, err, "grpc.NewClient")
	t.Cleanup(func() { _ = conn.Close() })
	return v1.NewPermissionsServiceClient(conn)
}

// writeTestSchema writes schemaText to the SpiceDB datastore keyed by token.
func writeTestSchema(t *testing.T, endpoint, token, schemaText string) {
	t.Helper()
	conn, err := grpc.NewClient(endpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpcutil.WithInsecureBearerToken(token),
	)
	require.NoError(t, err, "dial spicedb for schema write")
	t.Cleanup(func() { _ = conn.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = v1.NewSchemaServiceClient(conn).WriteSchema(ctx, &v1.WriteSchemaRequest{
		Schema: schemaText,
	})
	require.NoError(t, err, "write schema")
}

func TestRunnerAuthz_ReadonlyAllow(t *testing.T) {
	endpoint := testspicedb.SharedEndpoint(t)
	token := testspicedb.UniqueToken(t)
	writeTestSchema(t, endpoint, token, integrationSchema)

	cli := newAuthzClient(t, endpoint, token)
	perm := permServiceClient(t, endpoint, token)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Grant alice reader on repo:foo.
	_, err := perm.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{
		Updates: []*v1.RelationshipUpdate{{
			Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: &v1.Relationship{
				Resource: &v1.ObjectReference{ObjectType: "github_repo", ObjectId: "foo"},
				Relation: "reader",
				Subject:  &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: "alice"}},
			},
		}},
	})
	require.NoError(t, err, "WriteRelationships")

	res := toolcheck.Checker{Cli: cli}.CheckToolCall(ctx,
		authz.Permission{
			StateImpact: authz.Readonly,
			Check: &authz.PermissionCheck{
				ResourceType: "github_repo", ResourceIDTemplate: "{repo}", Permission: "read",
			},
		},
		authz.Inputs{
			Args:    map[string]any{"repo": "foo"},
			Subject: "alice",
		},
	)
	assert.Equal(t, authz.OutcomeAllowed, res.Outcome, "expected allow; message=%q", res.Message)
}

func TestRunnerAuthz_ReadwriteDeny(t *testing.T) {
	endpoint := testspicedb.SharedEndpoint(t)
	token := testspicedb.UniqueToken(t)
	writeTestSchema(t, endpoint, token, integrationSchema)

	cli := newAuthzClient(t, endpoint, token)
	perm := permServiceClient(t, endpoint, token)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// alice has reader, but tool needs `write` (writer-only).
	_, err := perm.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{
		Updates: []*v1.RelationshipUpdate{{
			Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: &v1.Relationship{
				Resource: &v1.ObjectReference{ObjectType: "github_repo", ObjectId: "foo"},
				Relation: "reader",
				Subject:  &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: "alice"}},
			},
		}},
	})
	require.NoError(t, err, "WriteRelationships")

	res := toolcheck.Checker{Cli: cli}.CheckToolCall(ctx,
		authz.Permission{
			StateImpact: authz.Readwrite,
			Check: &authz.PermissionCheck{
				ResourceType: "github_repo", ResourceIDTemplate: "{repo}", Permission: "write",
			},
		},
		authz.Inputs{
			Args:    map[string]any{"repo": "foo"},
			Subject: "alice",
		},
	)
	assert.Equal(t, authz.OutcomeDenied, res.Outcome, "expected deny")
	assert.Contains(t, res.Message, "alice", "message should name the failing subject")
}

func TestRunnerAuthz_ZedTokenFreshness(t *testing.T) {
	endpoint := testspicedb.SharedEndpoint(t)
	token := testspicedb.UniqueToken(t)
	writeTestSchema(t, endpoint, token, integrationSchema)

	cli := newAuthzClient(t, endpoint, token)
	perm := permServiceClient(t, endpoint, token)
	cache := toolcheck.NewZedTokenCache()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Write writer for alice; capture ZedToken; cache it.
	wresp, err := perm.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{
		Updates: []*v1.RelationshipUpdate{{
			Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: &v1.Relationship{
				Resource: &v1.ObjectReference{ObjectType: "github_repo", ObjectId: "foo"},
				Relation: "writer",
				Subject:  &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: "alice"}},
			},
		}},
	})
	require.NoError(t, err, "WriteRelationships")
	cache.Set("github_repo", "foo", wresp.WrittenAt.Token)

	res := toolcheck.Checker{Cli: cli, Cache: cache}.CheckToolCall(ctx,
		authz.Permission{
			StateImpact: authz.Readwrite,
			Check: &authz.PermissionCheck{
				ResourceType: "github_repo", ResourceIDTemplate: "{repo}", Permission: "write",
			},
		},
		authz.Inputs{
			Args:    map[string]any{"repo": "foo"},
			Subject: "alice",
		},
	)
	assert.Equal(t, authz.OutcomeAllowed, res.Outcome, "expected allow with fresh ZedToken; message=%q", res.Message)
}

// approverSchema models the owner-derived approver inputs: a session whose
// approve-set is its owners, and a resource whose owners are consulted as the
// per-gate source. ResolveApprovers intersects the two.
const approverSchema = `
definition user {}

definition agentsession {
	relation owner: user
	permission approve = owner
}

definition crm_resource {
	relation owner: user
}
`

// TestIntegration_ResolveApprovers_SharedAndEmpty exercises the owner-derived
// approver model end to end against real SpiceDB. With a resource owner-set
// present, eligible approvers are the RESOURCE's owners — session standing is
// neither required nor sufficient (see the rationale on authz.ResolveApprovers):
//
//   - a resource owner who is NOT a session owner is eligible (the normal
//     production shape: the requester is the sole session owner and someone
//     else owns the resource);
//   - a session owner who does not own the resource is NOT eligible
//     (no self-approval of access to someone else's resource);
//   - an unowned resource yields nobody → empty=true, which the tool-call and
//     info-leakage gates surface as the fail-closed "no one has standing".
//
// This is the unit of authorization the e2e approval scenarios (centerdot
// owner-approval, leakage approve/deny) rely on; locking it here keeps the
// model honest without booting the full pipeline per case.
func TestIntegration_ResolveApprovers_SharedAndEmpty(t *testing.T) {
	endpoint := testspicedb.SharedEndpoint(t)
	token := testspicedb.UniqueToken(t)
	writeTestSchema(t, endpoint, token, approverSchema)

	cli := newAuthzClient(t, endpoint, token)
	perm := permServiceClient(t, endpoint, token)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	write := func(resType, resID, userID string) {
		t.Helper()
		_, err := perm.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{
			Updates: []*v1.RelationshipUpdate{{
				Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
				Relationship: &v1.Relationship{
					Resource: &v1.ObjectReference{ObjectType: resType, ObjectId: resID},
					Relation: "owner",
					Subject:  &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: userID}},
				},
			}},
		})
		require.NoError(t, err, "write %s:%s#owner@user:%s", resType, resID, userID)
	}

	// Production shape: session owned by {requester} only (the owner
	// resolver writes just the requester); resource owned by {louis, other}.
	// The resource owners are eligible — requester is NOT (no self-approval
	// of access to someone else's resource).
	write("agentsession", "s1", "requester")
	write("crm_resource", "r1", "louis")
	write("crm_resource", "r1", "other")

	approvers, empty, err := authz.ResolveApprovers(ctx, cli,
		"agentsession:s1#approve", []string{"crm_resource:r1#owner"})
	require.NoError(t, err, "ResolveApprovers (resource owners)")
	assert.False(t, empty, "resource owners must be eligible even with zero session standing")
	assert.ElementsMatch(t, []string{"louis", "other"}, approvers,
		"the pool is the resource's owners; the requester (session owner) is not in it")

	// No approver: resource r2 has no owners at all → empty (fail-closed
	// "no one has standing"), regardless of the session having an owner.
	write("agentsession", "s2", "requester")

	none, empty2, err := authz.ResolveApprovers(ctx, cli,
		"agentsession:s2#approve", []string{"crm_resource:r2#owner"})
	require.NoError(t, err, "ResolveApprovers (unowned resource)")
	assert.True(t, empty2, "an unowned resource must yield no approver")
	assert.Empty(t, none, "no eligible approvers for an unowned resource")

	// Owner-only gate: with no resource owner-sets the session approve-set
	// is the pool.
	sessOnly, empty3, err := authz.ResolveApprovers(ctx, cli,
		"agentsession:s2#approve", nil)
	require.NoError(t, err, "ResolveApprovers (owner-only)")
	assert.False(t, empty3)
	assert.Equal(t, []string{"requester"}, sessOnly, "owner-only gates keep the session approve-set as the pool")
}

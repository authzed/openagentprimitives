//go:build integration

package schema_test

import (
	"context"
	"encoding/base64"
	"net/http/httptest"
	"testing"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/authzed/grpcutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/schema"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/toolcheck"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/httpclient"
	"github.com/authzed/openagentprimitives/pkg/memory/httpsrv"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/observation"
	"github.com/authzed/openagentprimitives/pkg/memory/search"
	searchinmem "github.com/authzed/openagentprimitives/pkg/memory/search/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
	"github.com/authzed/openagentprimitives/test/testspicedb"
)

// This exercises the production default-slot -> HTTP pool write -> later-session
// search path, with real SpiceDB checks and no human granting each weekly run.
func TestRepositoryMemory_ScheduledRunsShareOnlyAuthorizedHistory(t *testing.T) {
	ctx := context.Background()
	endpoint := testspicedb.SharedEndpoint(t)
	token := t.Name()
	src, _, _, err := schema.ComposeSlots(shippedSchema(t), []schema.SlotPair{{ResourceType: "github_repo_url", Permission: "write_memory"}})
	require.NoError(t, err)
	testspicedb.WriteSchemaText(t, endpoint, token, src)
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()), grpcutil.WithInsecureBearerToken(token))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	rpc := v1.NewPermissionsServiceClient(conn)
	cli, err := spicedb.NewClient(endpoint, token, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, cli.Close()) })
	id := base64.RawURLEncoding.EncodeToString([]byte("https://github.com/acme/widget"))
	for _, grant := range []struct{ relation, subjectType, id string }{
		{"memory_writer", "service", "coverage-scheduler"},
		{"owner", "user", "engineer"},
	} {
		_, err = rpc.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{Updates: []*v1.RelationshipUpdate{{Operation: v1.RelationshipUpdate_OPERATION_TOUCH, Relationship: &v1.Relationship{
			Resource: &v1.ObjectReference{ObjectType: "github_repo_url", ObjectId: id}, Relation: grant.relation,
			Subject: &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: grant.subjectType, ObjectId: grant.id}},
		}}}})
		require.NoError(t, err)
	}
	for _, tt := range []struct {
		permission, subjectType, subject string
		allowed                          bool
	}{
		{"write_memory", "service", "coverage-scheduler", true},
		{"read", "service", "coverage-scheduler", false},
		{"write", "service", "coverage-scheduler", false},
		{"admin", "service", "coverage-scheduler", false},
		{"view_memory", "user", "engineer", true},
		{"view_memory", "user", "stranger", false},
	} {
		r, err := rpc.CheckPermission(ctx, &v1.CheckPermissionRequest{
			Resource: &v1.ObjectReference{ObjectType: "github_repo_url", ObjectId: id}, Permission: tt.permission,
			Subject:     &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: tt.subjectType, ObjectId: tt.subject}},
			Consistency: &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}},
		})
		require.NoError(t, err)
		assert.Equal(t, tt.allowed, r.Permissionship == v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION, "%s as %s:%s", tt.permission, tt.subjectType, tt.subject)
	}
	backend := inmem.NewBackend()
	mem := memory.NewLocal(backend, memory.WithSearcher(search.New(search.WithProviders(searchinmem.New(backend)))))
	checker := toolcheck.Checker{Cli: cli}
	check := authz.CheckerFunc(func(ctx context.Context, p authz.Permission, in authz.Inputs) authz.Result {
		return checker.CheckToolCall(ctx, p, in)
	})
	reg := tokens.NewRegistry()
	srv := httptest.NewServer(httpsrv.NewHandler(mem, reg, httpsrv.WithPools(cli.Pools())))
	t.Cleanup(srv.Close)
	pool, err := memory.ResourceScope("github_repo_url", id)
	require.NoError(t, err)
	for _, run := range []string{"week-one", "week-two"} {
		session := authz.SessionRef{Namespace: "default", Name: run}
		scope := memory.Scope{Kind: "session", ID: "default/" + run}
		require.NoError(t, authz.BindClassDefaults(memory.WithSystemApproval(ctx, "test"), mem, scope, session,
			[]authz.BoundEntitySpec{{ResourceType: "github_repo_url", Permission: "write_memory", Defaults: []string{"acme/widget"}, ValueTransforms: []string{"github_repo_url_id"}}},
			check, cli.Relations(), "service:coverage-scheduler", time.Now, time.Hour))
		grants, err := cli.ListSlotGrants(ctx, "default", run)
		require.NoError(t, err)
		require.Len(t, grants, 1, "default must bind without a human or permissive tool-call bypass")
		reg.Set(memory.NamespacedName{Namespace: "default", Name: run}, run+"-token", "")
	}
	first := httpclient.New(srv.URL, "week-one-token")
	e, err := observation.NewEntry(`{"sha":"abc123","covered":80,"total":100}`, []string{"coverage-success", "repo:acme/widget"})
	require.NoError(t, err)
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	e.CreatedAt = start
	stored, err := first.PutToPool(ctx, memory.Scope{Kind: "session", ID: "default/week-one"}, pool, e)
	require.NoError(t, err)
	assert.Equal(t, pool, stored.Scope)
	second := httpclient.New(srv.URL, "week-two-token")
	found, err := second.Search(ctx, memory.SearchRequest{Scopes: []memory.Scope{{Kind: "session", ID: "default/week-two"}}, Tags: []string{"coverage-success", "repo:acme/widget"}, Since: &start, Limit: 100})
	require.NoError(t, err)
	require.Len(t, found.Entries, 1)
	assert.Equal(t, stored.ID, found.Entries[0].Entry.ID)
	assert.Equal(t, pool, found.Entries[0].Entry.Scope)
	// The upper bound is exclusive, so the baseline is absent before its time.
	before, err := second.Search(ctx, memory.SearchRequest{Scopes: []memory.Scope{{Kind: "session", ID: "default/week-two"}}, Tags: []string{"coverage-success"}, Until: &start, Limit: 100})
	require.NoError(t, err)
	assert.Empty(t, before.Entries)
	other, err := memory.ResourceScope("github_repo_url", "different-repository")
	require.NoError(t, err)
	_, err = second.PutToPool(ctx, memory.Scope{Kind: "session", ID: "default/week-two"}, other, e)
	require.Error(t, err, "an explicit grant on one repository cannot write another")
}

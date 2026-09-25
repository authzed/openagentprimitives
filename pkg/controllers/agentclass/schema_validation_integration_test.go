//go:build integration

// Integration tests for AgentClass SpiceDB schema validation.
// Requires Docker to spin up an in-process SpiceDB container.
// Run with:
//
//	go test -tags=integration ./pkg/controllers/agentclass/...
package agentclass_test

import (
	"context"
	"testing"
	"time"

	v1proto "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/authzed/grpcutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentclass"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/test/testspicedb"
)

// integrationSchema is a minimal schema used for the schema-validation
// integration tests. It intentionally does NOT match the production schema
// in testspicedb.Schema — each test uses a unique bearer token so the
// datastore is isolated.
const integrationSchema = `
definition user {}
definition github_repo {
    relation reader: user
    relation writer: user
    permission read = reader + writer
    permission write = writer
}
`

// writeCustomSchema writes schemaText to the SpiceDB instance at endpoint
// using the given bearer token. Requires Docker to be running.
func writeCustomSchema(t *testing.T, endpoint, token, schemaText string) {
	t.Helper()
	conn, err := grpc.NewClient(endpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpcutil.WithInsecureBearerToken(token),
	)
	require.NoError(t, err, "dial spicedb for custom schema")
	t.Cleanup(func() { _ = conn.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = v1proto.NewSchemaServiceClient(conn).WriteSchema(ctx, &v1proto.WriteSchemaRequest{
		Schema: schemaText,
	})
	require.NoError(t, err, "write custom schema")
}

// newSchemaReaderClient creates a *spicedb.Client for the given endpoint/token
// and registers a Cleanup to close it.
func newSchemaReaderClient(t *testing.T, endpoint, token string) spicedb.SchemaReader {
	t.Helper()
	c, err := spicedb.NewClient(endpoint, token, true /* insecure */)
	require.NoError(t, err, "spicedb.NewClient")
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// countingSchemaReader wraps a SchemaReader and counts how many times
// ReadSchema is called. Used to verify the laziness guarantee.
type countingSchemaReader struct {
	inner spicedb.SchemaReader
	calls int
}

func (c *countingSchemaReader) ReadSchema(ctx context.Context, in *v1proto.ReadSchemaRequest) (*v1proto.ReadSchemaResponse, error) {
	c.calls++
	return c.inner.ReadSchema(ctx, in)
}

// TestAgentClass_ValidWhenSchemaResolves verifies that an AgentClass
// whose tools' permission.check references match the SpiceDB schema
// comes out Valid=True with SchemaValidated=True.
func TestAgentClass_ValidWhenSchemaResolves(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	endpoint := testspicedb.SharedEndpoint(t)
	token := testspicedb.UniqueToken(t)
	writeCustomSchema(t, endpoint, token, integrationSchema)
	schemaR := newSchemaReaderClient(t, endpoint, token)

	r := &agentclass.Reconciler{
		Client:        env.Client,
		APIReader:     env.Client,
		SpiceDBSchema: schemaR,
	}

	createLLMSecret(t, ctx, env.Client)

	// Tool that references github_repo:read — exists in integrationSchema.
	createValidMCPServer(t, ctx, env.Client, "mcp-schema-valid", []spiceboxv1alpha1.MCPServerTool{
		{
			Name: "read-repo",
			Permission: &authz.Permission{
				StateImpact: authz.Readonly,
				Check: &authz.PermissionCheck{
					ResourceType:       "github_repo",
					ResourceIDTemplate: "{repo}",
					Permission:         "read",
				},
			},
		},
	})

	ac := newClass("ac-schema-valid")
	ac.Spec.MCPServers = []spiceboxv1alpha1.AgentClassMCPServerRef{{Name: "gh", Ref: "mcp-schema-valid"}}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "ac-schema-valid"}})
	require.NoError(t, err, "Reconcile")

	var got spiceboxv1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "ac-schema-valid"}, &got),
		"Get AgentClass")

	validCond := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid)
	require.NotNil(t, validCond, "Valid condition")
	assert.Equal(t, metav1.ConditionTrue, validCond.Status, "Valid status")

	schemaCond := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionSchemaValidated)
	require.NotNil(t, schemaCond, "SchemaValidated condition")
	assert.Equal(t, metav1.ConditionTrue, schemaCond.Status, "SchemaValidated status")
	assert.Equal(t, "Validated", schemaCond.Reason, "SchemaValidated reason")
}

// TestAgentClass_InvalidWhenSchemaMissingResource verifies that a tool
// whose check.resourceType does not exist in SpiceDB causes Valid=False
// with reason=PermissionSchemaMismatch.
func TestAgentClass_InvalidWhenSchemaMissingResource(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	endpoint := testspicedb.SharedEndpoint(t)
	token := testspicedb.UniqueToken(t)
	writeCustomSchema(t, endpoint, token, integrationSchema)
	schemaR := newSchemaReaderClient(t, endpoint, token)

	r := &agentclass.Reconciler{
		Client:        env.Client,
		APIReader:     env.Client,
		SpiceDBSchema: schemaR,
	}

	createLLMSecret(t, ctx, env.Client)

	// Tool that references "linear_team" — NOT in integrationSchema.
	createValidMCPServer(t, ctx, env.Client, "mcp-schema-mismatch", []spiceboxv1alpha1.MCPServerTool{
		{
			Name: "list-issues",
			Permission: &authz.Permission{
				StateImpact: authz.Readonly,
				Check: &authz.PermissionCheck{
					ResourceType:       "linear_team",
					ResourceIDTemplate: "{teamId}",
					Permission:         "read",
				},
			},
		},
	})

	ac := newClass("ac-schema-mismatch")
	ac.Spec.MCPServers = []spiceboxv1alpha1.AgentClassMCPServerRef{{Name: "linear", Ref: "mcp-schema-mismatch"}}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "ac-schema-mismatch"}})
	require.NoError(t, err, "Reconcile")

	var got spiceboxv1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "ac-schema-mismatch"}, &got),
		"Get AgentClass")

	validCond := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid)
	require.NotNil(t, validCond, "Valid condition")
	assert.Equal(t, metav1.ConditionFalse, validCond.Status, "Valid status")
	assert.Equal(t, spiceboxv1alpha1.ReasonPermissionSchemaMismatch, validCond.Reason,
		"Valid reason; msg=%q", validCond.Message)

	schemaCond := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionSchemaValidated)
	require.NotNil(t, schemaCond, "SchemaValidated condition")
	assert.Equal(t, metav1.ConditionFalse, schemaCond.Status, "SchemaValidated status")
	assert.Equal(t, spiceboxv1alpha1.ReasonPermissionSchemaMismatch, schemaCond.Reason, "SchemaValidated reason")
}

// TestAgentClass_SchemaValidationSkippedWhenObservedGenerationMatches
// verifies the laziness guarantee: once SchemaValidated=True is recorded
// for the current generation, a second Reconcile call must NOT call
// ReadSchema again.
func TestAgentClass_SchemaValidationSkippedWhenObservedGenerationMatches(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	endpoint := testspicedb.SharedEndpoint(t)
	token := testspicedb.UniqueToken(t)
	writeCustomSchema(t, endpoint, token, integrationSchema)
	innerR := newSchemaReaderClient(t, endpoint, token)
	spy := &countingSchemaReader{inner: innerR}

	r := &agentclass.Reconciler{
		Client:        env.Client,
		APIReader:     env.Client,
		SpiceDBSchema: spy,
	}

	createLLMSecret(t, ctx, env.Client)

	createValidMCPServer(t, ctx, env.Client, "mcp-schema-lazy", []spiceboxv1alpha1.MCPServerTool{
		{
			Name: "read-repo",
			Permission: &authz.Permission{
				StateImpact: authz.Readonly,
				Check: &authz.PermissionCheck{
					ResourceType:       "github_repo",
					ResourceIDTemplate: "{repo}",
					Permission:         "read",
				},
			},
		},
	})

	ac := newClass("ac-schema-lazy")
	ac.Spec.MCPServers = []spiceboxv1alpha1.AgentClassMCPServerRef{{Name: "gh", Ref: "mcp-schema-lazy"}}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "ac-schema-lazy"}}

	// First reconcile — should call ReadSchema exactly once.
	_, err := r.Reconcile(ctx, req)
	require.NoError(t, err, "first Reconcile")
	require.Equal(t, 1, spy.calls, "after first reconcile: ReadSchema call count")

	// Confirm SchemaValidated=True was set.
	var got spiceboxv1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, req.NamespacedName, &got), "Get AgentClass after first reconcile")
	schemaCond := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionSchemaValidated)
	require.NotNil(t, schemaCond, "SchemaValidated condition")
	require.Equal(t, metav1.ConditionTrue, schemaCond.Status,
		"after first reconcile: SchemaValidated should be True")

	// Second reconcile — same generation, SchemaValidated=True already recorded.
	// ReadSchema must NOT be called again.
	_, err = r.Reconcile(ctx, req)
	require.NoError(t, err, "second Reconcile")
	assert.Equal(t, 1, spy.calls,
		"after second reconcile (same generation): ReadSchema must not be called again")
}

// TestAgentClass_RecoversWhenTheFragmentIsComposedLater.
//
// The bug this closes: a class whose tool checks against a permission its own
// MCPServer declares parks at PermissionSchemaMismatch when it reconciles
// BEFORE the guardian has composed that fragment into SpiceDB — and then never
// recovers, because the code parked without requeueing on the premise that
// "the MCPServer watch recovers it".
//
// That watch cannot fire here. The guardian writes NOTHING to the status of an
// MCPServer whose fragment was always valid ("no condition is added just to
// say fine"), so nothing about the server changes after the compose. A single
// MCPServer usually hid it — that server's own Reachable/PinDrift/Valid writes
// tend to land after the compose and re-trigger the class by accident — and a
// class referencing two servers is where the ordering stopped being lucky.
//
// The class must therefore ask again, and become Valid once the schema has the
// pair.
func TestAgentClass_RecoversWhenTheFragmentIsComposedLater(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	endpoint := testspicedb.SharedEndpoint(t)
	token := testspicedb.UniqueToken(t)
	// The schema WITHOUT the resource the tool needs — the state during the
	// window before the guardian composes.
	writeCustomSchema(t, endpoint, token, integrationSchema)
	schemaR := newSchemaReaderClient(t, endpoint, token)

	r := &agentclass.Reconciler{Client: env.Client, APIReader: env.Client, SpiceDBSchema: schemaR}
	createLLMSecret(t, ctx, env.Client)
	createValidMCPServer(t, ctx, env.Client, "mcp-late-fragment", []spiceboxv1alpha1.MCPServerTool{{
		Name: "compose-doc",
		Permission: &authz.Permission{
			StateImpact: authz.Readwrite,
			Check: &authz.PermissionCheck{
				ResourceType: "summary_doc", ResourceIDTemplate: "summary", Permission: "compose",
			},
		},
	}})

	ac := newClass("ac-late-fragment")
	ac.Spec.MCPServers = []spiceboxv1alpha1.AgentClassMCPServerRef{{Name: "w", Ref: "mcp-late-fragment"}}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	key := types.NamespacedName{Namespace: "default", Name: "ac-late-fragment"}

	// First pass: the fragment is not composed yet.
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	require.NoError(t, err, "Reconcile before the compose")
	assert.Positive(t, res.RequeueAfter,
		"a mismatch may be a fragment that has not landed yet, so the class must ask again — "+
			"parking here is what left it permanently invalid with a perfectly good spec")

	var parked spiceboxv1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, key, &parked))
	validCond := findCondition(parked.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid)
	require.NotNil(t, validCond)
	assert.Equal(t, spiceboxv1alpha1.ReasonPermissionSchemaMismatch, validCond.Reason,
		"and it says exactly which pair is missing the whole time")

	// The guardian composes the fragment.
	writeCustomSchema(t, endpoint, token, integrationSchema+`
definition summary_doc {
	relation author: user
	permission compose = author
}
`)

	// Second pass: the same spec, a schema that now has the pair.
	_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	require.NoError(t, err, "Reconcile after the compose")

	var got spiceboxv1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, key, &got))
	recovered := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid)
	require.NotNil(t, recovered)
	assert.Equal(t, metav1.ConditionTrue, recovered.Status,
		"the class recovers once its fragment exists; msg=%q", recovered.Message)
}

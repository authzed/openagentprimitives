//go:build e2e

package policy_test

import (
	"context"
	"github.com/authzed/openagentprimitives/test/e2e"
	"strings"
	"testing"
	"time"

	spicedbv1schema "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLeakage_OperatorRunnerSchemaLoop: E1. operator + runner schema-write
// loop integration test. Applies the agent-leakage-e2e fixture (whose
// MCPServer carries a structured spicedbSchema fragment with `record`
// and `record_uuid` resources) and asserts the Guardian controller
// composed the fragments into the live SpiceDB schema. Catches schema-
// composition drift: if guardian forgets to write fragments, if rawZed
// references break, etc., this fails at WriteSchema time.
//
// The fixture also exercises the SpiceDBBootstrap controller's per-CR
// flow: relationships are seeded after WriteSchema, and a downstream
// CheckPermission proves both layers work together.
func TestLeakage_OperatorRunnerSchemaLoop(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir:       e2e.TestdataDir("agent-leakage-e2e"),
		DefaultTimeout: 30 * time.Second,
	})
	h.MCP.OnTool("get_record", func(_ map[string]any) any { return map[string]any{"id": "x"} })
	h.MCP.OnTool("get_record_uuid", func(_ map[string]any) any { return map[string]any{"uuid": "u"} })
	h.MCP.OnTool("unmapped_tool", func(_ map[string]any) any { return map[string]any{"ok": true} })
	h.WaitForAgentClassValid("leakage-e2e", 30*time.Second)
	h.WaitForSpiceDBBootstrap(30 * time.Second)

	// Read the live composed schema. Every fragment the test fixture
	// declared MUST appear: the embedded base (user, group,
	// agentsession), plus the MCPServer's contributions (record,
	// record_uuid), plus the leakage caveats baked into the operator's
	// composition (infoleakage_grant, infoleakage_resource_match).
	resp, err := h.SpiceDB.ReadSchema(context.Background(),
		&spicedbv1schema.ReadSchemaRequest{})
	require.NoError(t, err, "ReadSchema")
	schemaText := resp.SchemaText

	for _, defn := range []string{
		"definition user",
		"definition group",
		"definition agentsession",
		"definition record",
		"definition record_uuid",
		"definition infoleakage_grant",
		"definition externaltoken",
		"caveat infoleakage_resource_match",
		"caveat token_value_matches",
	} {
		assert.True(t, strings.Contains(schemaText, defn),
			"composed schema must contain %q; got:\n%s", defn, schemaText)
	}

	// Sanity: a CheckPermission against the composed schema succeeds
	// for the seeded record:happy → alice viewer relationship. This
	// proves SchemaService.WriteSchema accepted the composed schema
	// AND the bootstrap-applied relationships are queryable through it.
	checkResp, err := h.SpiceDB.CheckPermission(context.Background(),
		&spicedbv1schema.CheckPermissionRequest{
			Resource:   &spicedbv1schema.ObjectReference{ObjectType: "record", ObjectId: "happy"},
			Permission: "view",
			Subject: &spicedbv1schema.SubjectReference{
				Object: &spicedbv1schema.ObjectReference{
					ObjectType: "user",
					ObjectId:   e2e.CanonicalForFakeEmail("alice@example.com").String(),
				},
			},
			Consistency: &spicedbv1schema.Consistency{Requirement: &spicedbv1schema.Consistency_FullyConsistent{FullyConsistent: true}},
		})
	require.NoError(t, err, "CheckPermission")
	assert.Equal(t, spicedbv1schema.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION,
		checkResp.GetPermissionship(),
		"alice MUST have view on record:happy through the composed schema + bootstrap relationships")
}

// TestLeakage_ConnectorsSchemaSync: E3. After the operator has written
// its composed schema, a connector-style relationship write (mirroring
// what connectors-prototype does for Slack/Linear membership sync)
// MUST succeed without FailedPrecondition: the schema-composition must
// include every definition the connector writes against.
//
// This test impersonates the connector's write path: WriteRelationships
// directly against the same SpiceDB the operator writes schema to,
// against types declared in the MCPServer's spicedbSchema fragment.
// A schema-ordering invariant violation (schema not written yet, or
// missing definitions) surfaces as the WriteRelationships RPC error.
//
// In-repo smoke; the external connectors-prototype repo is not in scope
// for an e2e test (would require cross-repo orchestration). The test
// exercises the same write surface the connector uses, so it catches
// schema-fragment drift just as well.
func TestLeakage_ConnectorsSchemaSync(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir:       e2e.TestdataDir("agent-leakage-e2e"),
		DefaultTimeout: 30 * time.Second,
	})
	h.MCP.OnTool("get_record", func(_ map[string]any) any { return map[string]any{"id": "x"} })
	h.MCP.OnTool("get_record_uuid", func(_ map[string]any) any { return map[string]any{"uuid": "u"} })
	h.MCP.OnTool("unmapped_tool", func(_ map[string]any) any { return map[string]any{"ok": true} })
	h.WaitForAgentClassValid("leakage-e2e", 30*time.Second)
	h.WaitForSpiceDBBootstrap(30 * time.Second)

	// Connector-style write: a fresh relationship against the
	// composed-schema-declared type `record`. If schema composition
	// missed `record` or wrote it after this call, the RPC would error
	// with FailedPrecondition: "definition `record` not found".
	//
	// e2e.E2EHarnessSource: this impersonates an out-of-repo connector, not any
	// in-repo production writer. See pkg/authz/spicedb/relsource and
	// pkg/authz/spicedb/writer.go.
	_, err := h.SpiceDB.Writer(e2e.E2EHarnessSource).WriteRelationships(context.Background(),
		&spicedbv1schema.WriteRelationshipsRequest{
			Updates: []*spicedbv1schema.RelationshipUpdate{
				{
					Operation: spicedbv1schema.RelationshipUpdate_OPERATION_TOUCH,
					Relationship: &spicedbv1schema.Relationship{
						Resource: &spicedbv1schema.ObjectReference{ObjectType: "record", ObjectId: "connector-test"},
						Relation: "viewer",
						Subject: &spicedbv1schema.SubjectReference{
							Object: &spicedbv1schema.ObjectReference{ObjectType: "user", ObjectId: e2e.CanonicalForFakeEmail("connector@example.com").String()},
						},
					},
				},
			},
		})
	require.NoError(t, err, "connector-style WriteRelationships MUST succeed against composed schema")

	// Read it back to confirm the tuple landed.
	stream, err := h.SpiceDB.ReadRelationships(context.Background(),
		&spicedbv1schema.ReadRelationshipsRequest{
			RelationshipFilter: &spicedbv1schema.RelationshipFilter{
				ResourceType:       "record",
				OptionalResourceId: "connector-test",
			},
			Consistency: &spicedbv1schema.Consistency{Requirement: &spicedbv1schema.Consistency_FullyConsistent{FullyConsistent: true}},
		})
	require.NoError(t, err, "ReadRelationships")
	n := 0
	for {
		_, err := stream.Recv()
		if err != nil {
			break
		}
		n++
	}
	assert.Equal(t, 1, n, "connector-written relationship MUST be readable back")
}

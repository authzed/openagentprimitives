package toolcheck_test

import (
	"context"
	"testing"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/toolcheck"
	"github.com/authzed/openagentprimitives/pkg/tools/browserschema"
)

// externalActPerm is the shape a browser acting tool's Permission() returns in
// Task C3: External, web_host, permission act, instance = the page host.
func externalActPerm() authz.Permission {
	return authz.Permission{
		StateImpact: authz.External,
		Check: &authz.PermissionCheck{
			ResourceType:       browserschema.WebHostType,
			Permission:         browserschema.ActPermission,
			ResourceIDTemplate: "{host}",
		},
	}
}

// The browser acting tool mirrors git push: an External call on web_host is
// satisfied by a SLOT GRANT a human wrote for this session and host, and the
// grant leg is asked about the SESSION — the thing that makes it an approval and
// not ambient ownership. HAS_PERMISSION only after the grant exists.
func TestCheck_External_WebHost_AllowedByASlotGrantForThisSession(t *testing.T) {
	cli := &recordingSpiceDB{answers: map[string]v1.CheckPermissionResponse_Permissionship{
		"web_host:aa.com#" + authz.SlotGrantRelationName(browserschema.ActPermission): v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION,
	}}
	res := toolcheck.Checker{Cli: cli}.CheckToolCall(context.Background(), externalActPerm(), authz.Inputs{
		Subject:           "u-1",
		Args:              map[string]any{"host": "aa.com"},
		AgentSessionRef:   "ns/s1",
		SlotResourceTypes: []string{browserschema.WebHostType},
	})
	assert.Equal(t, authz.OutcomeAllowed, res.Outcome,
		"web_host:aa.com#act@agentsession:ns/s1 must be permitted once the slot grant exists")
	require.NotEmpty(t, cli.calls)
	assert.Contains(t, cli.calls[0], "agentsession",
		"the grant leg is asked about the SESSION, not the user — that is what makes it an approval")
	assert.Contains(t, cli.calls[0], "web_host:aa.com#slot_grant_act",
		"the relation checked is slot_grant_act, never the composed act permission")
}

// Before any grant exists, the same call is denied and routed to a human — only
// after a grant does it pass. The two halves are the round-trip the brief asks
// to prove (HAS after a grant, NOT before).
func TestCheck_External_WebHost_DeniedWithoutAGrant(t *testing.T) {
	cli := &recordingSpiceDB{} // no answers: slot_grant_act resolves NO_PERMISSION
	res := toolcheck.Checker{Cli: cli}.CheckToolCall(context.Background(), externalActPerm(), authz.Inputs{
		Subject:           "u-1",
		Args:              map[string]any{"host": "aa.com"},
		AgentSessionRef:   "ns/s1",
		SlotResourceTypes: []string{browserschema.WebHostType},
	})
	assert.Equal(t, authz.OutcomeDenied, res.Outcome,
		"with no slot grant, an external acting call must route to human approval")
	assert.Contains(t, res.Message, "approval")
}

// web_host must be a DECLARED slot for the grant leg to be queried at all: if
// the class never declared it, ComposeSlots wrote no slot_grant_act relation, so
// asking SpiceDB would only error. Denies to approval at zero round trips.
func TestCheck_External_WebHost_UndeclaredSlotDeniesWithoutQuerying(t *testing.T) {
	cli := &recordingSpiceDB{answers: map[string]v1.CheckPermissionResponse_Permissionship{
		"web_host:aa.com#" + authz.SlotGrantRelationName(browserschema.ActPermission): v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION,
	}}
	res := toolcheck.Checker{Cli: cli}.CheckToolCall(context.Background(), externalActPerm(), authz.Inputs{
		Subject:         "u-1",
		Args:            map[string]any{"host": "aa.com"},
		AgentSessionRef: "ns/s1",
		// SlotResourceTypes omitted: this class declares no web_host slot.
	})
	assert.Equal(t, authz.OutcomeDenied, res.Outcome)
	assert.Empty(t, cli.calls,
		"a type with no declared slot cannot carry a grant; querying it would only error")
}

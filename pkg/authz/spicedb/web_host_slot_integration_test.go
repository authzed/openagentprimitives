//go:build integration

package spicedb

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/toolcheck"
	"github.com/authzed/openagentprimitives/pkg/tools/browserschema"
	"github.com/authzed/openagentprimitives/test/testspicedb"
)

// webHostComposedFragment is the web_host definition as ComposeSlots emits it
// for a declared web_host#act slot — the SESSION-ONLY shape, byte-identical to
// git_repo push. The unit test TestBrowserFragment_ComposesWebHostSessionOnlySlot
// (pkg/authz/guardian/schema) proves browserschema.Fragment() + ComposeSlots
// produces exactly this, so this fixture tracks that output; this integration
// test then proves a real SpiceDB enforces the grant round-trip on it.
const webHostComposedFragment = `
definition web_host {
    relation slot_grant_act: agentsession with expiration
    relation owner: user
    permission act = slot_grant_act->interact + owner
}
`

// The acting-tool round-trip against a real SpiceDB: an External web_host#act
// call is DENIED before any grant and ALLOWED after authz.GrantSlots writes the
// slot_grant_act tuple for this session and host — exactly the promise C3's
// acting tools and C8's unattended grant rely on. The check runs through the
// real toolcheck.checkExternalSlotGrant path (CheckToolCall), which queries the
// slot_grant_act RELATION for the SESSION subject, not the composed permission.
func TestIntegration_WebHostSlotGrant_RoundTrip(t *testing.T) {
	endpoint := testspicedb.Endpoint(t)
	token := testspicedb.UniqueToken(t)
	testspicedb.WriteSchemaWithExtra(t, endpoint, token, webHostComposedFragment)
	c, err := NewClient(endpoint, token, true)
	require.NoError(t, err, "NewClient")
	t.Cleanup(func() { _ = c.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	const ns, sessName, host = "ns", "s1", "aa.com"
	sess := authz.SessionRef{Namespace: ns, Name: sessName}
	t.Cleanup(func() { _ = c.DeleteSlotGrants(context.Background(), ns, sessName) })

	// *Client satisfies toolcheck.Client (CheckPermission) directly, so the real
	// external-check path runs against real SpiceDB.
	checker := toolcheck.Checker{Cli: c}
	actPerm := authz.Permission{
		StateImpact: authz.External,
		Check: &authz.PermissionCheck{
			ResourceType:       browserschema.WebHostType,
			Permission:         browserschema.ActPermission,
			ResourceIDTemplate: "{host}",
		},
	}
	inputs := authz.Inputs{
		Subject:           "u-1",
		Args:              map[string]any{"host": host},
		AgentSessionRef:   ns + "/" + sessName,
		SlotResourceTypes: []string{browserschema.WebHostType},
	}

	// BEFORE: no grant → denied, routed to a human.
	before := checker.CheckToolCall(ctx, actPerm, inputs)
	assert.Equal(t, authz.OutcomeDenied, before.Outcome,
		"web_host:aa.com#act@agentsession:ns/s1 must NOT be permitted before a grant")

	// Grant through the real GrantSlots writer.
	require.NoError(t, authz.GrantSlots(ctx, c.Relations(), sess,
		[]authz.SlotBinding{{
			ResourceType: browserschema.WebHostType,
			ResourceID:   authz.TrustedObjectID(host),
			Permission:   browserschema.ActPermission,
		}}, time.Now().Add(time.Hour)), "GrantSlots")

	// AFTER: the same call now passes silently.
	after := checker.CheckToolCall(ctx, actPerm, inputs)
	assert.Equal(t, authz.OutcomeAllowed, after.Outcome,
		"web_host:aa.com#act@agentsession:ns/s1 must be permitted once the slot grant exists")

	// And revoking denies the next fully-consistent check — a grant that can be
	// written must be revocable.
	require.NoError(t, authz.RevokeSlots(ctx, c.Relations(), sess,
		[]authz.SlotBinding{{
			ResourceType: browserschema.WebHostType,
			ResourceID:   authz.TrustedObjectID(host),
			Permission:   browserschema.ActPermission,
		}}), "RevokeSlots")
	revoked := checker.CheckToolCall(ctx, actPerm, inputs)
	assert.Equal(t, authz.OutcomeDenied, revoked.Outcome,
		"revoking the slot grant must deny the next acting call")
}

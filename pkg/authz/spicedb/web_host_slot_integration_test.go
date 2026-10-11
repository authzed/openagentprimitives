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

// The acting-tool round-trip against a real SpiceDB, using a REAL DOTTED host
// (app.example.com): an External web_host#act call is DENIED before any grant and
// ALLOWED after a grant is written on the ESCAPED object id, exactly as an
// approved plan-gate card / C8's unattended grant writes it. The check runs
// through the real toolcheck.checkExternalSlotGrant path (CheckToolCall), which
// queries the slot_grant_act RELATION for the SESSION subject.
//
// This is the regression guard the dots bug (C-R4) lacked. A dotted host is not a
// legal SpiceDB object id (charset [a-zA-Z0-9_/\-=|+] -- no "."), so the pre-fix
// code keyed web_host on the raw host and this round trip could never work
// against real SpiceDB. Both legs now key on the SAME injective spicedb_escape
// id (browserschema.WebHostValueTransforms): the grant write derives it with
// NewObjectID, and the check resolves it because the acting tool's Check now
// carries the identical chain. Drop the transform from either leg and this fails
// -- the grant write with InvalidArgument on the raw id, or the ALLOWED check
// because the two ids no longer match.
func TestIntegration_WebHostSlotGrant_RoundTrip(t *testing.T) {
	endpoint := testspicedb.Endpoint(t)
	token := testspicedb.UniqueToken(t)
	testspicedb.WriteSchemaWithExtra(t, endpoint, token, webHostComposedFragment)
	c, err := NewClient(endpoint, token, true)
	require.NoError(t, err, "NewClient")
	t.Cleanup(func() { _ = c.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	const ns, sessName, host = "ns", "s1", "app.example.com"
	const escaped = "app=2Eexample=2Ecom"
	sess := authz.SessionRef{Namespace: ns, Name: sessName}
	t.Cleanup(func() { _ = c.DeleteSlotGrants(context.Background(), ns, sessName) })

	// The grant id as the approval/bind path mints it: NewObjectID over the class's
	// published web_host chain. Asserted by VALUE so a regression to a raw id is
	// caught here, not only by the SpiceDB write failing.
	grantID, err := authz.NewObjectID(host, browserschema.WebHostValueTransforms())
	require.NoError(t, err, "derive the web_host object id")
	require.Equal(t, escaped, grantID.String(), "the dotted host must escape to a legal object id")

	// *Client satisfies toolcheck.Client (CheckPermission) directly, so the real
	// external-check path runs against real SpiceDB. The Check carries the SAME
	// escape chain the acting tool's Permission() now sets.
	checker := toolcheck.Checker{Cli: c}
	actPerm := authz.Permission{
		StateImpact: authz.External,
		Check: &authz.PermissionCheck{
			ResourceType:         browserschema.WebHostType,
			Permission:           browserschema.ActPermission,
			ResourceIDTemplate:   "{host}",
			ResourceIDTransforms: browserschema.WebHostValueTransforms(),
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
		"web_host:app=2Eexample=2Ecom#act@agentsession:ns/s1 must NOT be permitted before a grant")

	// Grant through the real GrantSlots writer, on the escaped id.
	require.NoError(t, authz.GrantSlots(ctx, c.Relations(), sess,
		[]authz.SlotBinding{{
			ResourceType: browserschema.WebHostType,
			ResourceID:   grantID,
			Permission:   browserschema.ActPermission,
		}}, time.Now().Add(time.Hour)), "GrantSlots")

	// AFTER: the same call now passes silently.
	after := checker.CheckToolCall(ctx, actPerm, inputs)
	assert.Equal(t, authz.OutcomeAllowed, after.Outcome,
		"web_host:app=2Eexample=2Ecom#act@agentsession:ns/s1 must be permitted once the slot grant exists")

	// And revoking denies the next fully-consistent check — a grant that can be
	// written must be revocable.
	require.NoError(t, authz.RevokeSlots(ctx, c.Relations(), sess,
		[]authz.SlotBinding{{
			ResourceType: browserschema.WebHostType,
			ResourceID:   grantID,
			Permission:   browserschema.ActPermission,
		}}), "RevokeSlots")
	revoked := checker.CheckToolCall(ctx, actPerm, inputs)
	assert.Equal(t, authz.OutcomeDenied, revoked.Outcome,
		"revoking the slot grant must deny the next acting call")
}

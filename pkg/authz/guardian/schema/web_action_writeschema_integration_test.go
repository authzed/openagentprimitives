//go:build integration

// The assertion the unit suite cannot make: ValidateComposedSchema
// (web_action_slot_test.go) type-checks the composed web_action schema
// IN-PROCESS, but SpiceDB's own WriteSchema is the authority on whether a
// relation `with pagestate_matches and expiration` is actually accepted against
// a live datastore. A caveat the composer emits that the server rejects is the
// exact failure task E1 must not ship — a schema green in a unit test but
// refused by WriteSchema freezes the whole cluster's schema, not one agent.
//
//	go test -tags=integration -count=1 ./pkg/authz/guardian/schema/
package schema_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/schema"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/tools/browserschema"
	"github.com/authzed/openagentprimitives/test/testspicedb"
)

// TestSpiceDBAcceptsWebActionPagestateMatchesSlot composes exactly what the
// guardian path produces for a browser acting class — the scaffold (which now
// carries the pagestate_matches caveat) plus the web_host and web_action builtin
// fragments, with both slots injected — and WRITES it to a real SpiceDB,
// asserting the server ACCEPTS it.
//
// This is the keystone proof for E1. ComposeSlots emits web_action's slot grant
// as `slot_grant_act: agentsession with pagestate_matches and expiration`; if
// the pagestate_matches caveat were missing from the scaffold, or its CEL body
// were malformed, or a caveated-relation reference were somehow invalid, the
// server's WriteSchema type-check — the one compiler.Compile does not run —
// would refuse the whole schema here.
func TestSpiceDBAcceptsWebActionPagestateMatchesSlot(t *testing.T) {
	base, err := schema.ComposeBase([]*spiceboxv1alpha1.SpiceDBSchemaFragment{
		browserschema.Fragment(),          // web_host
		browserschema.WebActionFragment(), // web_action
	})
	require.NoError(t, err, "scaffold + browser builtin fragments must compose")

	composed, changed, skipped, err := schema.ComposeSlots(base, []schema.SlotPair{
		{ResourceType: browserschema.WebHostType, Permission: browserschema.ActPermission},
		{ResourceType: browserschema.WebActionType, Permission: browserschema.ActPermission},
	})
	require.NoError(t, err)
	require.True(t, changed)
	require.Empty(t, skipped, "both builtin types are declared, so neither slot is skipped")
	require.Contains(t, composed, "relation slot_grant_act: agentsession with pagestate_matches and expiration",
		"the composed text must carry web_action's caveated grant before we write it")

	endpoint := testspicedb.SharedEndpoint(t)
	token := testspicedb.UniqueToken(t)

	cli, err := spicedb.NewClient(endpoint, token, true /* insecure */)
	require.NoError(t, err, "spicedb.NewClient")
	t.Cleanup(func() { _ = cli.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	io := spicedb.SchemaIOFor(cli)

	// THE assertion: a live SpiceDB accepts the pagestate_matches-caveated
	// web_action grant. compiler.Compile (TestSchemaCompiles) says nothing about
	// this — only the server's reference-resolving type-check does.
	require.NoError(t, io.WriteSchema(ctx, composed),
		"a live SpiceDB must accept the web_action slot grant caveated with pagestate_matches")

	// And it round-trips: the server stores and re-renders the caveated relation,
	// and the caveat definition survives. (SpiceDB drops comments and normalizes
	// whitespace, so this is a semantic, not byte, check.)
	got, err := io.ReadSchema(ctx)
	require.NoError(t, err, "ReadSchema after a successful write")
	assert.Contains(t, got, "caveat pagestate_matches",
		"the pagestate_matches caveat must survive the write")
	assert.True(t, strings.Contains(got, "pagestate_matches") && strings.Contains(got, "slot_grant_act"),
		"web_action's caveated grant relation must survive the write; got:\n%s", got)
}

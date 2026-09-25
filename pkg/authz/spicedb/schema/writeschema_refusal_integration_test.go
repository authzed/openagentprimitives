//go:build integration

// The assertion TestSchemaCompiles (schema_test.go) cannot make: compiler.Compile
// is a PARSE, not a validator — it resolves no references, so a permission
// naming a relation its own definition never declared compiles cleanly there.
// SpiceDB's own reference-resolving validator (pkg/schemautil) is not
// importable without dragging its whole server dependency tree into go.mod,
// so this test pins the cost against a real server instead: a live SpiceDB's
// WriteSchema refuses text compiler.Compile would accept.
//
//	go test -tags=integration -count=1 ./pkg/authz/spicedb/schema/
package schema_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	authzschema "github.com/authzed/openagentprimitives/pkg/authz/spicedb/schema"
	"github.com/authzed/openagentprimitives/test/testspicedb"
)

// TestSpiceDBRefusesAPermissionWithADanglingRelation writes a copy of the
// canonical scaffold with one permission expression swapped for the slot form
// the guardian composer used to produce before it was fixed to skip a slot
// naming a reserved scaffold definition (see
// pkg/authz/guardian/schema/slots_reserved_scaffold_test.go). The
// substitution is deliberately hand-built rather than routed through
// ComposeSlots: that composer can no longer produce this text, and this test
// is about what the SERVER does with it, not about how it might arise.
//
// The unit suite (TestSchemaCompiles) proves compiler.Compile accepts the
// canonical scaffold; it says nothing about a scaffold with a dangling
// reference because compiler.Compile never resolves references at all. Only a
// real server proves the cost of that gap: WriteSchema refuses, and because
// schema compose is global and all-or-nothing, that refusal is every agent's
// schema, not one.
func TestSpiceDBRefusesAPermissionWithADanglingRelation(t *testing.T) {
	// Reproduce composeOneSlot's two edits by hand (pkg/authz/guardian/schema/
	// slots.go): it injects the slot_grant_<perm> relation right after the
	// definition header, then REPLACES the named permission's expression with
	// slotPermissionExpr — slot_grant_<perm>->interact + owner. Both edits are
	// needed for fidelity: with only the permission line swapped,
	// slot_grant_read itself would ALSO be undeclared, and SpiceDB would report
	// that dangling reference first, masking the one this test is about. owner
	// is never declared on memory_entry — that is the actual dangling
	// reference the pre-fix composer would have produced by targeting a
	// reserved scaffold definition.
	// Each substitution is guarded separately against its OWN input, not just
	// checked once at the end against the two combined. A single trailing
	// require.NotEqual(final, original) cannot tell "both anchors matched"
	// from "one anchor rotted and the other alone changed the text" — either
	// way `final != original`. Guarding independently means a rotted anchor
	// fails here, naming itself, instead of surfacing later as a server error
	// about the wrong identifier.
	afterRelationInjection := strings.Replace(authzschema.Schema,
		"definition memory_entry {",
		"definition memory_entry {\n    relation slot_grant_read: agentsession with expiration", 1)
	require.NotEqual(t, authzschema.Schema, afterRelationInjection,
		"the relation-injection anchor no longer matches schema.zed: %q", "definition memory_entry {")

	bad := strings.Replace(afterRelationInjection,
		"permission read = session->read_transcript",
		"permission read = slot_grant_read->interact + owner", 1)
	require.NotEqual(t, afterRelationInjection, bad,
		"the permission anchor no longer matches schema.zed: %q", "permission read = session->read_transcript")

	endpoint := testspicedb.SharedEndpoint(t)
	token := testspicedb.UniqueToken(t)

	cli, err := spicedb.NewClient(endpoint, token, true /* insecure */)
	require.NoError(t, err, "spicedb.NewClient")
	t.Cleanup(func() { _ = cli.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err = spicedb.SchemaIOFor(cli).WriteSchema(ctx, bad)
	require.Error(t, err, "the server type-checks what compiler.Compile does not")
	t.Logf("WriteSchema refusal: %v", err)
	assert.Contains(t, strings.ToLower(err.Error()), "owner",
		"expected the server's error to name the undeclared relation/permission; got: %v", err)
}

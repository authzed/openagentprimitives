package schema_test

import (
	"strings"
	"testing"

	"github.com/authzed/spicedb/pkg/schemadsl/compiler"
	"github.com/authzed/spicedb/pkg/schemadsl/input"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/guardian/schema"
	authzschema "github.com/authzed/openagentprimitives/pkg/authz/spicedb/schema"
)

// compileSchema is what this repo's validation actually runs, here and in
// ValidateFragment: a PARSE. It resolves no references, so a permission naming
// a relation the definition does not declare passes it. The server's own
// WriteSchema type-checks and refuses; nothing in-process does.
//
// Reproducing the refusal in a unit test is not free: SpiceDB's
// schemautil.ValidateSchemaChanges is the server-side check, and importing it
// drags the whole SpiceDB server dependency tree into go.mod. So this file
// proves the rewrite and the dangling reference textually, and the write-time
// refusal belongs in the integration suite, which already has a live SpiceDB.
func compileSchema(t *testing.T, src string) error {
	t.Helper()
	_, err := compiler.Compile(compiler.InputSchema{
		Source:       input.Source("reserved-scaffold-probe"),
		SchemaString: src,
	}, compiler.AllowUnprefixedObjectType())
	return err
}

func definitionBlock(t *testing.T, src, name string) string {
	t.Helper()
	start := strings.Index(src, "definition "+name+" {")
	require.GreaterOrEqual(t, start, 0, "definition %s must exist in the scaffold", name)
	end := strings.Index(src[start:], "\n}")
	require.Greater(t, end, 0, "definition %s must be closed", name)
	return src[start : start+end+2]
}

// removeTrimmedLine deletes the first line of src whose TRIMMED content
// equals want, regardless of how much leading whitespace precedes it. A
// composed schema's indentation is the generator's to choose (tabs from
// composeFragmentSet's compiler round-trip, verbatim source whitespace from
// an untouched RawZed block, …) — a test asserting "this relation is gone"
// should not also be pinning which one produced the text.
//
// Fails the test outright when want is not found, rather than returning src
// unchanged: a caller asking to remove a line and silently getting the same
// text back would go on to assert something about a "without X" schema that
// is actually identical to "with X" — a vacuous test that passes for the
// wrong reason.
func removeTrimmedLine(t *testing.T, src, want string) string {
	t.Helper()
	lines := strings.Split(src, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) == want {
			return strings.Join(append(append([]string{}, lines[:i]...), lines[i+1:]...), "\n")
		}
	}
	t.Fatalf("removeTrimmedLine: no line trimmed-equal to %q found in src:\n%s", want, src)
	return ""
}

// A slot naming a definition the SCAFFOLD owns is skipped and reported, exactly
// like a slot naming a type that does not exist. Both are the same failure from
// the composer's side — an AgentClass asked for something it may not have — and
// both must leave every other agent's schema alone.
func TestComposeSlots_ASlotOnAReservedScaffoldTypeIsSkippedAndReported(t *testing.T) {
	require.NoError(t, compileSchema(t, authzschema.Schema), "baseline: the scaffold parses")

	cases := []struct {
		name         string
		resourceType string
		permission   string
		platformExpr string // the expression the platform wrote, which must survive
	}{
		{
			name:         "memory_entry read: the per-entry memory ReBAC filter",
			resourceType: "memory_entry",
			permission:   "read",
			platformExpr: "session->read_transcript",
		},
		{
			name:         "artifact view: the live-view gate, incl. the platform-admin arm",
			resourceType: "artifact",
			permission:   "view",
			platformExpr: "parent->interact + parent->artifact_org_view + platform->view_audit",
		},
		{
			name:         "pt_tag reader: the monotone per-datum egress audience",
			resourceType: "pt_tag",
			permission:   "reader",
			platformExpr: "direct_reader + derived_from.all(reader)",
		},
		{
			name:         "agentclass start_session: who may start a session at all",
			resourceType: "agentclass",
			permission:   "start_session",
			platformExpr: "starter + platform->start_session",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pair := schema.SlotPair{ResourceType: tc.resourceType, Permission: tc.permission}

			out, changed, skipped, err := schema.ComposeSlots(authzschema.Schema, []schema.SlotPair{pair})
			require.NoError(t, err)

			assert.False(t, changed, "a reserved scaffold definition is not rewritten")
			assert.Equal(t, []schema.SlotPair{pair}, skipped,
				"and the slot is REPORTED, so a declared-but-inert slot is visible rather than silent")
			assert.Equal(t, authzschema.Schema, out, "the scaffold is returned untouched")

			block := definitionBlock(t, out, tc.resourceType)
			assert.Contains(t, block, tc.platformExpr, "the platform's own permission expression survives")
			assert.NotContains(t, block, schema.SlotGrantRelationName(tc.permission),
				"and no slot grant relation was injected into a definition the platform owns")
		})
	}
}

// The counterpart, and the shape the fix should take: a slot naming a type the
// schema does not declare is skipped and reported rather than composed, so one
// AgentClass cannot wedge schema writes for everyone else. Reserved types want
// exactly this treatment.
func TestComposeSlots_AnUndeclaredTypeIsSkippedNotComposed(t *testing.T) {
	pair := schema.SlotPair{ResourceType: "no_such_type", Permission: "read"}

	out, changed, skipped, err := schema.ComposeSlots(authzschema.Schema, []schema.SlotPair{pair})
	require.NoError(t, err)

	assert.False(t, changed, "nothing to rewrite")
	assert.Equal(t, []schema.SlotPair{pair}, skipped, "reported, so a declared-but-inert slot is visible")
	assert.NoError(t, compileSchema(t, out), "and the cluster's schema is untouched")
}

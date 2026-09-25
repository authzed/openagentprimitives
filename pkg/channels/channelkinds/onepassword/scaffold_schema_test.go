package onepassword_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	guardianschema "github.com/authzed/openagentprimitives/pkg/authz/guardian/schema"
	authzschema "github.com/authzed/openagentprimitives/pkg/authz/spicedb/schema"
)

// This file replaces a test that asserted the defect: it checked that a
// package-local SchemaFragment() string carried `definition
// onepassword_group`, and it was green while NOTHING composed that string into
// any schema. The function had no caller but that test, it was not the
// channelkinds.SchemaContributor interface the composer actually walks, and so
// the definition reached no cluster — while the scaffold's `group#member`
// named it. Every SpiceDB WriteSchema in the repo failed at the server.
//
// So the invariant is not "the fragment text says X". It is that the SHIPPED
// scaffold declares what it names, and resolves standing alone.

// onepassword_group is the definition this kind writes every one of its tuples
// against, and it must be in the scaffold rather than in a fragment: the
// scaffold is written bare by the install bootstrap, by `oap spicedb
// apply-schema`, and by test/testspicedb.WriteSchemaText, none of which
// compose a fragment.
//
// 1pwgroup is NOT a legal SpiceDB definition name — a definition must start
// with a lowercase LETTER, verified against the vendored compiler:
//
//	^([a-z][a-z0-9_]{1,62}[a-z0-9]/)*[a-z][a-z0-9_]{1,62}[a-z0-9]$
//
// This test pins the name we did choose, so a later "tidy-up" to a shorter one
// fails here rather than at a live WriteSchema.
func TestScaffold_DeclaresOnepasswordGroupWithASentinel(t *testing.T) {
	assert.Contains(t, authzschema.Schema, "definition onepassword_group {")
	assert.Contains(t, authzschema.Schema, "relation member: user")
	assert.Contains(t, authzschema.Schema, "relation relhash: string")
}

// The display-name label has to be in the SCAFFOLD for the same reason
// relhash is: the three places that write the scaffold bare compose no
// fragment, and a relation SpiceDB has never heard of makes the whole
// WriteRelationships fail — which would take this scope's MEMBERSHIP down with
// it, not merely its name. It rides the same permission-less `string` subject
// type and appears in no permission, so declaring it grants nothing.
func TestScaffold_DeclaresTheDisplayNameLabelOnOnepasswordGroup(t *testing.T) {
	assert.Contains(t, authzschema.Schema, "relation label: string",
		"the sync writes onepassword_group:<id>#label@string:<encoded name>; an undeclared relation fails the whole write")

	// The structural half of "a label grants nothing": no permission anywhere
	// in the scaffold computes from it. Scanned rather than asserted as a
	// literal absence, because the risk is not a `permission label` line — it
	// is a label turning up as an ARM of some other permission's union, which a
	// substring check for the declaration would never see.
	for _, line := range strings.Split(authzschema.Schema, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "permission ") {
			continue
		}
		_, expr, ok := strings.Cut(trimmed, "=")
		require.True(t, ok, "a permission line must have an expression: %q", trimmed)
		for _, arm := range strings.FieldsFunc(expr, func(r rune) bool {
			return r == '+' || r == '-' || r == '&' || r == ' ' || r == '\t'
		}) {
			assert.NotEqual(t, "label", arm,
				"a label is display data; no permission may compute from one (%q)", trimmed)
		}
	}
}

// Consumers keep referencing group#member and never learn which directory
// produced a member. A second directory is one more arm, not a new seam.
func TestScaffold_GroupUnionsTheSyncedDirectory(t *testing.T) {
	assert.Contains(t, authzschema.Schema, "onepassword_group#member",
		"group.member must union the synced directory, so view_memory = group:eng#member resolves through it")
}

// The invariant the deleted test could not express, and the one that actually
// failed at every live WriteSchema: the scaffold ALONE — no fragments, no
// channel kinds — names nothing it does not declare. This is what says
// onepassword_group is reachable by the bootstrap ConfigMap and by the
// integration/e2e fixture setup, not merely present in some string somewhere.
func TestScaffold_ResolvesStandingAlone(t *testing.T) {
	found, err := guardianschema.UnresolvedReferences(authzschema.Schema)
	require.NoError(t, err)
	assert.Empty(t, found,
		"the bare scaffold must resolve with no fragment composed into it: %v", found)
}

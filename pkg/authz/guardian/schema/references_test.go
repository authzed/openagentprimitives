package schema_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/schema"
	authzschema "github.com/authzed/openagentprimitives/pkg/authz/spicedb/schema"
)

// The scaffold resolves cleanly. If this ever fails, the resolver is wrong, not
// the scaffold.
func TestUnresolvedReferences_ScaffoldIsClean(t *testing.T) {
	found, err := schema.UnresolvedReferences(authzschema.Schema)
	require.NoError(t, err)
	assert.Empty(t, found, "the shipped scaffold must resolve: %v", found)
}

// The shape that took down every schema write in the repo: the scaffold's
// `group#member` named `onepassword_group`, which only a channel-kind helper
// string declared — and nothing composed that string. compiler.Compile is a
// parse and let it through; SpiceDB's WriteSchema resolves allowed subject
// types and rejected the whole schema, so `oap install`, `apply-schema`, the
// guardian compose and testspicedb's fixture setup all failed at once.
//
// Each case breaks exactly one thing, so a failure here names which half of
// the resolver is wrong rather than "something reported something".
func TestUnresolvedReferences_CatchesAnUndeclaredSubjectType(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []string
	}{
		{
			name: "a relation names a definition nothing declares: reported",
			want: []string{"nosuchtype"},
			src: `
definition user {}
definition group {
    relation member: user | nosuchtype
}
`,
		},
		{
			name: "a subject SET names a relation the target does not declare: reported as type#relation",
			want: []string{"other#nosuchrelation"},
			src: `
definition user {}
definition other {
    relation member: user
}
definition group {
    relation member: user | other#nosuchrelation
}
`,
		},
		{
			name: "the same shape, resolved: nothing reported",
			src: `
definition user {}
definition other {
    relation member: user
}
definition group {
    relation member: user | other#member
}
`,
		},
		{
			name: "a wildcard names its object type and no relation",
			want: []string{"nosuchtype"},
			src: `
definition user {}
definition thing {
    relation viewer: user:* | nosuchtype:*
}
`,
		},
		{
			name: "a relation-less sentinel is a legal subject type — the scaffold's `string` must never be reported",
			src: `
definition user {}
definition string {}
definition slack_channel {
    relation relhash: string
}
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			found, err := schema.UnresolvedReferences(tc.src)
			require.NoError(t, err)
			var names []string
			for _, f := range found {
				names = append(names, f.Name)
			}
			assert.ElementsMatch(t, tc.want, names)
		})
	}
}

// A subject-type finding names the direct RELATION at fault, not a permission,
// so an operator reading the log line can go straight to the declaration.
func TestUnresolvedReferences_SubjectTypeFindingNamesTheRelation(t *testing.T) {
	src := `
definition user {}
definition group {
    relation member: user | nosuchtype
}
`
	found, err := schema.UnresolvedReferences(src)
	require.NoError(t, err)
	require.Len(t, found, 1)
	assert.Equal(t, "group", found[0].Definition)
	assert.Equal(t, "member", found[0].Permission)
	assert.Equal(t, "nosuchtype", found[0].Name)
	assert.Contains(t, found[0].String(), `group#member references "nosuchtype"`)
}

// The exact shape the reserved-slot defect produced: a permission naming a
// relation the definition does not declare. compiler.Compile accepts this,
// which is why nothing in-process caught it.
func TestUnresolvedReferences_CatchesADanglingRelation(t *testing.T) {
	src := `
definition user {}
definition thing {
    relation creator: user
    permission read = creator + owner
}
`
	found, err := schema.UnresolvedReferences(src)
	require.NoError(t, err)
	require.Len(t, found, 1)
	assert.Equal(t, "thing", found[0].Definition)
	assert.Equal(t, "read", found[0].Permission)
	assert.Equal(t, "owner", found[0].Name)
}

// The literal shape of the defect fixed earlier on this branch: a slot rewrote
// memory_entry's permission into `slot_grant_read->interact + owner`, and
// memory_entry declares no `owner`. It compiled; the server refused it.
func TestUnresolvedReferences_CatchesTheReservedSlotDefectShape(t *testing.T) {
	src := `
definition user {}
definition agentsession {
    relation owner: user
    permission interact = owner
}
definition memory_entry {
    relation session: agentsession
    relation slot_grant_read: agentsession
    permission read = slot_grant_read->interact + owner
}
`
	found, err := schema.UnresolvedReferences(src)
	require.NoError(t, err)
	require.Len(t, found, 1, "exactly the dangling `owner`, and not the arrow: %v", found)
	assert.Equal(t, "memory_entry", found[0].Definition)
	assert.Equal(t, "read", found[0].Permission)
	assert.Equal(t, "owner", found[0].Name)
}

func TestUnresolvedReferences_AcceptsAPermissionReferencingAPermission(t *testing.T) {
	src := `
definition user {}
definition thing {
    relation creator: user
    permission read = creator
    permission view = read
}
`
	found, err := schema.UnresolvedReferences(src)
	require.NoError(t, err)
	assert.Empty(t, found, "a permission may name another permission in the same definition")
}

func TestUnresolvedReferences_AcceptsAnArrow(t *testing.T) {
	src := `
definition user {}
definition parent {
    relation owner: user
    permission admin = owner
}
definition child {
    relation upward: parent
    permission admin = upward->admin
}
`
	found, err := schema.UnresolvedReferences(src)
	require.NoError(t, err)
	assert.Empty(t, found, "an arrow resolves its left side locally and its right side on the target")
}

// The scaffold uses `derived_from.all(reader)`. A walker that descends only
// into TupleToUserset misses the functioned form entirely — and a walker that
// misses a node type reports nothing for every input, which every
// "clean schema has no findings" test above would still pass.
func TestUnresolvedReferences_DescendsIntoAFunctionedArrow(t *testing.T) {
	cases := []struct {
		name     string
		src      string
		wantName string
	}{
		{
			name: "all() over a declared relation onto a declared permission: clean",
			src: `
definition user {}
definition thing {
    relation derived_from: thing
    relation direct_reader: user
    permission reader = direct_reader + derived_from.all(reader)
}
`,
		},
		{
			name: "all() whose LEFT side is undeclared: reports the left side",
			src: `
definition user {}
definition thing {
    relation direct_reader: user
    permission reader = direct_reader + derived_from.all(reader)
}
`,
			wantName: "derived_from",
		},
		{
			name: "any() whose RIGHT side is undeclared on the target: reports the right side",
			src: `
definition user {}
definition parent {
    relation owner: user
}
definition thing {
    relation upward: parent
    permission reader = upward.any(nonexistent)
}
`,
			wantName: "nonexistent",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			found, err := schema.UnresolvedReferences(tc.src)
			require.NoError(t, err)
			if tc.wantName == "" {
				assert.Empty(t, found, "expected a clean resolve: %v", found)
				return
			}
			require.Len(t, found, 1, "findings: %v", found)
			assert.Equal(t, tc.wantName, found[0].Name)
		})
	}
}

// Nested set operations are where a walker most plausibly stops descending.
func TestUnresolvedReferences_DescendsIntoNestedSetOperations(t *testing.T) {
	cases := []struct {
		name string
		expr string
		want []string
	}{
		{name: "union", expr: "rel_a + missing_u", want: []string{"missing_u"}},
		{name: "intersection", expr: "rel_a & missing_i", want: []string{"missing_i"}},
		{name: "exclusion left", expr: "missing_x - rel_a", want: []string{"missing_x"}},
		{name: "exclusion right", expr: "rel_a - missing_y", want: []string{"missing_y"}},
		{name: "parenthesized nesting", expr: "rel_a + (rel_b & (rel_a - missing_n))", want: []string{"missing_n"}},
		{name: "deep mix, two findings", expr: "(rel_a & missing_p) + (rel_b - missing_q)", want: []string{"missing_p", "missing_q"}},
		{name: "all declared", expr: "(rel_a + rel_b) & (rel_b - rel_a)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := `
definition user {}
definition thing {
    relation rel_a: user
    relation rel_b: user
    permission perm_p = ` + tc.expr + `
}
`
			found, err := schema.UnresolvedReferences(src)
			require.NoError(t, err)
			var names []string
			for _, f := range found {
				names = append(names, f.Name)
			}
			assert.ElementsMatch(t, tc.want, names)
		})
	}
}

// The right side of an arrow resolves on the TARGET, not locally. When the
// target is not a definition this schema declares, there is nothing to check
// it against — skip rather than invent a finding.
//
// The two halves of an undeclared target are judged differently, on purpose.
// The arrow's right half (`whatever`) is SKIPPED: a fragment validated over
// the scaffold alone may legitimately arrow onto a type a sibling fragment
// contributes, and a false positive there freezes schema writes cluster-wide.
// The subject TYPE itself (`nosuchtype`) is reported, because SpiceDB resolves
// allowed subject types for every schema it is handed — including the scaffold
// written bare — so nothing downstream can supply it later.
func TestUnresolvedReferences_SkipsAnArrowOntoAnUnknownType(t *testing.T) {
	src := `
definition thing {
    relation upward: nosuchtype
    permission admin = upward->whatever
}
`
	found, err := schema.UnresolvedReferences(src)
	require.NoError(t, err)
	var names []string
	for _, f := range found {
		names = append(names, f.Name)
	}
	assert.NotContains(t, names, "whatever",
		"the target type is undeclared, so its permission set is unknown")
	assert.Equal(t, []string{"nosuchtype"}, names,
		"the undeclared subject type itself is the finding")
}

// A relation may point at several types, and SpiceDB resolves the arrow
// against each. Reporting a name that resolves on ANY of them would be a false
// positive, and a false positive is the failure mode that matters: once this
// gates anything, it freezes schema writes cluster-wide.
func TestUnresolvedReferences_ArrowOverAMultiTypeRelation(t *testing.T) {
	cases := []struct {
		name  string
		src   string
		empty bool
	}{
		{
			name:  "resolves on one of two types: clean",
			empty: true,
			src: `
definition user {}
definition group {
    relation member: user
    permission admin = member
}
definition thing {
    relation upward: user | group
    permission alpha = upward->admin
}
`,
		},
		{
			name: "resolves on neither: reported",
			src: `
definition user {}
definition group {
    relation member: user
}
definition thing {
    relation upward: user | group
    permission alpha = upward->admin
}
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			found, err := schema.UnresolvedReferences(tc.src)
			require.NoError(t, err)
			if tc.empty {
				assert.Empty(t, found, "findings: %v", found)
				return
			}
			require.Len(t, found, 1)
			assert.Equal(t, "admin", found[0].Name)
		})
	}
}

// Forms a hand-written walker can misread. None of these is a finding.
func TestUnresolvedReferences_AcceptsLegitimateEdgeForms(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{
			name: "wildcard subject type",
			src: `
definition user {}
definition thing {
    relation viewer: user | user:*
    permission view = viewer
}
`,
		},
		{
			name: "subject relation subject type",
			src: `
definition user {}
definition group {
    relation member: user
    permission admin = member
}
definition thing {
    relation upward: group#member
    permission alpha = upward->admin
}
`,
		},
		{
			name: "caveated and expiring relation",
			src: `
use expiration

caveat check_hash(provided string, expected string) {
    provided == expected
}
definition user {}
definition thing {
    relation grant: user with check_hash and expiration
    permission use_grant = grant
}
`,
		},
		{
			name: "arrow onto a relation rather than a permission on the target",
			src: `
definition user {}
definition parent {
    relation owner: user
}
definition child {
    relation upward: parent
    permission admin = upward->owner
}
`,
		},
		{
			name: "self-referential permission chain",
			src: `
definition user {}
definition thing {
    relation parent: thing
    relation owner: user
    permission ancestor = parent + parent->ancestor
}
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			found, err := schema.UnresolvedReferences(tc.src)
			require.NoError(t, err)
			assert.Empty(t, found, "legitimate form must not be reported: %v", found)
		})
	}
}

func TestUnresolvedReferences_ReportsAParseFailure(t *testing.T) {
	_, err := schema.UnresolvedReferences("definition thing { this is not a schema")
	require.Error(t, err)
}

// Findings are ordered, so a caller logging them and a test asserting on them
// both see a stable sequence.
func TestUnresolvedReferences_FindingsAreDeterministic(t *testing.T) {
	src := `
definition user {}
definition b_def {
    relation rel_r: user
    permission perm_z = missing_z
    permission perm_a = missing_a
}
definition a_def {
    relation rel_r: user
    permission perm_p = missing_p
}
`
	first, err := schema.UnresolvedReferences(src)
	require.NoError(t, err)
	require.Len(t, first, 3)
	for i := 0; i < 5; i++ {
		again, err := schema.UnresolvedReferences(src)
		require.NoError(t, err)
		assert.Equal(t, first, again, "repeat run %d must produce the same order", i)
	}
}

// TestRunAll_RefusesAFragmentWithADanglingReference pins a behavior change
// this composable-schema migration (Task 4) intentionally makes: fragment
// ASSEMBLY now runs through composeFragmentSet (ComposeFragments), which
// validates against spicedb's own type system before RunAll ever gets a
// schema string back — see composer.go's composeFragmentSet doc. A fragment
// carrying a genuinely SELF-CONTAINED dangling reference (`owner`, which
// wiring_probe itself never declares — no sibling, no baseline type could
// ever resolve it) is refused right there, so RunAll now returns an error
// and never reaches io.WriteSchema, instead of writing the schema and only
// logging a report (the pre-Task-4 contract). The LATE report-only pass this
// paragraph used to contrast with — a second UnresolvedReferences resolve
// over the fully composed text, run inside composeAllWithSkipped after
// grant/slot rewrites — is gone as of Task 6: composeAllWithSkipped now
// relies on ValidateComposedSchema alone for anything a grant or slot
// rewrite introduces, so there is no report-only fallback left for a
// dangling reference like this one, or any other, if the earlier gate ever
// missed it.
//
// This is not a new hazard. In production the write target is a real
// SpiceDB, which was always going to refuse this exact schema at
// WriteSchema — RunAll now discovers that in-process, before the network
// round trip, rather than via a live rejection reaching the cluster.
//
// The clean case runs FIRST, in the same test, as the isolation proof: the
// identical wiring_probe shape, minus the injected `+ owner` arm, composes
// and writes normally through the exact same RunAll path. That is what makes
// the dangling case's failure attributable to the injected arm specifically,
// rather than to some other property of the fixture — the same isolation
// discipline
// TestComposeAllWithSkipped_ConfirmsTheSurgeryIntroducesADuplicateName
// (postsurgery_validate_internal_test.go) uses for the grant/slot-surgery
// class of surgery-introduced invalid schema: the fragment composes clean on
// its own, and only running ComposeSlots over it produces text
// ValidateComposedSchema refuses. That test's defect is a duplicate
// relation/permission name rather than a dangling reference — the
// dangling-reference shape slot composition itself used to produce (a
// slot's `owner` leg resolving to nothing on a target that declares no
// `owner`) is now closed; see slotPermissionExpr's doc and
// TestComposeAllWithSkipped_ConfirmsTheSurgeryOmitsTheOwnerLeg, which proves
// the identical fixture that used to dangle now composes to valid schema
// instead. Kept as one test rather than a separate clean twin so the two
// halves cannot drift apart the way the earlier standalone clean-case test
// (deleted in Task 6, once the report-only pass it pinned was gone) drifted
// from this one.
func TestRunAll_RefusesAFragmentWithADanglingReference(t *testing.T) {
	cleanFragment := &spiceboxv1alpha1.SpiceDBSchemaFragment{
		RawZed: `
definition wiring_probe {
    relation session: agentsession
    permission read = session->interact
}
`,
	}
	danglingFragment := &spiceboxv1alpha1.SpiceDBSchemaFragment{
		RawZed: `
definition wiring_probe {
    relation session: agentsession
    permission read = session->interact + owner
}
`,
	}

	cleanIO := &fakeSchemaIO{}
	res, err := schema.RunAll(context.Background(), cleanIO, identify(cleanFragment), nil, nil, nil)
	require.NoError(t, err, "wiring_probe without the injected `+ owner` arm must compose and write")
	assert.True(t, res.Changed, "the clean fixture must be written")
	assert.Equal(t, 1, cleanIO.writes, "the clean fixture must reach WriteSchema exactly once")

	io := &fakeSchemaIO{}
	_, err = schema.RunAll(context.Background(), io, identify(danglingFragment), nil, nil, nil)
	require.Error(t, err, "a self-contained dangling reference must refuse the compose, not merely report it")
	assert.Contains(t, err.Error(), "owner")
	assert.Equal(t, 0, io.writes, "an invalid schema must never reach WriteSchema")
}

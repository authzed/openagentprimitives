package schema_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	guardianschema "github.com/authzed/openagentprimitives/pkg/authz/guardian/schema"
)

// scaffoldFrag is a minimal stand-in for the real scaffold: enough types for
// the other fixtures to reference, and nothing else.
func scaffoldFrag() guardianschema.NamedFragment {
	return guardianschema.NamedFragment{Name: "000-scaffold", ZED: `
definition user {}

definition agentsession {
	relation owner: user
	permission interact = owner
}
`}
}

func TestComposeFragments_MergesEveryFragment(t *testing.T) {
	out, err := guardianschema.ComposeFragments([]guardianschema.NamedFragment{
		scaffoldFrag(),
		{Name: "chat", ZED: "definition chat_room {\n\trelation member: user\n\tpermission view = member\n}\n"},
		{Name: "forge", ZED: "definition forge_repo {\n\trelation admin: user\n\tpermission read = admin\n}\n"},
	})
	require.NoError(t, err, "three non-conflicting fragments must compose")
	assert.Contains(t, out, "definition agentsession")
	assert.Contains(t, out, "definition chat_room")
	assert.Contains(t, out, "definition forge_repo")
}

// The composed text is written to SpiceDB and compared against what is already
// live. A composer whose output moved between two runs over identical inputs
// would rewrite the schema on every reconcile.
func TestComposeFragments_IsByteStableAcrossRuns(t *testing.T) {
	frags := []guardianschema.NamedFragment{
		scaffoldFrag(),
		{Name: "zeta", ZED: "definition zeta_thing {\n\trelation owner: user\n}\n"},
		{Name: "alpha", ZED: "definition alpha_thing {\n\trelation owner: user\n}\n"},
	}
	first, err := guardianschema.ComposeFragments(frags)
	require.NoError(t, err)
	second, err := guardianschema.ComposeFragments(frags)
	require.NoError(t, err)
	assert.Equal(t, first, second, "identical inputs must produce identical bytes")

	// Input ORDER must not change output either: callers assemble the slice
	// from registries and map ranges whose order is not promised.
	reordered := []guardianschema.NamedFragment{frags[2], frags[0], frags[1]}
	third, err := guardianschema.ComposeFragments(reordered)
	require.NoError(t, err)
	assert.Equal(t, first, third, "fragment slice order must not change the output")
}

// The scaffold carries doc comments that explain the authorization model. A
// composer that dropped them would silently strip the schema's documentation
// from every cluster.
func TestComposeFragments_PreservesCommentsAndCaveats(t *testing.T) {
	out, err := guardianschema.ComposeFragments([]guardianschema.NamedFragment{
		{Name: "000-scaffold", ZED: `
/** user is a principal. */
definition user {}

definition doc {
	// owner started it.
	relation owner: user with expiring_grant
	permission read = owner
}

caveat expiring_grant(now int, until int) {
	now < until
}
`},
	})
	require.NoError(t, err)
	assert.Contains(t, out, "/** user is a principal. */")
	assert.Contains(t, out, "// owner started it.")
	assert.Contains(t, out, "caveat expiring_grant(now int, until int)")
	assert.Contains(t, out, "with expiring_grant")
}

// Two fragments declaring the same definition is the conflict the partition
// exists to isolate. The error must name the LOSING FRAGMENT, because the
// caller turns it into a status condition on that contributor's own CR.
func TestComposeFragments_DuplicateDefinitionNamesTheFragment(t *testing.T) {
	_, err := guardianschema.ComposeFragments([]guardianschema.NamedFragment{
		scaffoldFrag(),
		{Name: "tenant-a", ZED: "definition report {\n\trelation owner: user\n}\n"},
		{Name: "tenant-b", ZED: "definition report {\n\trelation viewer: user\n}\n"},
	})
	require.Error(t, err, "two fragments declaring `report` must not compose")
	assert.Contains(t, err.Error(), "tenant-b",
		"the error must name the fragment that lost, so the caller can condition its CR")
	assert.Contains(t, err.Error(), "report")
}

// A permission naming something nothing declares is refused by SpiceDB at
// WriteSchema — once, cluster-wide, after every fragment has merged. Catching
// it here is the entire point of validating rather than only parsing.
func TestComposeFragments_RefusesAnUnresolvedReference(t *testing.T) {
	_, err := guardianschema.ComposeFragments([]guardianschema.NamedFragment{
		scaffoldFrag(),
		{Name: "dangling", ZED: "definition thing {\n\trelation owner: user\n\tpermission read = nobody_declared_this\n}\n"},
	})
	require.Error(t, err, "a permission naming an undeclared relation must be refused")
	assert.Contains(t, err.Error(), "nobody_declared_this")
}

// A relation pointing at a definition no fragment contributes. This is the
// cross-fragment failure mode: it looks fine in the fragment alone.
func TestComposeFragments_RefusesAReferenceToAMissingDefinition(t *testing.T) {
	_, err := guardianschema.ComposeFragments([]guardianschema.NamedFragment{
		scaffoldFrag(),
		{Name: "orphan", ZED: "definition review {\n\trelation repo: forge_repo\n}\n"},
	})
	require.Error(t, err, "a relation naming a definition nothing declares must be refused")
	assert.Contains(t, err.Error(), "forge_repo")
}

func TestComposeFragments_RejectsUnusableNames(t *testing.T) {
	cases := []struct {
		name  string
		frags []guardianschema.NamedFragment
		want  string
	}{
		{
			name:  "empty input: no fragments to compose",
			frags: nil,
			want:  "no fragments",
		},
		{
			name:  "empty name: fragment cannot be addressed in an error",
			frags: []guardianschema.NamedFragment{{Name: "", ZED: "definition a {}"}},
			want:  "empty name",
		},
		{
			name: "duplicate name: one fragment would silently overwrite the other",
			frags: []guardianschema.NamedFragment{
				{Name: "dup", ZED: "definition a {}"},
				{Name: "dup", ZED: "definition b {}"},
			},
			want: "duplicate fragment name",
		},
		{
			name:  "path separator: name must be one segment of a synthetic FS",
			frags: []guardianschema.NamedFragment{{Name: "a/b", ZED: "definition a {}"}},
			want:  "single path segment",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := guardianschema.ComposeFragments(tc.frags)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestValidateComposedSchema_AcceptsValidAndRefusesDangling(t *testing.T) {
	good := "definition user {}\n\ndefinition doc {\n\trelation owner: user\n\tpermission read = owner\n}\n"
	assert.NoError(t, guardianschema.ValidateComposedSchema(good))

	bad := "definition user {}\n\ndefinition doc {\n\trelation owner: user\n\tpermission read = missing\n}\n"
	err := guardianschema.ValidateComposedSchema(bad)
	require.Error(t, err, "ValidateComposedSchema must refuse what SpiceDB would refuse")
	assert.Contains(t, err.Error(), "missing")
}

// TestValidateComposedSchema_AcceptsAnUnresolvedArrowRightHalf pins an
// asymmetry that internal/cmd/operator's TestCompileTimeFragmentsCompose
// depends on to justify running BOTH ValidateComposedSchema AND
// UnresolvedReferences over the compile-time set, rather than one in place of
// the other (a swap that briefly happened and silently dropped a whole class
// of catch).
//
// spicedb's type system — what ValidateComposedSchema runs — resolves an
// arrow's LEFT half (the tupleset relation must exist on the local
// definition) but never resolves the RIGHT half: a computed permission name
// that nothing declares on the arrow's target type type-checks clean and
// simply resolves to nothing at Check time. UnresolvedReferences walks that
// same right half and rejects it (see its own doc, "An arrow's right half").
//
// If a future change swaps one check for the other again, this test goes red
// before that swap ships.
func TestValidateComposedSchema_AcceptsAnUnresolvedArrowRightHalf(t *testing.T) {
	src := `
definition user {}

definition link {
	relation target: user
	permission view = target->no_such_permission
}
`
	assert.NoError(t, guardianschema.ValidateComposedSchema(src),
		"spicedb's type system does not resolve an arrow's right half; this must NOT fail")

	unresolved, err := guardianschema.UnresolvedReferences(src)
	require.NoError(t, err)
	require.NotEmpty(t, unresolved,
		"UnresolvedReferences must catch what ValidateComposedSchema does not, or the two checks are redundant")
	assert.Equal(t, "no_such_permission", unresolved[0].Name)
}

// The composer must not be fooled by a fragment whose text happens to parse
// but declares nothing.
func TestComposeFragments_EmptyFragmentTextIsHarmless(t *testing.T) {
	base := []guardianschema.NamedFragment{scaffoldFrag()}
	withEmpty := append(append([]guardianschema.NamedFragment{}, base...),
		guardianschema.NamedFragment{Name: "silent", ZED: "\n\n"})

	want, err := guardianschema.ComposeFragments(base)
	require.NoError(t, err)
	got, err := guardianschema.ComposeFragments(withEmpty)
	require.NoError(t, err, "a fragment with no declarations must not fail the compose")

	// Anchor: without this, a regression that truncated the import list (so
	// ComposeFragments dropped everything after the first fragment) would
	// leave both `want` and `got` degenerate but still EQUAL to each other,
	// and the assertion below would pass having proven nothing.
	require.Contains(t, want, "definition agentsession",
		"the scaffold fixture must actually reach the composed text, or the equality below proves nothing")
	assert.Equal(t, want, got,
		"a fragment that declares nothing must contribute nothing: composing with "+
			"and without it must produce identical schema text")
}

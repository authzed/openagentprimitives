package schema_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/schema"
)

func TestComposeBase_ScaffoldAloneIsTheScaffold(t *testing.T) {
	out, err := schema.ComposeBase(nil)
	require.NoError(t, err)
	assert.Contains(t, out, "definition agentsession", "the scaffold is always present")
	assert.NoError(t, compileSchema(t, out))
}

func TestComposeBase_MergesACompileTimeFragment(t *testing.T) {
	frag := &spiceboxv1alpha1.SpiceDBSchemaFragment{
		RawZed: "definition build_time_thing {\n    relation owner: user\n    permission read = owner\n}\n",
	}
	out, err := schema.ComposeBase([]*spiceboxv1alpha1.SpiceDBSchemaFragment{frag})
	require.NoError(t, err)
	assert.Contains(t, out, "definition build_time_thing")
	assert.NoError(t, compileSchema(t, out))
}

// A compile-time fragment that does not parse is a BUILD bug, so ComposeBase
// reports it rather than silently dropping it. There is no CR to mark, which is
// exactly why this set does not go through the runtime partition.
func TestComposeBase_ReportsAFragmentThatDoesNotParse(t *testing.T) {
	frag := &spiceboxv1alpha1.SpiceDBSchemaFragment{RawZed: "definition broken {"}
	_, err := schema.ComposeBase([]*spiceboxv1alpha1.SpiceDBSchemaFragment{frag})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "compile-time")
}

// Redeclaring a scaffold definition is the one thing a compile-time fragment
// must never do, for the same reason a runtime fragment may not.
func TestComposeBase_RefusesToRedeclareAScaffoldDefinition(t *testing.T) {
	frag := &spiceboxv1alpha1.SpiceDBSchemaFragment{
		RawZed: "definition memory_entry {\n    relation creator: user\n    permission read = creator\n}\n",
	}
	_, err := schema.ComposeBase([]*spiceboxv1alpha1.SpiceDBSchemaFragment{frag})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "memory_entry")
}

// Two fragments can each pass ValidateFragment in isolation — parsed against
// the scaffold alone — and still collide once merged: EmitSpicedbSchema's
// conflict detection covers only structured Resources[], not RawZed text, so
// two RawZed blocks that both declare `definition foo` with different bodies
// are concatenated without complaint. ComposeBase must catch this by
// compiling what it actually returns, the same way the production compose
// path (ComposeWithSkipped) compiles scaffold+fragments as one unit rather
// than fragment-by-fragment.
func TestComposeBase_ReportsARawZedCollisionAcrossFragments(t *testing.T) {
	fragA := &spiceboxv1alpha1.SpiceDBSchemaFragment{
		RawZed: "definition foo {\n    relation owner: user\n    permission read = owner\n}\n",
	}
	fragB := &spiceboxv1alpha1.SpiceDBSchemaFragment{
		RawZed: "definition foo {\n    relation admin: user\n    permission read = admin\n}\n",
	}
	require.NoError(t, schema.ValidateFragment(fragA), "fragA must parse fine against the scaffold alone")
	require.NoError(t, schema.ValidateFragment(fragB), "fragB must parse fine against the scaffold alone")

	_, err := schema.ComposeBase([]*spiceboxv1alpha1.SpiceDBSchemaFragment{fragA, fragB})
	require.Error(t, err, "the merged text redeclares foo and must not compile silently")
	assert.Contains(t, err.Error(), "foo")
}

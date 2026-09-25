package schema_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/guardian/schema"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
	"github.com/authzed/openagentprimitives/toolkits"
)

// A toolkit's checks name a resourceType and a permission, and until the
// toolkit could carry the definition, nothing declared them: a gh-tooled agent
// under toolCalls.mode=enforcing got `object definition "github_repo" not found`
// on every gated call. The fragment has to reach the same compose pass the
// MCPServer and SpiceDBBootstrap fragments go through, which is what this
// conversion is for.
func TestToolkitFragments_convertsADeclaredSchema(t *testing.T) {
	got := schema.ToolkitFragments([]toolkit.Toolkit{{
		Name: "demokit",
		SpiceDBSchema: &toolkit.SpiceDBSchemaFragment{
			Resources: []toolkit.SpiceDBResource{{
				Name:      "demo_repo",
				Relations: []toolkit.SpiceDBRelation{{Name: "owner", SubjectType: "user"}},
				Permissions: []toolkit.SpiceDBPermission{
					{Name: "read", Expr: "owner"},
					{Name: "push", Expr: "owner"},
				},
			}},
		},
	}})

	require.Len(t, got, 1)
	require.Len(t, got[0].Resources, 1)
	r := got[0].Resources[0]
	assert.Equal(t, "demo_repo", r.Name)
	require.Len(t, r.Relations, 1)
	assert.Equal(t, "owner", r.Relations[0].Name)
	assert.Equal(t, "user", r.Relations[0].SubjectType)
	require.Len(t, r.Permissions, 2)
	assert.Equal(t, "read", r.Permissions[0].Name)
	assert.Equal(t, "owner", r.Permissions[0].Expr)
	assert.Equal(t, "push", r.Permissions[1].Name)
}

// Wildcard and RawZed are the two fields a hand-rolled field copy is most likely
// to forget, and forgetting either is silent: a dropped wildcard narrows a
// permission nobody asked to narrow, and a dropped RawZed loses the subject-
// relation forms the structured shape cannot express at all.
func TestToolkitFragments_carriesWildcardAndRawZed(t *testing.T) {
	got := schema.ToolkitFragments([]toolkit.Toolkit{{
		Name: "demokit",
		SpiceDBSchema: &toolkit.SpiceDBSchemaFragment{
			Resources: []toolkit.SpiceDBResource{{
				Name: "demo_repo",
				Relations: []toolkit.SpiceDBRelation{
					{Name: "any_user", SubjectType: "user", Wildcard: true},
				},
			}},
			RawZed: "definition demo_side {}",
		},
	}})

	require.Len(t, got, 1)
	require.Len(t, got[0].Resources, 1)
	require.Len(t, got[0].Resources[0].Relations, 1)
	assert.True(t, got[0].Resources[0].Relations[0].Wildcard, "wildcard must survive conversion")
	assert.Equal(t, "definition demo_side {}", got[0].RawZed)
}

// Most toolkits declare no checks at all (cat, echo, docker), so most contribute
// nothing. An empty fragment is not the same as no fragment downstream: it would
// be one more entry the composer and the conflict partitioner have to carry, and
// one more thing to explain in a compose failure that has nothing to do with it.
func TestToolkitFragments_skipsToolkitsThatDeclareNothing(t *testing.T) {
	got := schema.ToolkitFragments([]toolkit.Toolkit{
		{Name: "nokit"},
		{Name: "emptykit", SpiceDBSchema: &toolkit.SpiceDBSchemaFragment{}},
		{Name: "rawonly", SpiceDBSchema: &toolkit.SpiceDBSchemaFragment{RawZed: "definition x {}"}},
	})

	require.Len(t, got, 1, "only the toolkit that actually declares something contributes")
	assert.Equal(t, "definition x {}", got[0].RawZed,
		"a RawZed-only fragment still counts — it declares real schema")
}

// Every SHIPPED toolkit's fragment must survive the validation the operator
// puts an MCPServer's through — and until gh.yaml grew a RawZed block, nothing
// asserted that, because every builtin fragment was structured and `expr:
// owner`. RawZed is the half no structural check covers: it is appended
// verbatim, so a syntax error, a name colliding with the base scaffold, or a
// subject relation the scaffold does not declare (`github_user#sole_user`)
// resolves nowhere until a live WriteSchema — cluster-wide, after every other
// fragment has been merged in, which is precisely the all-or-nothing failure
// ValidateFragment exists to keep out of RunAll.
//
// A built-in cannot be repaired by editing a CR, so it must not be possible to
// ship one that fails here.
func TestBuiltinToolkitFragments_eachValidatesOnItsOwn(t *testing.T) {
	require.NotEmpty(t, schema.ToolkitFragments(toolkits.All()),
		"some builtin toolkit must declare schema, or this asserts nothing")

	for _, tk := range toolkits.All() {
		one := schema.ToolkitFragments([]toolkit.Toolkit{tk})
		if len(one) == 0 {
			// Declares nothing; TestToolkitFragments_skipsToolkitsThatDeclareNothing
			// is what covers that case.
			continue
		}
		t.Run(tk.Name, func(t *testing.T) {
			assert.NoError(t, schema.ValidateFragment(one[0]),
				"the %s toolkit ships a fragment the operator would refuse", tk.Name)
		})
	}
}

// And they must validate TOGETHER, over the base scaffold. Each one passing
// alone says nothing about two of them colliding on a definition name, which is
// the failure that takes down the compose for every AgentSessionGrants in the
// cluster rather than only for the toolkit that caused it.
func TestBuiltinToolkitFragments_composeTogetherOverTheScaffold(t *testing.T) {
	frags := schema.ToolkitFragments(toolkits.All())
	require.NotEmpty(t, frags)

	composed, err := schema.ComposeBase(frags)
	require.NoError(t, err, "the shipped toolkits must compose over the base scaffold as a set")

	// Spot-check that both halves of the gh repository model reached the
	// composed schema, so a fragment silently dropped in conversion cannot pass
	// as a clean compose.
	assert.Contains(t, composed, "definition github_repo_url",
		"the URL-keyed type every gh check resolves against must reach the live schema")
	assert.Contains(t, composed, "definition github_repo",
		"the forge-keyed type the directory sync writes must reach the live schema")

	// And every name the shipped toolkits ask for must RESOLVE, subject types
	// included. ValidateFragment deliberately drops subject-type findings — it
	// holds one fragment over the scaffold and cannot tell a sibling's type
	// from a typo — so a `github_user#sole_user` misspelled as `#nope`
	// validates clean there and is refused by SpiceDB's WriteSchema instead,
	// cluster-wide, taking every AgentSessionGrants with it.
	//
	// Here the view is not partial: the builtin toolkits reference nothing but
	// their own definitions and the base scaffold's, so a finding is a real
	// dangling name rather than a fragment this test could not see.
	unresolved, err := schema.UnresolvedReferences(composed)
	require.NoError(t, err)
	assert.Empty(t, unresolved,
		"a builtin toolkit names something nothing declares; SpiceDB refuses the whole schema for this")
}

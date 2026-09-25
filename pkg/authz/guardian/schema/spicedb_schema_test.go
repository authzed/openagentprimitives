package schema

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// TestSchema_IncludesInfoLeakageGrant asserts that the static base scaffold
// includes the infoleakage_grant definition with the expected relations,
// caveat, and permission. infoleakage_grant is code-owned (not
// MCPServer-contributed) so it must appear even with nil fragments and nil
// pairs.
func TestSchema_IncludesInfoLeakageGrant(t *testing.T) {
	s, err := ComposeAll(nil, nil, nil)
	require.NoError(t, err)

	// The caveat carrying the accessed-resource identity (type + id strings).
	// Parameter order is the composable-DSL generator's (alphabetical), not
	// the scaffold source's — cosmetic, since caveat parameters bind by name:
	// composeFragmentSet's assembly now round-trips the WHOLE scaffold through
	// ComposeFragments' compile+render rather than concatenating it verbatim.
	assert.Contains(t, s, "caveat infoleakage_resource_match(resource_id string, resource_type string)")

	// The definition itself.
	assert.Contains(t, s, "definition infoleakage_grant {")
	// session linkage — allows cleanup when a session ends.
	assert.Contains(t, s, "relation session: agentsession")
	// audience_subject holds the recipient with resource-match caveat + TTL.
	assert.Contains(t, s, "relation audience_subject: user with infoleakage_resource_match and expiration")
	// valid permission: both the session link and the audience_subject caveat+expiry must hold.
	assert.Contains(t, s, "permission valid = session & audience_subject")
}

func TestEmitSpicedbSchema_SingleServer(t *testing.T) {
	fragment := &spiceboxv1alpha1.SpiceDBSchemaFragment{
		Resources: []spiceboxv1alpha1.SpiceDBResource{
			{Name: "hubspot_owner", Relations: []spiceboxv1alpha1.SpiceDBRelation{
				{Name: "user", SubjectType: "user"},
			}, Permissions: []spiceboxv1alpha1.SpiceDBPermission{
				{Name: "resolve", Expr: "user"},
			}},
		},
	}
	got, err := EmitSpicedbSchema([]*spiceboxv1alpha1.SpiceDBSchemaFragment{fragment})
	require.NoError(t, err, "EmitSpicedbSchema")
	assert.Contains(t, got, "definition hubspot_owner {", "definition emitted")
	assert.Contains(t, got, "relation user: user", "relation emitted")
	assert.Contains(t, got, "permission resolve = user", "permission emitted")
}

func TestEmitSpicedbSchema_DedupesIdenticalResources(t *testing.T) {
	r := spiceboxv1alpha1.SpiceDBResource{Name: "x", Permissions: []spiceboxv1alpha1.SpiceDBPermission{{Name: "p", Expr: "self"}}}
	f1 := &spiceboxv1alpha1.SpiceDBSchemaFragment{Resources: []spiceboxv1alpha1.SpiceDBResource{r}}
	f2 := &spiceboxv1alpha1.SpiceDBSchemaFragment{Resources: []spiceboxv1alpha1.SpiceDBResource{r}}
	got, err := EmitSpicedbSchema([]*spiceboxv1alpha1.SpiceDBSchemaFragment{f1, f2})
	require.NoError(t, err, "EmitSpicedbSchema")
	assert.Equal(t, 1, strings.Count(got, "definition x {"), "identical resources collapsed to one definition")
}

func TestEmitSpicedbSchema_WildcardSubject(t *testing.T) {
	frag := &spiceboxv1alpha1.SpiceDBSchemaFragment{
		Resources: []spiceboxv1alpha1.SpiceDBResource{{
			Name: "crm_company",
			Relations: []spiceboxv1alpha1.SpiceDBRelation{
				{Name: "any_user", SubjectType: "user", Wildcard: true},
				{Name: "owner", SubjectType: "user"},
			},
			Permissions: []spiceboxv1alpha1.SpiceDBPermission{
				{Name: "read", Expr: "any_user + owner"},
			},
		}},
	}
	out, err := EmitSpicedbSchema(
		[]*spiceboxv1alpha1.SpiceDBSchemaFragment{frag},
	)
	require.NoError(t, err)
	assert.Contains(t, out, "relation any_user: user:*")
	assert.Contains(t, out, "relation owner: user")
	assert.Contains(t, out, "permission read = any_user + owner")
}

func TestEmitSpicedbSchema_ConflictErrors(t *testing.T) {
	f1 := &spiceboxv1alpha1.SpiceDBSchemaFragment{Resources: []spiceboxv1alpha1.SpiceDBResource{
		{Name: "x", Permissions: []spiceboxv1alpha1.SpiceDBPermission{{Name: "p", Expr: "a"}}},
	}}
	f2 := &spiceboxv1alpha1.SpiceDBSchemaFragment{Resources: []spiceboxv1alpha1.SpiceDBResource{
		{Name: "x", Permissions: []spiceboxv1alpha1.SpiceDBPermission{{Name: "p", Expr: "b"}}},
	}}
	_, err := EmitSpicedbSchema([]*spiceboxv1alpha1.SpiceDBSchemaFragment{f1, f2})
	require.Error(t, err, "conflict must error")
	assert.Contains(t, err.Error(), "conflicting definitions", "error message describes conflict")
}

func TestEmitSpicedbSchema_AppendsRawZed(t *testing.T) {
	fragment := &spiceboxv1alpha1.SpiceDBSchemaFragment{
		RawZed: "definition fake_thing {\n  relation parent: user\n}\n",
	}
	got, err := EmitSpicedbSchema([]*spiceboxv1alpha1.SpiceDBSchemaFragment{fragment})
	require.NoError(t, err)
	assert.Contains(t, got, "definition fake_thing {")
	assert.Contains(t, got, "relation parent: user")
}

func TestEmitSpicedbSchema_StructuredAndRawCoexist(t *testing.T) {
	fragment := &spiceboxv1alpha1.SpiceDBSchemaFragment{
		Resources: []spiceboxv1alpha1.SpiceDBResource{
			{Name: "structured_res", Relations: []spiceboxv1alpha1.SpiceDBRelation{
				{Name: "owner", SubjectType: "user"},
			}},
		},
		RawZed: "definition raw_res {}\n",
	}
	got, err := EmitSpicedbSchema([]*spiceboxv1alpha1.SpiceDBSchemaFragment{fragment})
	require.NoError(t, err)
	assert.Contains(t, got, "definition structured_res {")
	assert.Contains(t, got, "definition raw_res {}")
}

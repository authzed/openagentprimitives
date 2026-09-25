package spicedb_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
)

const fixtureSchema = `
definition user {}

definition github_repo {
    relation reader: user
    relation writer: user
    permission read = reader + writer
    permission write = writer
}
`

func TestParseSchema_LoadsDefinitions(t *testing.T) {
	s, err := spicedb.ParseSchema(fixtureSchema)
	require.NoError(t, err, "ParseSchema")
	assert.True(t, s.HasDefinition("github_repo"), "github_repo should be present")
	assert.True(t, s.HasDefinition("user"), "user should be present")
	assert.False(t, s.HasDefinition("nope"), "nope should be absent")
}

func TestResolvePermission(t *testing.T) {
	s, err := spicedb.ParseSchema(fixtureSchema)
	require.NoError(t, err, "ParseSchema")
	cases := []struct {
		name       string
		definition string
		permission string
		want       spicedb.PermissionResolution
	}{
		{name: "permission name resolves as Permission", definition: "github_repo", permission: "read", want: spicedb.PermissionResolutionPermission},
		{name: "relation name resolves as Relation", definition: "github_repo", permission: "reader", want: spicedb.PermissionResolutionRelation},
		{name: "unknown name on known definition resolves as NotFound", definition: "github_repo", permission: "no_such", want: spicedb.PermissionResolutionNotFound},
		{name: "unknown definition resolves as NotFound", definition: "nope", permission: "any", want: spicedb.PermissionResolutionNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := s.ResolvePermission(tc.definition, tc.permission)
			assert.Equal(t, tc.want, got, "ResolvePermission(%q, %q)", tc.definition, tc.permission)
		})
	}
}

func TestParseSchema_RejectsGarbage(t *testing.T) {
	_, err := spicedb.ParseSchema("this is not a schema {{{")
	// Just confirm we got an error; exact message wording is brittle.
	assert.Error(t, err, "expected error on bad schema")
}

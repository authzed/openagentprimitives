package spicedb_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	authzschema "github.com/authzed/openagentprimitives/pkg/authz/spicedb/schema"
)

func TestSchema_ArtifactHasViewPermission(t *testing.T) {
	s, err := spicedb.ParseSchema(authzschema.Schema)
	require.NoError(t, err, "schema must compile")
	require.True(t, s.HasDefinition("artifact"), "schema must define `artifact`")
	assert.Equal(t, spicedb.PermissionResolutionPermission, s.ResolvePermission("artifact", "view"),
		"artifact must expose a `view` permission")
	assert.Equal(t, spicedb.PermissionResolutionRelation, s.ResolvePermission("artifact", "parent"),
		"artifact must have a `parent` relation")
	assert.Equal(t, spicedb.PermissionResolutionRelation, s.ResolvePermission("artifact", "platform"),
		"artifact must have a `platform` relation (platform admins view any artifact via platform->view_audit)")
}

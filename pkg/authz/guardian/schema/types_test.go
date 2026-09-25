package schema_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/guardian/schema"
)

func TestGrantPairRelationName(t *testing.T) {
	p := schema.GrantPair{ResourceType: "github_repo", Permission: "admin"}
	assert.Equal(t, "grant_admin_github_repo", p.RelationName())
}

func TestGrantPairPermissionName(t *testing.T) {
	p := schema.GrantPair{ResourceType: "github_repo", Permission: "admin"}
	assert.Equal(t, "check_admin_github_repo", p.PermissionName())
}

func TestDedupGrantPairs(t *testing.T) {
	in := []schema.GrantPair{
		{ResourceType: "github_repo", Permission: "admin"},
		{ResourceType: "linear_team", Permission: "view"},
		{ResourceType: "github_repo", Permission: "admin"}, // duplicate
		{ResourceType: "github_repo", Permission: "read"},
	}
	// Sorted: (github_repo, admin), (github_repo, read), (linear_team, view).
	want := []schema.GrantPair{
		{ResourceType: "github_repo", Permission: "admin"},
		{ResourceType: "github_repo", Permission: "read"},
		{ResourceType: "linear_team", Permission: "view"},
	}
	out := schema.DedupAndSort(in)
	require.Len(t, out, 3, "deduped length")
	assert.Equal(t, want, out, "deduped+sorted pairs")
}

package schema_test

import (
	"testing"

	"github.com/authzed/openagentprimitives/pkg/authz/guardian/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A repository slot opens a pool without changing its independently defined audience.
func TestGhToolkit_RepositoryMemoryAudience(t *testing.T) {
	for _, tt := range []struct{ resource, audience string }{
		{"github_repo_url", "owner + repo->can_read"},
		{"github_repo", "can_read"},
	} {
		t.Run(tt.resource, func(t *testing.T) {
			src := shippedSchema(t)
			out, _, skipped, err := schema.ComposeSlots(src, []schema.SlotPair{{ResourceType: tt.resource, Permission: "write_memory"}})
			require.NoError(t, err)
			require.Empty(t, skipped)
			block := definitionBlock(t, out, tt.resource)
			assert.Contains(t, block, "permission view_memory = "+tt.audience)
			assert.Contains(t, block, "relation slot_grant_write_memory")
			_, _, _, err = schema.ComposeSlots(src, []schema.SlotPair{{ResourceType: tt.resource, Permission: "view_memory"}})
			require.ErrorContains(t, err, "memory audience permission")
		})
	}
}

// A memory writer is a deliberately narrower standing grant than repository owner.
func TestComposeSlots_MemoryWriterDoesNotGrantCodePermissions(t *testing.T) {
	src := `definition project {
 relation owner: user
 relation memory_writer: service
 permission read = owner
 permission write = owner
 }`
	out, _, _, err := schema.ComposeSlots(src, []schema.SlotPair{
		{ResourceType: "project", Permission: "write_memory"},
		{ResourceType: "project", Permission: "read"},
		{ResourceType: "project", Permission: "write"},
	})
	require.NoError(t, err)
	assert.Contains(t, out, "permission write_memory = slot_grant_write_memory->interact + owner + memory_writer")
	assert.Contains(t, out, "permission read = slot_grant_read->interact + owner\n")
	assert.Contains(t, out, "permission write = slot_grant_write->interact + owner\n")
	again, changed, _, err := schema.ComposeSlots(out, []schema.SlotPair{{ResourceType: "project", Permission: "write_memory"}})
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Equal(t, out, again)
}

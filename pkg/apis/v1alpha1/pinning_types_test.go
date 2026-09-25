package v1alpha1_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// TestUpsertObservedPin covers the insert, update, and multi-name/multi-kind
// semantics of UpsertObservedPin: first write appends; same name+kind updates
// in-place; distinct name appends independently; same name but different kind
// appends independently (no cross-kind clobber).
func TestUpsertObservedPin(t *testing.T) {
	pin1 := spiceboxv1alpha1.PinRecord{Kind: "image", Strength: "named", Digest: "sha256:aaaa"}
	pin1updated := spiceboxv1alpha1.PinRecord{Kind: "image", Strength: "named", Digest: "sha256:bbbb"}
	pin2 := spiceboxv1alpha1.PinRecord{Kind: "image", Strength: "frozen", Digest: "sha256:cccc"}

	// Insert into empty list.
	list := spiceboxv1alpha1.UpsertObservedPin(nil, "box-a", pin1)
	require.Len(t, list, 1, "first insert produces one entry")
	assert.Equal(t, "box-a", list[0].Name)
	assert.Equal(t, "sha256:aaaa", list[0].Pin.Digest)

	// Upsert (same name + same kind) updates the existing entry, not appends.
	list = spiceboxv1alpha1.UpsertObservedPin(list, "box-a", pin1updated)
	require.Len(t, list, 1, "upsert must not duplicate")
	assert.Equal(t, "sha256:bbbb", list[0].Pin.Digest, "upsert must update the digest")

	// Different name appends independently.
	list = spiceboxv1alpha1.UpsertObservedPin(list, "box-b", pin2)
	require.Len(t, list, 2, "second distinct name produces a second entry")
	assert.Equal(t, "box-b", list[1].Name)
	assert.Equal(t, "sha256:cccc", list[1].Pin.Digest)

	// Same name but different kind must produce a second entry, not clobber the
	// existing image entry. This mirrors a scenario where an MCP server and a
	// SidecarToolbox happen to share the same name.
	pinMCP := spiceboxv1alpha1.PinRecord{Kind: "mcp", Strength: "named", Digest: "sha256:dddd"}
	list = spiceboxv1alpha1.UpsertObservedPin(list, "box-a", pinMCP)
	require.Len(t, list, 3, "same name + different kind must produce a new entry, not clobber")
	// Find the mcp entry.
	var mcpEntry *spiceboxv1alpha1.ObservedPin
	for i := range list {
		if list[i].Name == "box-a" && list[i].Pin.Kind == "mcp" {
			mcpEntry = &list[i]
		}
	}
	require.NotNil(t, mcpEntry, "mcp entry for box-a must exist")
	assert.Equal(t, "sha256:dddd", mcpEntry.Pin.Digest, "mcp entry must carry the mcp digest")
	// The original image entry for box-a must be unchanged.
	var imageEntry *spiceboxv1alpha1.ObservedPin
	for i := range list {
		if list[i].Name == "box-a" && list[i].Pin.Kind == "image" {
			imageEntry = &list[i]
		}
	}
	require.NotNil(t, imageEntry, "image entry for box-a must still exist")
	assert.Equal(t, "sha256:bbbb", imageEntry.Pin.Digest, "image entry must be unchanged")
}

package v1alpha1_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// TestUpsertSlotPin covers the insert, update, and multi-type semantics of
// UpsertSlotPin: first write appends; same ResourceType updates in-place
// (including clearing MovedBy/MovedAt back to a first-fill shape); a distinct
// ResourceType appends independently.
func TestUpsertSlotPin(t *testing.T) {
	pin1 := spiceboxv1alpha1.SlotPin{ResourceType: "github_repo", ResourceID: "owner/repo-a"}

	// Insert into empty list.
	list := spiceboxv1alpha1.UpsertSlotPin(nil, pin1)
	require.Len(t, list, 1, "first insert produces one entry")
	assert.Equal(t, "github_repo", list[0].ResourceType)
	assert.Equal(t, "owner/repo-a", list[0].ResourceID)
	assert.Empty(t, list[0].MovedBy, "a first-fill pin carries no MovedBy")
	assert.Nil(t, list[0].MovedAt, "a first-fill pin carries no MovedAt")

	// Upsert (same ResourceType) replaces the existing entry, not appends —
	// this is the shape a move produces: a new ResourceID plus MovedBy/MovedAt.
	movedAt := metav1.Now()
	pin1moved := spiceboxv1alpha1.SlotPin{
		ResourceType: "github_repo",
		ResourceID:   "owner/repo-b",
		MovedBy:      "phase-key-1",
		MovedAt:      &movedAt,
	}
	list = spiceboxv1alpha1.UpsertSlotPin(list, pin1moved)
	require.Len(t, list, 1, "upsert must not duplicate")
	assert.Equal(t, "owner/repo-b", list[0].ResourceID, "upsert must update the resource id")
	assert.Equal(t, "phase-key-1", list[0].MovedBy)
	require.NotNil(t, list[0].MovedAt)
	assert.True(t, movedAt.Time.Equal(list[0].MovedAt.Time))

	// Same-instance re-mirror (a re-grant, a redundant advisory read): the
	// entry already says everything this upsert would, so the recorded move
	// provenance is PRESERVED, not clobbered back to a first-fill shape.
	list = spiceboxv1alpha1.UpsertSlotPin(list,
		spiceboxv1alpha1.SlotPin{ResourceType: "github_repo", ResourceID: "owner/repo-b"})
	require.Len(t, list, 1, "same-instance upsert must not duplicate")
	assert.Equal(t, "phase-key-1", list[0].MovedBy, "same-instance re-grant must preserve MovedBy")
	require.NotNil(t, list[0].MovedAt, "same-instance re-grant must preserve MovedAt")
	assert.True(t, movedAt.Time.Equal(list[0].MovedAt.Time))

	// A DIFFERENT instance without move metadata still replaces wholesale —
	// the pin genuinely moved and the old provenance describes a move onto an
	// instance no longer pinned, so keeping it would attribute the wrong move.
	list = spiceboxv1alpha1.UpsertSlotPin(list,
		spiceboxv1alpha1.SlotPin{ResourceType: "github_repo", ResourceID: "owner/repo-c"})
	require.Len(t, list, 1)
	assert.Equal(t, "owner/repo-c", list[0].ResourceID)
	assert.Empty(t, list[0].MovedBy, "a new instance with unknown provenance must not inherit the old move's MovedBy")
	assert.Nil(t, list[0].MovedAt)

	// A distinct ResourceType appends independently.
	pin2 := spiceboxv1alpha1.SlotPin{ResourceType: "github_pr", ResourceID: "owner/repo#42"}
	list = spiceboxv1alpha1.UpsertSlotPin(list, pin2)
	require.Len(t, list, 2, "second distinct resource type produces a second entry")
	assert.Equal(t, "github_pr", list[1].ResourceType)
	assert.Equal(t, "owner/repo#42", list[1].ResourceID)

	// The first entry (github_repo) must be unchanged by the second insert.
	assert.Equal(t, "owner/repo-c", list[0].ResourceID, "unrelated insert must not disturb the existing entry")
}

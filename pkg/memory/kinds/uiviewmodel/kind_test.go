package uiviewmodel_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/uiviewmodel"
)

func TestKindIsMutableAndEssentialWhileLive(t *testing.T) {
	k := uiviewmodel.Kind{}
	r := k.Retention()
	assert.False(t, r.AppendOnly,
		"append-only would make the second update_view for a slot fail with ErrAppendOnlyConflict")
	assert.True(t, r.EssentialWhileLive,
		"a live session must not evict the agent's composed slots under a soft cap")
	assert.NotEmpty(t, r.ArchiveOn)
	assert.Positive(t, r.TTLAfterArchive)
}

func TestKindIsRegistered(t *testing.T) {
	// Importing this package runs its init(); memory.RegisterKind itself
	// panics at process start on a prefix collision with any other Kind, so a
	// passing run is the non-overlap proof — no prefix list to maintain here.
	_, ok := memory.LookupKind(uiviewmodel.KindName)
	assert.True(t, ok)
}

func TestKindIDPrefixMatchesConstant(t *testing.T) {
	k, ok := memory.LookupKind(uiviewmodel.KindName)
	require.True(t, ok)
	assert.Equal(t, uiviewmodel.IDPrefix, k.IDPrefix())
}

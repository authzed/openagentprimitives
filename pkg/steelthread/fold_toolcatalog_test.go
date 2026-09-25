package steelthread_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	bt "github.com/authzed/openagentprimitives/pkg/bronzethread"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/toolcatalog"
	"github.com/authzed/openagentprimitives/pkg/steelthread"
)

// oneCallTurns is the minimal transcript the catalog cases hang off: what the
// run was OFFERED is independent of what it CALLED, which is the whole reason
// the Kind exists.
func oneCallTurns() []memory.Turn {
	return []memory.Turn{
		userText(0, "list the widgets"),
		assistantCall(1, "tu_1", "acme_list_widgets", `{"operation_id":"op-1","_reason":"asked","args":{}}`),
		toolResult(2, "tu_1", `{"results":[]}`, false),
		assistantCall(3, "tu_2", "respond_to_user", `{"text":"none"}`),
	}
}

func TestFold_ToolCatalogs(t *testing.T) {
	cases := []struct {
		name     string
		recorded []toolcatalog.Content
		want     []bt.ToolCatalog
	}{
		{
			name: "every recorded change is carried, in turn order",
			recorded: []toolcatalog.Content{
				{FromTurnIndex: 1, Tools: []string{"a", "b"}, Digest: "irrelevant"},
				{FromTurnIndex: 9, Tools: []string{"a", "b", "c"}, Digest: "irrelevant"},
			},
			want: []bt.ToolCatalog{
				{FromTurnIndex: 1, Tools: []string{"a", "b"}},
				{FromTurnIndex: 9, Tools: []string{"a", "b", "c"}},
			},
		},
		{
			name: "rows arriving out of order are put in turn order: the step function is ambiguous otherwise",
			recorded: []toolcatalog.Content{
				{FromTurnIndex: 9, Tools: []string{"a", "c"}},
				{FromTurnIndex: 1, Tools: []string{"a"}},
			},
			want: []bt.ToolCatalog{
				{FromTurnIndex: 1, Tools: []string{"a"}},
				{FromTurnIndex: 9, Tools: []string{"a", "c"}},
			},
		},
		{
			name:     "an unsorted tool list is sorted: the bundle's own validation requires it",
			recorded: []toolcatalog.Content{{FromTurnIndex: 1, Tools: []string{"z", "a"}}},
			want:     []bt.ToolCatalog{{FromTurnIndex: 1, Tools: []string{"a", "z"}}},
		},
		{
			name:     "a session recorded before the Kind shipped pins nothing",
			recorded: nil,
			want:     nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := steelthread.Fold(
				steelthread.Records{Turns: oneCallTurns(), ToolCatalogs: tc.recorded}, foldOpts())
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.ToolCatalogs)
		})
	}
}

// The Digest is a pure function of Tools. A bundle carrying both would store a
// value it can derive, in a file a human edits — one more thing that can
// disagree with itself.
func TestFold_ToolCatalogsDropTheDerivedDigest(t *testing.T) {
	got, err := steelthread.Fold(steelthread.Records{
		Turns:        oneCallTurns(),
		ToolCatalogs: []toolcatalog.Content{{FromTurnIndex: 1, Tools: []string{"a"}, Digest: "deadbeef"}},
	}, foldOpts())
	require.NoError(t, err)

	require.Len(t, got.ToolCatalogs, 1)
	assert.Equal(t, bt.ToolCatalog{FromTurnIndex: 1, Tools: []string{"a"}}, got.ToolCatalogs[0])
}

// The fold must not reach back into the records it was handed: Records is read
// by the self-check and by DeriveAssertions after Fold runs, and a sort applied
// in place would reorder what they see.
func TestFold_ToolCatalogsDoNotMutateTheRecords(t *testing.T) {
	recorded := []toolcatalog.Content{{FromTurnIndex: 1, Tools: []string{"z", "a"}}}
	_, err := steelthread.Fold(steelthread.Records{Turns: oneCallTurns(), ToolCatalogs: recorded}, foldOpts())
	require.NoError(t, err)

	assert.Equal(t, []string{"z", "a"}, recorded[0].Tools, "Fold must sort a COPY")
}

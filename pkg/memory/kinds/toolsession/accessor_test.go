package toolsession_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/toolsession"
)

// testScope is a fixed session scope for accessor tests.
var testScope = memory.Scope{Kind: "session", ID: "ns/sess-1"}

func newMemory(t *testing.T) memory.Memory {
	t.Helper()
	return memory.NewLocal(inmem.NewBackend())
}

func TestRecordAndReadAll(t *testing.T) {
	// Three events spanning every interesting EventType + field set; a
	// long full-mode session would emit these in this order.
	events := []toolsession.Event{
		{
			ToolCallRef: "call-1",
			EventType:   "tool_use_start",
			ToolName:    "Bash",
			ToolID:      "toolu_abc",
			Summary:     "List files in the working directory",
		},
		{
			ToolCallRef: "call-1",
			EventType:   "tool_use_stop",
			ToolName:    "Bash",
			ToolID:      "toolu_abc",
			OK:          true,
		},
		{
			ToolCallRef: "call-1",
			EventType:   "result",
			OK:          true,
			DurationMs:  1234,
			CostUSD:     0.0042,
		},
	}

	ctx := memory.WithSystemApproval(context.Background(), "test")
	mem := newMemory(t)
	for i, ev := range events {
		require.NoErrorf(t, toolsession.Record(ctx, mem, testScope, ev), "Record event %d", i)
	}

	got, err := toolsession.ReadAll(ctx, mem, testScope)
	require.NoError(t, err, "ReadAll must succeed")
	// Records made in quick succession can share a CreatedAt to
	// microsecond, so ReadAll's CreatedAt sort is not a reliable total
	// order here — assert the recorded set, not strict slice order.
	assert.ElementsMatch(t, events, got)
}

func TestEntryToEvent(t *testing.T) {
	cases := []struct {
		name    string
		content []byte
		want    toolsession.Event
		wantErr bool
	}{
		{
			name: "valid content round-trips to its Event",
			content: mustMarshal(t, toolsession.Event{
				ToolCallRef: "call-9",
				EventType:   "text_delta",
				Text:        "hello world",
			}),
			want: toolsession.Event{
				ToolCallRef: "call-9",
				EventType:   "text_delta",
				Text:        "hello world",
			},
		},
		{
			name:    "malformed content returns an error",
			content: []byte("{not json"),
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := toolsession.EntryToEvent(memory.Entry{
				ID:      "toolsess-deadbeef",
				Content: tc.content,
			})
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestRecordRoundTripViaQuery(t *testing.T) {
	// Record an Event, fetch the raw Entry back via Query, decode it with
	// EntryToEvent — proves Record's stored Content survives the full
	// persist/query/decode path.
	ctx := memory.WithSystemApproval(context.Background(), "test")
	mem := newMemory(t)
	want := toolsession.Event{
		ToolCallRef: "call-rt",
		EventType:   "tool_use_start",
		ToolName:    "Read",
		ToolID:      "toolu_rt",
		Summary:     "Read a file",
	}
	require.NoError(t, toolsession.Record(ctx, mem, testScope, want))

	res, err := mem.Query(ctx, memory.Query{Scope: testScope, Kinds: []string{toolsession.KindName}})
	require.NoError(t, err)
	require.Len(t, res.Entries, 1, "exactly one entry recorded")

	got, err := toolsession.EntryToEvent(res.Entries[0])
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func mustMarshal(t *testing.T, ev toolsession.Event) []byte {
	t.Helper()
	raw, err := json.Marshal(ev)
	require.NoError(t, err)
	return raw
}

package lifecycle_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
)

func TestLifecycle_Registered(t *testing.T) {
	got, ok := memory.LookupKind("lifecycle")
	require.True(t, ok)
	assert.Equal(t, "lifecycle-", got.IDPrefix())
}

func TestLifecycle_HookRecordsEverySignal(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	now := time.Now().UTC().Truncate(time.Second)

	lifecycle.Setup(m)
	defer lifecycle.Teardown()

	require.NoError(t, m.SendSignal(memory.WithSystemApproval(context.Background(), "test"), memory.Signal{
		Kind: lifecycle.SigSessionStarted, Scope: scope, At: now,
	}))
	require.NoError(t, m.SendSignal(memory.WithSystemApproval(context.Background(), "test"), memory.Signal{
		Kind: lifecycle.SigSessionCompleted, Scope: scope, At: now.Add(time.Second),
	}))

	res, err := m.Query(memory.WithSystemApproval(context.Background(), "test"), memory.Query{
		Scope: scope, Kinds: []string{"lifecycle"},
		OrderBy: memory.OrderBy{Field: "createdAt"},
	})
	require.NoError(t, err)
	require.Len(t, res.Entries, 2)
	// The recorded tag is the signal kind inside the signal namespace, never the
	// bare kind: the kind is caller-supplied on the _signal route and the entry
	// is operator-signed, so the two tag namespaces are kept disjoint (see
	// SignalTagPrefix).
	assert.Equal(t, []string{lifecycle.SignalTag(lifecycle.SigSessionStarted)}, res.Entries[0].Tags)
	assert.Equal(t, []string{lifecycle.SignalTag(lifecycle.SigSessionCompleted)}, res.Entries[1].Tags)
}

// TestLifecycle_OnSignal_EntryAppendedIsNotRecorded covers the one signal
// kind OnSignal does NOT record (memory.SignalEntryAppended, a framework
// signal announcing that some entry landed, not a session lifecycle event),
// alongside a session lifecycle signal, which it does.
func TestLifecycle_OnSignal_EntryAppendedIsNotRecorded(t *testing.T) {
	cases := []struct {
		name        string
		signalKind  memory.SignalKind
		wantEntries int
	}{
		{
			name:        "session lifecycle signal: recorded",
			signalKind:  lifecycle.SigSessionStarted,
			wantEntries: 1,
		},
		{
			name:        "entry-appended signal: not recorded",
			signalKind:  memory.SignalEntryAppended,
			wantEntries: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := memory.NewLocal(inmem.NewBackend())
			scope := memory.Scope{Kind: "session", ID: "ns/a"}

			lifecycle.Setup(m)
			defer lifecycle.Teardown()

			require.NoError(t, m.SendSignal(memory.WithSystemApproval(context.Background(), "test"), memory.Signal{
				Kind: tc.signalKind, Scope: scope,
			}))

			res, err := m.Query(memory.WithSystemApproval(context.Background(), "test"), memory.Query{
				Scope: scope, Kinds: []string{"lifecycle"},
			})
			require.NoError(t, err, "Query lifecycle")
			assert.Len(t, res.Entries, tc.wantEntries)
		})
	}
}

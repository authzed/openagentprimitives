package lifecycle_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	lc "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
)

// TestReadOrdered_UndecodableEventEntryIsSkippedNotFatal pins the degradation
// the fold owes an operator.
//
// The lifecycle Kind is BOTH append-only and session-written, so a runner may
// append an EventTag-tagged entry whose content is not an Event envelope, and
// then nothing can remove it: per-entry Delete is refused unconditionally for an
// append-only Kind and DeleteScope skips it. A hard error here therefore made
// EVERY ReadOrdered for that scope fail forever — and the AgentSession
// reconciler folds the log on every pass (pkg/controllers/agentsession/
// sequencer.go), so one malformed record wedged the session permanently with no
// operator remedy. One bad entry must degrade the record, not halt it.
func TestReadOrdered_UndecodableEventEntryIsSkippedNotFatal(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	base := time.Unix(1770000000, 0).UTC()

	require.NoError(t, lifecycle.Append(ctx, m, scope, lc.TurnCompleted{}, base, lifecycle.OrderKey{}),
		"seed one well-formed transition event")

	// The poison: tagged as an event, content that is not an envelope. Written
	// through the same Put a session bearer reaches over HTTP.
	_, err := m.Put(ctx, memory.Entry{
		Scope:     scope,
		Kind:      "lifecycle",
		ID:        "lifecycle-evt-poison",
		CreatedAt: base.Add(time.Second),
		Tags:      []string{lifecycle.EventTag},
		Content:   []byte("{not an envelope"),
	})
	require.NoError(t, err, "the poisoned entry is accepted — lifecycle declares no ContentSchema")

	ordered, err := lifecycle.ReadOrdered(ctx, m, scope)
	require.NoError(t, err, "one undecodable entry must not fail the whole read")
	require.Len(t, ordered, 1, "the well-formed event survives the poisoned one")
	assert.IsType(t, lc.TurnCompleted{}, ordered[0].Event)
}

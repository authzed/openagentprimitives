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

// incarnationKey builds a runner-region key at seq attributed to the AgentSession
// instance uid.
func incarnationKey(seq uint64, uid string) lifecycle.OrderKey {
	return lifecycle.OrderKey{Seq: seq, Region: string(lc.RegionRunner), SessionUID: uid}
}

// TestForIncarnation_DropsAPriorIncarnationKeepsUnstamped is the unit-level
// statement of the sharing rule: one durable scope can hold the events of more
// than one AgentSession instance, because the scope is keyed by
// namespace/name and a deleted CR can be re-created under the same name.
// Folding "this session" must therefore consume only this instance's events.
//
// An event carrying NO SessionUID is kept for every incarnation: that is the
// shape every entry written before the ordering key existed has, and dropping
// those would erase a live session's own history at upgrade.
func TestForIncarnation_DropsAPriorIncarnationKeepsUnstamped(t *testing.T) {
	evs := []lifecycle.OrderedEvent{
		{EntryID: "a", Event: lc.RunnerClaimed{}, Key: incarnationKey(1, "uid-1")},
		{EntryID: "b", Event: lc.RunnerCrash{}, Key: incarnationKey(2, "uid-1")},
		{EntryID: "c", Event: lc.TurnCompleted{}, Key: lifecycle.OrderKey{}},
		{EntryID: "d", Event: lc.RunnerClaimed{}, Key: incarnationKey(3, "uid-2")},
	}

	got := lifecycle.ForIncarnation(evs, "uid-2")
	ids := make([]string, len(got))
	for i := range got {
		ids[i] = got[i].EntryID
	}
	assert.Equal(t, []string{"c", "d"}, ids,
		"a different instance's events are dropped; unstamped events are kept")

	all := lifecycle.ForIncarnation(evs, "")
	assert.Len(t, all, 4,
		"an empty uid means the caller cannot identify an instance and must fold everything")
}

// TestEventsForIncarnation_ReadsOnlyThisInstance is the same rule through the
// accessor the operator and the runner fold on.
func TestEventsForIncarnation_ReadsOnlyThisInstance(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/shared-name"}
	base := time.Unix(1770000000, 0).UTC()

	require.NoError(t, lifecycle.Append(ctx, m, scope, lc.RunnerClaimed{}, base, incarnationKey(1, "uid-1")))
	require.NoError(t, lifecycle.Append(ctx, m, scope, lc.RunnerCrash{}, base.Add(time.Second), incarnationKey(2, "uid-1")))

	first, err := lifecycle.EventsForIncarnation(ctx, m, scope, "uid-1")
	require.NoError(t, err)
	require.Len(t, first, 2, "the instance that wrote them still folds its own events")
	assert.Equal(t, lc.PhaseFailed, lc.Fold(first).Phase)

	second, err := lifecycle.EventsForIncarnation(ctx, m, scope, "uid-2")
	require.NoError(t, err)
	assert.Empty(t, second, "a re-created session inherits nothing from the instance it replaced")
}

// TestTerminalStopKey_CarriesTheIncarnation pins the one keyed event that used
// to carry no instance identity. A SIGTERM Stopped folds to Failed on its own,
// so an unattributed one leaks a terminal state into whatever session next
// holds the name.
func TestTerminalStopKey_CarriesTheIncarnation(t *testing.T) {
	k := lifecycle.TerminalStopKey("uid-1")
	assert.Equal(t, "uid-1", k.SessionUID, "the sentinel key names the instance that stopped")
	assert.Equal(t, lifecycle.SeqTerminalStop, k.Seq, "it still sorts after every keyed event")

	evs := []lifecycle.OrderedEvent{{EntryID: "s", Event: lc.Stopped{}, Key: k}}
	assert.Empty(t, lifecycle.ForIncarnation(evs, "uid-2"),
		"a prior instance's process-termination Stopped is not this session's terminal")
}

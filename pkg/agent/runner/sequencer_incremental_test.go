package runner

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
)

// countingLifecycleMemory measures what the sequencer's per-event fold actually
// costs: entries handed back by Query, which in production is the decode work
// and the bytes over HTTP, all of it inside seqMu.
//
// It also stamps each append from its own clock rather than letting the caller's
// time.Now() land, so the log spreads over wall-clock time the way a real
// session's does. That matters because memory.Query's only tail predicate
// (Since) filters on createdAt: a log whose entries all share one instant has no
// tail to read, so the spread is what makes read cost observable at all. The
// same technique is used by the seam's own cost test.
type countingLifecycleMemory struct {
	memory.Memory

	mu          sync.Mutex
	queries     int
	entriesRead int
	clock       time.Time
	step        time.Duration
}

func (c *countingLifecycleMemory) Put(ctx context.Context, e memory.Entry) (memory.Entry, error) {
	c.mu.Lock()
	c.clock = c.clock.Add(c.step)
	e.CreatedAt = c.clock
	c.mu.Unlock()
	return c.Memory.Put(ctx, e)
}

func (c *countingLifecycleMemory) Query(ctx context.Context, q memory.Query) (memory.QueryResult, error) {
	res, err := c.Memory.Query(ctx, q)
	c.mu.Lock()
	c.queries++
	c.entriesRead += len(res.Entries)
	c.mu.Unlock()
	return res, err
}

// counts snapshots the read counters so an assertion can be made BEFORE any
// verification read of the log adds to them.
func (c *countingLifecycleMemory) counts() (queries, entries int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.queries, c.entriesRead
}

// newSequencerCostLoop builds the minimal Loop the sequencer needs: a session
// key and a lifecycle log. Everything else applyEvent touches on the
// TurnCompleted path (SessionContext, PublishPlanActivity) is nil-safe.
func newSequencerCostLoop(t *testing.T) (*Loop, *countingLifecycleMemory) {
	t.Helper()
	mem := &countingLifecycleMemory{
		Memory: memory.NewLocal(inmem.NewBackend()),
		clock:  time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC),
		step:   time.Minute,
	}
	return &Loop{
		SessionKey:      memory.NamespacedName{Namespace: "ns", Name: "seq-cost"},
		LifecycleMemory: mem,
	}, mem
}

func sequencerTestCtx() context.Context {
	return memory.WithSystemApproval(context.Background(), "test")
}

// TestApplyEvent_PerEventFoldReadsOnlyTheTail_NotTheWholeLog is the cost test
// for the sequencer defect: applyEvent folds the WHOLE append-only transition
// log on every event it emits, under seqMu, so a session's lifecycle bookkeeping
// costs O(E²) entries decoded (and bytes over HTTP) in E, which is unbounded in
// session age.
//
// At E=100 the full-read baseline is 0+1+…+99 = 4950 entries read. Folding the
// tail instead must land far below that. The assertion is on read COST, not wall
// clock, so it does not flake under load.
func TestApplyEvent_PerEventFoldReadsOnlyTheTail_NotTheWholeLog(t *testing.T) {
	l, mem := newSequencerCostLoop(t)
	ctx := sequencerTestCtx()

	const events = 100
	for i := 0; i < events; i++ {
		l.emitLifecycleEvent(ctx, lifecyclecore.TurnCompleted{})
	}
	queries, entriesRead := mem.counts()

	// The fold must have had a growing log to read: without this the cost
	// assertion below would pass trivially on an empty log.
	logged, err := lifecycle.Events(ctx, mem, l.lifecycleScope())
	require.NoError(t, err, "read back the transition log")
	require.Len(t, logged, events, "every emitted event must have been appended")

	const fullReadBaseline = events * (events - 1) / 2 // fold-before-append: 4950
	t.Logf("entries read across %d per-event folds: %d (full-read baseline %d, %d queries)",
		events, entriesRead, fullReadBaseline, queries)
	assert.Less(t, entriesRead, 1500,
		"the per-event fold must read the TAIL of the transition log, not the whole log: "+
			"the full-read baseline is %d entries for %d events", fullReadBaseline, events)
}

// TestApplyEvent_BackdatedOperatorEventIsRecoveredByThePeriodicFullReRead pins
// the safety net at the RUNNER's level, not just the reader's. A tail read is
// bounded by a lookback window over a publisher-stamped wall clock, so the
// operator writing this same scope from a badly-skewed clock can land an event
// below the runner's watermark, where no tail read can ever see it. That is only
// acceptable because the fold re-reads the whole log every
// lifecycle.FullReadEvery folds. If a future edit reset or bypassed the reader
// per fold, the repair would go with it — silently.
func TestApplyEvent_BackdatedOperatorEventIsRecoveredByThePeriodicFullReRead(t *testing.T) {
	l, mem := newSequencerCostLoop(t)
	ctx := sequencerTestCtx()

	l.emitLifecycleEvent(ctx, lifecyclecore.RunnerClaimed{})
	l.emitLifecycleEvent(ctx, lifecyclecore.TurnCompleted{})
	running, err := l.foldLifecycle(ctx)
	require.NoError(t, err, "fold while the runner holds the live region")
	require.Equal(t, lifecyclecore.PhaseRunning, running.Phase, "the claimed session folds to Running")

	// The operator appends with a clock far outside the lookback window. It goes
	// through the inner memory so the fixture's synthetic clock does not restamp
	// it, and carries a high Seq so it folds LAST and therefore decides the phase.
	backdated := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC).Add(-2 * lifecycle.IncrementalLookback)
	require.NoError(t, lifecycle.Append(ctx, mem.Memory, l.lifecycleScope(), lifecyclecore.IdleYield{},
		backdated, lifecycle.OrderKey{Seq: lifecycle.SeqTerminalStop - 1, Region: string(lifecyclecore.RegionOperatorPost)}),
		"badly-backdated operator append")

	// Honest about the limit: the tail read alone cannot see it.
	tail, err := l.foldLifecycle(ctx)
	require.NoError(t, err, "tail fold after the backdated append")
	require.Equal(t, lifecyclecore.PhaseRunning, tail.Phase,
		"an entry outside the lookback window is invisible to a tail read")

	// …and the periodic full re-read must recover it within FullReadEvery folds.
	var got lifecyclecore.State
	for i := 0; i < lifecycle.FullReadEvery; i++ {
		got, err = l.foldLifecycle(ctx)
		require.NoError(t, err, "fold %d while waiting for the periodic full re-read", i)
		if got.Phase == lifecyclecore.PhaseIdle {
			break
		}
	}
	assert.Equal(t, lifecyclecore.PhaseIdle, got.Phase,
		"the periodic full re-read must recover the event a tail read missed, within FullReadEvery folds")
}

// TestApplyEvent_FoldedStateMatchesAFullReadOfTheLog is the correctness half:
// an incremental fold must be indistinguishable from folding a complete re-read,
// including after the OTHER publisher (the operator) appends to the same scope
// while the runner is live — the two-publisher hazard that makes a seed-once
// cache wrong for this kind.
func TestApplyEvent_FoldedStateMatchesAFullReadOfTheLog(t *testing.T) {
	l, mem := newSequencerCostLoop(t)
	ctx := sequencerTestCtx()

	for i := 0; i < 5; i++ {
		l.emitLifecycleEvent(ctx, lifecyclecore.TurnCompleted{})
	}

	// The operator appends to the same scope, out-of-band from the runner's
	// sequencer, exactly as it does for Expired / Sleep / RetryTTLExpired.
	require.NoError(t, lifecycle.Append(ctx, mem, l.lifecycleScope(), lifecyclecore.IdleYield{},
		time.Time{}, lifecycle.OrderKey{Seq: 1, Region: string(lifecyclecore.RegionOperatorPost)}),
		"operator-side append to the shared lifecycle log")

	got, err := l.foldLifecycle(ctx)
	require.NoError(t, err, "fold after the operator's out-of-band append")

	events, err := lifecycle.Events(ctx, mem, l.lifecycleScope())
	require.NoError(t, err, "full read of the transition log")
	assert.Equal(t, lifecyclecore.Fold(events), got,
		"the sequencer's fold must equal a fold of a complete re-read, including the other publisher's events")
}

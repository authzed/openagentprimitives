package lifecycle_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	lc "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	kind "github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
)

// countingMemory records what every Query actually shipped back, which is the
// quantity the O(E²) defect is measured in: entries decoded (and, in production,
// bytes over HTTP) per fold. It counts nothing else, so a test's assertion reads
// as a direct statement about read cost.
type countingMemory struct {
	memory.Memory
	queries      int
	entriesRead  int
	queryErr     error // when set, every Query fails with it
	sawSincePred bool  // whether any Query carried a Since bound
}

func (c *countingMemory) Query(ctx context.Context, q memory.Query) (memory.QueryResult, error) {
	if c.queryErr != nil {
		return memory.QueryResult{}, c.queryErr
	}
	if q.Since != nil {
		c.sawSincePred = true
	}
	res, err := c.Memory.Query(ctx, q)
	c.queries++
	c.entriesRead += len(res.Entries)
	return res, err
}

func testCtx() context.Context {
	return memory.WithSystemApproval(context.Background(), "test")
}

// newCountingLog returns a fresh lifecycle scope over an inmem backend, wrapped
// so reads are counted.
func newCountingLog(t *testing.T) (*countingMemory, memory.Scope) {
	t.Helper()
	return &countingMemory{Memory: memory.NewLocal(inmem.NewBackend())},
		memory.Scope{Kind: "session", ID: "ns/incr"}
}

// appendRunnerEvent appends one runner-region event stamped at `at`.
func appendRunnerEvent(t *testing.T, m memory.Memory, scope memory.Scope, turn int, at time.Time) {
	t.Helper()
	require.NoError(t, kind.Append(testCtx(), m, scope, lc.TurnCompleted{}, at, runnerKey(turn, 1)),
		"append runner event for turn %d", turn)
}

// TestIncrementalReader_PerEventFoldReadsOnlyTheTail_NotTheWholeLog is the cost
// test for the defect: the runner's sequencer folds the whole transition log on
// every event it emits, so a session's lifecycle bookkeeping costs O(E²) entries
// read (and, in production, O(E²) bytes over HTTP under seqMu).
//
// At E=100 the full-read baseline is 1+2+…+100 = 5050 entries. The tail read
// (IncrementalLookback behind the newest createdAt, deduped by EntryID, plus a
// full re-read every FullReadEvery reads) must land far below that.
func TestIncrementalReader_PerEventFoldReadsOnlyTheTail_NotTheWholeLog(t *testing.T) {
	m, scope := newCountingLog(t)
	base := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)

	const events = 100
	// One event per minute: real sessions spread their lifecycle events over
	// wall-clock time, and a lookback window can only save reads when they do.
	var reader kind.IncrementalReader
	for i := 0; i < events; i++ {
		appendRunnerEvent(t, m, scope, i, base.Add(time.Duration(i)*time.Minute))
		got, err := reader.Read(testCtx(), m, scope)
		require.NoError(t, err, "Read after append %d", i)
		require.Len(t, got, i+1, "every appended event must still be folded after append %d", i)
	}

	const fullReadBaseline = events * (events + 1) / 2 // 5050
	t.Logf("entries read across %d per-event folds: %d (full-read baseline %d, %d queries)",
		events, m.entriesRead, fullReadBaseline, m.queries)
	assert.Less(t, m.entriesRead, 1500,
		"per-event fold must read the TAIL, not the whole log: full-read baseline is %d entries for %d events",
		fullReadBaseline, events)
	assert.True(t, m.sawSincePred,
		"the incremental read must bound its query with Since; an unbounded query is the defect")
}

// TestIncrementalReader_ReadEqualsFullReadOrdered_AcrossInterleavedPublishers
// pins the property that makes the seam adoptable: merging tails must be
// indistinguishable from re-reading everything, including for a two-publisher
// log whose createdAt stamps do not agree with its logical order.
func TestIncrementalReader_ReadEqualsFullReadOrdered_AcrossInterleavedPublishers(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/equiv"}
	base := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)

	// The operator's pre-region event for each turn is stamped from a clock that
	// runs 20s BEHIND the runner's, so wall-clock order and fold order disagree —
	// exactly what OrderKey exists to defeat.
	steps := []step{}
	for turn := 0; turn < 6; turn++ {
		at := base.Add(time.Duration(turn) * time.Minute)
		steps = append(steps,
			step{ev: lc.RunnerClaimed{}, at: at.Add(-20 * time.Second), key: opPreKey(turn)},
			step{ev: lc.TurnCompleted{}, at: at, key: runnerKey(turn, 1)},
		)
	}

	var reader kind.IncrementalReader
	for i, s := range steps {
		require.NoError(t, kind.Append(testCtx(), m, scope, s.ev, s.at, s.key), "append step %d", i)
		want, err := kind.ReadOrdered(testCtx(), m, scope)
		require.NoError(t, err, "ReadOrdered after step %d", i)
		got, err := reader.Read(testCtx(), m, scope)
		require.NoError(t, err, "incremental Read after step %d", i)
		assert.Equal(t, want, got, "incremental Read must equal a full ReadOrdered after step %d", i)
	}
}

// TestIncrementalReader_SecondPublisherBehindTheWatermarkIsStillObserved is the
// two-publisher hazard the lookback window exists for. A bare high-water mark
// (Since = newest createdAt seen) silently loses an entry the OTHER publisher
// writes with an earlier stamp; the lookback window plus EntryID dedup does not.
func TestIncrementalReader_SecondPublisherBehindTheWatermarkIsStillObserved(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/skew"}
	base := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)

	// The runner establishes the watermark.
	for turn := 0; turn < 3; turn++ {
		require.NoError(t, kind.Append(testCtx(), m, scope, lc.TurnCompleted{},
			base.Add(time.Duration(turn)*time.Minute), runnerKey(turn, 1)), "runner append %d", turn)
	}
	newest := base.Add(2 * time.Minute)

	var reader kind.IncrementalReader
	seen, err := reader.Read(testCtx(), m, scope)
	require.NoError(t, err, "first Read")
	require.Len(t, seen, 3, "first Read is a full read")

	// The operator now appends with a clock 90s behind the runner's — BELOW the
	// newest createdAt the reader has already seen, but inside the lookback.
	require.NoError(t, kind.Append(testCtx(), m, scope, lc.RunnerClaimed{},
		newest.Add(-90*time.Second), opPreKey(3)), "operator append behind the watermark")

	// A bare high-water mark cannot see it — this is what makes the naive cache
	// wrong, asserted so the lookback is never "simplified" away.
	bare, err := kind.ReadOrderedSince(testCtx(), m, scope, newest)
	require.NoError(t, err, "ReadOrderedSince at a bare high-water mark")
	for _, e := range bare {
		assert.NotEqual(t, opPreKey(3), e.Key,
			"a bare high-water mark must NOT see the lagging publisher's entry (that is the hazard)")
	}

	got, err := reader.Read(testCtx(), m, scope)
	require.NoError(t, err, "second Read")
	assert.Len(t, got, 4, "the lookback window must pick up the lagging publisher's entry")
}

// TestIncrementalReader_EntryOlderThanTheLookbackIsRecoveredByTheFullReRead
// pins the safety net. No finite window over a publisher-stamped wall clock can
// be sufficient (see IncrementalReader's doc), so the reader re-reads everything
// every FullReadEvery reads. That converts a permanent, silent fold error into a
// bounded, self-healing one — which is the only reason a tail read is safe on a
// correctness path at all.
func TestIncrementalReader_EntryOlderThanTheLookbackIsRecoveredByTheFullReRead(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/backdated"}
	base := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)

	require.NoError(t, kind.Append(testCtx(), m, scope, lc.TurnCompleted{}, base, runnerKey(0, 1)),
		"runner append establishing the watermark")

	var reader kind.IncrementalReader
	seen, err := reader.Read(testCtx(), m, scope)
	require.NoError(t, err, "first Read")
	require.Len(t, seen, 1, "first Read is a full read")

	// A publisher whose clock is far outside the lookback window.
	require.NoError(t, kind.Append(testCtx(), m, scope, lc.RunnerClaimed{},
		base.Add(-2*kind.IncrementalLookback), opPreKey(0)), "badly backdated append")

	// Honest about the limit: the tail read alone cannot see it.
	got, err := reader.Read(testCtx(), m, scope)
	require.NoError(t, err, "tail Read after the backdated append")
	require.Len(t, got, 1, "an entry outside the lookback window is not visible to a tail read")

	// …and the periodic full re-read recovers it within FullReadEvery reads.
	for i := 0; i < kind.FullReadEvery; i++ {
		got, err = reader.Read(testCtx(), m, scope)
		require.NoError(t, err, "Read %d while waiting for the full re-read", i)
		if len(got) == 2 {
			break
		}
	}
	assert.Len(t, got, 2,
		"the periodic full re-read must recover an entry the tail read missed, within FullReadEvery reads")
}

// TestIncrementalReader_QueryErrorLeavesPriorStateIntactForTheNextRead: a failed
// read must not be mistaken for an empty log, and must not discard what the
// reader already holds — folding a truncated log would move the session's
// lifecycle phase backwards.
func TestIncrementalReader_QueryErrorLeavesPriorStateIntactForTheNextRead(t *testing.T) {
	m, scope := newCountingLog(t)
	base := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	appendRunnerEvent(t, m, scope, 0, base)

	var reader kind.IncrementalReader
	first, err := reader.Read(testCtx(), m, scope)
	require.NoError(t, err, "first Read")
	require.Len(t, first, 1, "first Read sees the appended event")

	boom := errors.New("memory unavailable")
	m.queryErr = boom
	_, err = reader.Read(testCtx(), m, scope)
	require.ErrorIs(t, err, boom, "a query failure must be returned, never swallowed into an empty log")

	m.queryErr = nil
	again, err := reader.Read(testCtx(), m, scope)
	require.NoError(t, err, "Read after recovery")
	assert.Len(t, again, 1, "the reader's held log must survive a failed Read")
}

// TestMergeOrdered covers the pure merge on its own: the lookback window always
// re-delivers entries the reader already holds, so dedup by EntryID plus a
// re-sort under the package's own total order is the whole contract.
func TestMergeOrdered(t *testing.T) {
	base := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	ev := func(id string, turn int, at time.Time) kind.OrderedEvent {
		return kind.OrderedEvent{EntryID: id, Event: lc.TurnCompleted{}, Key: runnerKey(turn, 1), CreatedAt: at}
	}
	a := ev("lifecycle-a", 0, base)
	b := ev("lifecycle-b", 1, base.Add(time.Minute))
	c := ev("lifecycle-c", 2, base.Add(2*time.Minute))

	cases := []struct {
		name        string
		prior       []kind.OrderedEvent
		fresh       []kind.OrderedEvent
		wantEntries []string
	}{
		{
			name:        "empty fresh: prior returned unchanged",
			prior:       []kind.OrderedEvent{a, b},
			fresh:       nil,
			wantEntries: []string{"lifecycle-a", "lifecycle-b"},
		},
		{
			name:        "overlapping window: the re-delivered entry appears once",
			prior:       []kind.OrderedEvent{a, b},
			fresh:       []kind.OrderedEvent{b, c},
			wantEntries: []string{"lifecycle-a", "lifecycle-b", "lifecycle-c"},
		},
		{
			name:        "fresh arriving out of order: result is re-sorted by the total order",
			prior:       []kind.OrderedEvent{a},
			fresh:       []kind.OrderedEvent{c, b},
			wantEntries: []string{"lifecycle-a", "lifecycle-b", "lifecycle-c"},
		},
		{
			name:        "empty prior: fresh is the whole log",
			prior:       nil,
			fresh:       []kind.OrderedEvent{c, a, b},
			wantEntries: []string{"lifecycle-a", "lifecycle-b", "lifecycle-c"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := kind.MergeOrdered(tc.prior, tc.fresh)
			ids := make([]string, len(got))
			for i := range got {
				ids[i] = got[i].EntryID
			}
			assert.Equal(t, tc.wantEntries, ids)
		})
	}
}

// TestMergeOrdered_DoesNotMutateItsInputs: the reader hands its own held slice
// in as prior on every read, so an in-place sort would corrupt the log a
// concurrent caller is still folding.
func TestMergeOrdered_DoesNotMutateItsInputs(t *testing.T) {
	base := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	prior := []kind.OrderedEvent{
		{EntryID: "lifecycle-a", Event: lc.TurnCompleted{}, Key: runnerKey(0, 1), CreatedAt: base},
		{EntryID: "lifecycle-b", Event: lc.TurnCompleted{}, Key: runnerKey(1, 1), CreatedAt: base.Add(time.Minute)},
	}
	fresh := []kind.OrderedEvent{
		{EntryID: "lifecycle-z", Event: lc.TurnCompleted{}, Key: runnerKey(0, 0), CreatedAt: base.Add(-time.Minute)},
	}
	priorCopy := append([]kind.OrderedEvent(nil), prior...)
	freshCopy := append([]kind.OrderedEvent(nil), fresh...)

	out := kind.MergeOrdered(prior, fresh)
	require.Len(t, out, 3, "merged log holds every distinct entry")
	assert.Equal(t, priorCopy, prior, "prior must not be mutated")
	assert.Equal(t, freshCopy, fresh, "fresh must not be mutated")
}

// TestNextSince_SubtractsTheLookbackFromTheNewestStamp: the watermark is
// deliberately BEHIND the newest entry seen, and is zero (a full read) before
// anything has been seen.
func TestNextSince_SubtractsTheLookbackFromTheNewestStamp(t *testing.T) {
	base := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	assert.True(t, kind.NextSince(nil).IsZero(), "nothing seen yet: the first read must be a full read")

	seen := []kind.OrderedEvent{
		{EntryID: "lifecycle-a", CreatedAt: base},
		{EntryID: "lifecycle-b", CreatedAt: base.Add(5 * time.Minute)},
		{EntryID: "lifecycle-c", CreatedAt: base.Add(time.Minute)},
	}
	assert.Equal(t, base.Add(5*time.Minute).Add(-kind.IncrementalLookback), kind.NextSince(seen),
		"the watermark is the newest stamp seen minus the lookback, regardless of slice order")
}

// TestReadOrderedSince_ZeroSinceReadsEverything guards the no-behavior-change
// half of the seam: ReadOrdered is now ReadOrderedSince with a zero bound, and
// a zero bound must set no predicate at all rather than a 1970 lower bound.
func TestReadOrderedSince_ZeroSinceReadsEverything(t *testing.T) {
	m, scope := newCountingLog(t)
	base := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	for turn := 0; turn < 3; turn++ {
		appendRunnerEvent(t, m, scope, turn, base.Add(time.Duration(turn)*time.Minute))
	}

	all, err := kind.ReadOrderedSince(testCtx(), m, scope, time.Time{})
	require.NoError(t, err, "ReadOrderedSince(zero)")
	assert.Len(t, all, 3, "a zero bound reads the whole log")
	assert.False(t, m.sawSincePred, "a zero bound must not send a Since predicate")

	tail, err := kind.ReadOrderedSince(testCtx(), m, scope, base.Add(time.Minute))
	require.NoError(t, err, "ReadOrderedSince(bound)")
	assert.Len(t, tail, 2, "the bound is inclusive: entries at or after it are returned")
	assert.True(t, m.sawSincePred, "a non-zero bound must send a Since predicate")
}

// TestOrderedEvent_CarriesEntryIDAndCreatedAt: without these two fields no
// caller can dedupe a re-read or derive a watermark, which is why the fix could
// not be made from the sequencer.
func TestOrderedEvent_CarriesEntryIDAndCreatedAt(t *testing.T) {
	m, scope := newCountingLog(t)
	at := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	appendRunnerEvent(t, m, scope, 0, at)

	got, err := kind.ReadOrdered(testCtx(), m, scope)
	require.NoError(t, err, "ReadOrdered")
	require.Len(t, got, 1, "one event appended")
	assert.NotEmpty(t, got[0].EntryID, "the entry ID is the dedup key an incremental read needs")
	assert.True(t, at.Equal(got[0].CreatedAt), "createdAt is the value the watermark is derived from")
}

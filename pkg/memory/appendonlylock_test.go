package memory_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
)

// barrierBackend is a real inmem.Backend whose Get holds each caller until
// every expected writer has READ the contested key — or until grace elapses,
// whichever comes first — and only then hands each its result.
//
// The order matters and was got wrong once: holding writers BEFORE the read
// achieves nothing, because they are released one at a time and the first one
// stores before the next reads, so the rest correctly see a conflict. The
// window this file is about opens the moment a writer has been told the entry
// is ABSENT and has not yet stored. Blocking after the read is what pries that
// window open for all of them at once.
//
// It exists because a plain burst of goroutines does NOT reliably reproduce the
// race. That window is a few hundred nanoseconds wide against inmem (a map
// under a mutex), and each writer crosses it before the next is scheduled: 32
// goroutines released from one closed channel produced exactly one winner on
// every run, WITH THE LOCK REMOVED. A test like that asserts nothing.
//
// Widening the window is not a thumb on the scale, it is the production shape.
// The operator ships MEMORY_BACKEND=postgres (config/manager/deployment.yaml),
// where the pre-check Get is a network round trip and every concurrent writer
// really does sit inside that window at once. The barrier reproduces it
// deterministically instead of hoping the scheduler does.
type barrierBackend struct {
	*inmem.Backend

	want  int           // writers expected to meet inside Get
	grace time.Duration // how long one caller waits for the others

	mu      sync.Mutex
	arrived int
	release chan struct{}
}

func newBarrierBackend(want int, grace time.Duration) *barrierBackend {
	return &barrierBackend{
		Backend: inmem.NewBackend(),
		want:    want,
		grace:   grace,
		release: make(chan struct{}),
	}
}

func (b *barrierBackend) Get(ctx context.Context, scope memory.Scope, kind, id string) (memory.Entry, bool, error) {
	e, found, err := b.Backend.Get(ctx, scope, kind, id)

	b.mu.Lock()
	b.arrived++
	if b.arrived == b.want {
		close(b.release)
	}
	release := b.release
	b.mu.Unlock()

	// The grace fallback is what makes this safe under the lock: serialized
	// writers can never all be inside Get at once, so each waits out grace and
	// proceeds. That costs the test time; it cannot change its verdict, because
	// mutual exclusion does not depend on timing.
	select {
	case <-release:
	case <-time.After(b.grace):
	}
	return e, found, err
}

// TestPutAppendOnlyConcurrentWritersExactlyOneWins is the regression for the
// facade's append-only check-then-write span being non-atomic: the pre-check
// Get and the backend Put were separated by provenance verification and
// approval minting with nothing serializing them, so two writers of DIFFERENT
// content for one (scope, kind, id) could both find the entry absent and both
// store, the second silently overwriting the first. That defeats write-once for
// every append-only Kind, and it leaves no trace — the loser's bytes are simply
// gone, with no error and no log line.
//
// Sequential writers already pin the CONFLICT rule (TestPutAppendOnlyConflict);
// only concurrent ones pin that the rule survives the window between the check
// and the write. Run under -race.
//
// The claim is three-part, which is why the assertions are not one "no error":
// exactly one writer succeeds, every other loses with ErrAppendOnlyConflict
// rather than some other failure, and the value left in the store is the
// winner's own — not a loser's overwrite.
func TestPutAppendOnlyConcurrentWritersExactlyOneWins(t *testing.T) {
	t.Cleanup(memory.ResetRegistryForTest)
	memory.RegisterKind(appendOnlyKind{name: "ao-race", prefix: "aor-"})

	const (
		writers   = 8
		kindName  = "ao-race"
		contested = "aor-contested"
		grace     = 50 * time.Millisecond
	)
	mem := memory.NewLocal(newBarrierBackend(writers, grace))
	scope := memory.Scope{Kind: "session", ID: "nsRace/sessRace"}
	// One fixed timestamp across every writer, so CONTENT is the only thing that
	// differs. entriesEquivalent compares content, createdAt, tags and links; if
	// the timestamps differed too, a passing test would not show that differing
	// content alone is enough to make two writes conflict.
	createdAt := time.Unix(0, 0).UTC()

	errs := make([]error, writers)
	returned := make([]memory.Entry, writers)

	// A closed channel rather than a countdown: every goroutine is already parked
	// on the receive when it opens, so they enter Put as closely together as the
	// scheduler allows, and the barrier inside Get holds them there.
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e := memory.Entry{
				Scope:     scope,
				Kind:      kindName,
				ID:        contested,
				CreatedAt: createdAt,
				Content:   json.RawMessage(fmt.Sprintf(`{"writer":%d}`, i)),
			}
			<-start
			returned[i], errs[i] = mem.Put(context.Background(), e)
		}()
	}
	close(start)
	wg.Wait()

	var winners []int
	for i, err := range errs {
		if err == nil {
			winners = append(winners, i)
			continue
		}
		assert.ErrorIsf(t, err, memory.ErrAppendOnlyConflict,
			"writer %d lost, which is correct, but must lose with ErrAppendOnlyConflict; got %v", i, err)
	}
	require.Lenf(t, winners, 1,
		"exactly one of %d concurrent writers of different content may store to one append-only "+
			"(scope, kind, id); %d succeeded (%v). More than one means the facade's check-then-write "+
			"span is no longer serialized and a write was silently overwritten — see appendOnlyWriteLocks "+
			"in pkg/memory/appendonlylock.go",
		writers, len(winners), winners)

	winner := winners[0]
	got, found, err := mem.Get(context.Background(), scope, kindName, contested)
	require.NoError(t, err, "reading the contested entry back")
	require.True(t, found, "the winning write must be durable")
	assert.JSONEq(t, fmt.Sprintf(`{"writer":%d}`, winner), string(got.Content),
		"the stored value must be the winner's own, not a loser's overwrite")
	assert.JSONEq(t, fmt.Sprintf(`{"writer":%d}`, winner), string(returned[winner].Content),
		"the winner must be handed back the entry it wrote")
}

// TestPutAppendOnlyConcurrentDistinctKeysAllSucceed and its sibling below are
// guards against OVER-locking, not proofs of the race: both pass with the lock
// removed, and are here so that a future coarser key (locking the scope, say)
// or a lock that mistakes a retry for a conflict fails loudly.
//
// Distinct ids share the striped mutex array — 32 keys over 256 stripes collide
// sometimes — so a collision must cost waiting and never a spurious conflict.
func TestPutAppendOnlyConcurrentDistinctKeysAllSucceed(t *testing.T) {
	t.Cleanup(memory.ResetRegistryForTest)
	memory.RegisterKind(appendOnlyKind{name: "ao-race", prefix: "aor-"})
	mem := memory.NewLocal(inmem.NewBackend())

	const writers = 32
	scope := memory.Scope{Kind: "session", ID: "nsRace/sessDistinct"}
	errs := make([]error, writers)

	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e := memory.Entry{
				Scope:     scope,
				Kind:      "ao-race",
				ID:        fmt.Sprintf("aor-%d", i),
				CreatedAt: time.Unix(0, 0).UTC(),
				Content:   json.RawMessage(fmt.Sprintf(`{"writer":%d}`, i)),
			}
			<-start
			_, errs[i] = mem.Put(context.Background(), e)
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		assert.NoErrorf(t, err, "writer %d wrote its own entry id and must not be refused", i)
	}
}

// TestPutAppendOnlyConcurrentIdenticalWritersAllSucceed pins that the lock does
// not turn the idempotent re-put into a conflict. Concurrent retries of one
// signed write are ordinary in production — the memory HTTP client retries 5xx
// — and every one of them must still be handed the stored entry back.
func TestPutAppendOnlyConcurrentIdenticalWritersAllSucceed(t *testing.T) {
	t.Cleanup(memory.ResetRegistryForTest)
	memory.RegisterKind(appendOnlyKind{name: "ao-race", prefix: "aor-"})
	mem := memory.NewLocal(inmem.NewBackend())

	const writers = 32
	e := memory.Entry{
		Scope:     memory.Scope{Kind: "session", ID: "nsRace/sessIdentical"},
		Kind:      "ao-race",
		ID:        "aor-identical",
		CreatedAt: time.Unix(0, 0).UTC(),
		Content:   json.RawMessage(`{"v":1}`),
	}
	errs := make([]error, writers)

	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = mem.Put(context.Background(), e)
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		assert.NoErrorf(t, err, "writer %d re-put byte-identical content; that is idempotent, not a conflict", i)
	}
}

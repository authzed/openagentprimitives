package lifecycle

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	lc "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// IncrementalLookback is how far BEHIND its own watermark an incremental read
// must reach. memory.Query's only tail predicate (Since) filters on createdAt,
// stamped from the clock of whichever publisher appended the entry — and this
// log has two publishers on independent clocks: the runner (session:<ns/name>,
// live region) and the operator (system:operator, pre/post regions). Under a
// bare high-water mark, an entry the OTHER publisher stamped slightly earlier
// falls below the bound and is silently invisible forever. That is the exact
// skew OrderKey exists to defeat, and also why a cache that seeds the log once
// and then only appends its own writes is wrong for this kind.
//
// Re-reading a window and deduping by EntryID bounds the problem: an entry is
// missed only if its publisher's clock trails the watermark by more than this
// window. Five minutes far exceeds any NTP-synced node skew a cluster tolerates.
//
// NOT a tuning knob: shrinking it trades a silent, permanent fold error for a
// little bandwidth. Callers needing a guaranteed-complete read use ReadOrdered.
const IncrementalLookback = 5 * time.Minute

// FullReadEvery is how many tail reads an IncrementalReader performs before it
// re-reads the WHOLE log once. It is what makes a tail read safe on a
// correctness path: no finite lookback window suffices, because Since filters on
// a wall clock each publisher stamps itself. A publisher running far AHEAD drags
// the watermark above entries another is about to write; one running far BEHIND
// lands its first entry under the bound, where it stays invisible and so can
// never raise its own watermark either. memory.Query offers no monotonic cursor.
//
// A periodic full re-read closes both: MergeOrdered is idempotent by EntryID, so
// a full read simply repairs whatever the tail reads missed, and logs what it
// recovered. At 64 the per-event fold is ~O(E²/64) plus the tails.
const FullReadEvery = 64

// MergeOrdered merges a freshly-read tail into an already-held ordered log and
// returns the combined log in fold order. Entries present in both — the lookback
// window always re-delivers some — are deduped by EntryID, keeping the PRIOR
// copy, since both decode from the same immutable append-only entry. Result
// ordering is the same total order ReadOrdered produces (see less), so merging is
// transparent to Fold. Neither input is mutated; the result is a fresh slice.
func MergeOrdered(prior, fresh []OrderedEvent) []OrderedEvent {
	if len(fresh) == 0 {
		out := make([]OrderedEvent, len(prior))
		copy(out, prior)
		return out
	}
	seen := make(map[string]struct{}, len(prior)+len(fresh))
	ds := make([]decoded, 0, len(prior)+len(fresh))
	add := func(evs []OrderedEvent) {
		for i := range evs {
			e := evs[i]
			// A legacy/synthetic event with no EntryID cannot be deduped; keep
			// it rather than collapsing every such event into one, and let the
			// sort place it. Real reads always carry an ID.
			if e.EntryID != "" {
				if _, dup := seen[e.EntryID]; dup {
					continue
				}
				seen[e.EntryID] = struct{}{}
			}
			ds = append(ds, decoded{id: e.EntryID, event: e.Event, key: e.Key, at: e.CreatedAt})
		}
	}
	add(prior)
	add(fresh)
	sort.SliceStable(ds, func(i, j int) bool { return less(ds[i], ds[j]) })
	out := make([]OrderedEvent, len(ds))
	for i := range ds {
		out[i] = OrderedEvent{EntryID: ds[i].id, Event: ds[i].event, Key: ds[i].key, CreatedAt: ds[i].at}
	}
	return out
}

// NextSince returns the Since bound the next incremental read should use, given
// every event seen so far: the newest createdAt minus IncrementalLookback. A
// zero time (which ReadOrderedSince treats as "read everything") is returned
// when nothing has been seen yet, so the first read is a full read.
func NextSince(seen []OrderedEvent) time.Time {
	var newest time.Time
	for i := range seen {
		if seen[i].CreatedAt.After(newest) {
			newest = seen[i].CreatedAt
		}
	}
	if newest.IsZero() {
		return time.Time{}
	}
	return newest.Add(-IncrementalLookback)
}

// IncrementalReader folds a session's lifecycle log without re-reading it.
//
// The problem: the runner's sequencer reads the whole transition log to fold it,
// then appends one event, on EVERY event it emits. The kind is append-only with
// no cap and the scope survives resume, so that is O(E²) bytes over HTTP and
// O(E²) decodes across a session, all inside the sequencer's serializing mutex.
//
// The reader holds the log it has already decoded and asks the backend only for
// the tail (a WINDOW, not a bare high-water mark — see IncrementalLookback),
// then merges by EntryID. Nothing is ever seeded once and trusted: every Read
// re-queries, so events the operator appends while the runner is live are picked
// up, which is what makes it safe against the two-publisher hazard.
//
// A tail read alone is still not sufficient — a clock far enough out of step in
// EITHER direction hides an entry from every subsequent tail read — so every
// FullReadEvery-th read is a full read, which repairs the held log and logs what
// it recovered. The residual exposure is bounded, and never silent.
//
// The zero value is ready to use. Reads are serialized on an internal mutex: the
// merge must see the same base the read started from.
type IncrementalReader struct {
	mu   sync.Mutex
	seen []OrderedEvent
	// tailReads counts reads since the last full read; see FullReadEvery.
	tailReads int
}

// Read returns the scope's full transition log in fold order, fetching only the
// tail it does not already hold. The returned slice is the reader's own backing
// array and MUST NOT be mutated by the caller; treat it as read-only input to
// Fold.
//
// On a query error nothing is retained or discarded: the reader's state is left
// exactly as it was, so the next Read retries the same tail rather than silently
// folding an incomplete log. A backend that does not honor Query.Since degrades
// safely — it answers with MORE entries than the bound asked for, which merges to
// the same log.
func (r *IncrementalReader) Read(ctx context.Context, m memory.Memory, scope memory.Scope) ([]OrderedEvent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	since := NextSince(r.seen)
	if r.tailReads >= FullReadEvery {
		since = time.Time{} // periodic full re-read; see FullReadEvery
	}
	fresh, err := ReadOrderedSince(ctx, m, scope, since)
	if err != nil {
		return nil, err
	}
	if since.IsZero() {
		r.reportRecovered(fresh, scope)
		r.tailReads = 0
	} else {
		r.tailReads++
	}
	r.seen = MergeOrdered(r.seen, fresh)
	return r.seen, nil
}

// reportRecovered logs, loudly, any entry a full read returned that the reader
// did NOT already hold AND that sat below the watermark a tail read would have
// used. Those are exactly the entries no tail read could ever have seen, so each
// is direct evidence of a publisher clock far enough out of step to have been
// silently corrupting the fold. Entries above the watermark are ordinary new
// appends and are not reported. Called under r.mu, before r.seen is replaced.
func (r *IncrementalReader) reportRecovered(fresh []OrderedEvent, scope memory.Scope) {
	if len(r.seen) == 0 {
		return // first read: everything is new by definition
	}
	bound := NextSince(r.seen)
	held := make(map[string]struct{}, len(r.seen))
	for i := range r.seen {
		held[r.seen[i].EntryID] = struct{}{}
	}
	var recovered, oldest string
	var n int
	for i := range fresh {
		e := fresh[i]
		if !e.CreatedAt.Before(bound) {
			continue
		}
		if _, ok := held[e.EntryID]; ok {
			continue
		}
		n++
		if recovered == "" {
			recovered = e.EntryID
			oldest = e.CreatedAt.Format(time.RFC3339Nano)
		}
	}
	if n == 0 {
		return
	}
	slog.Default().Info("lifecycle: periodic full re-read recovered transition events a tail read could not see; a publisher's clock is outside the lookback window",
		"scope", scope.ID, "recoveredCount", n, "firstRecoveredEntry", recovered,
		"createdAt", oldest, "watermark", bound.Format(time.RFC3339Nano),
		"lookback", IncrementalLookback.String())
}

// Events is Read reduced to the bare event stream, matching the signature of
// the package-level Events accessor so a per-emit fold can adopt the reader by
// swapping one call.
func (r *IncrementalReader) Events(ctx context.Context, m memory.Memory, scope memory.Scope) ([]lc.Event, error) {
	ordered, err := r.Read(ctx, m, scope)
	if err != nil {
		return nil, err
	}
	return justEvents(ordered), nil
}

// EventsForIncarnation is Events narrowed to the AgentSession instance
// sessionUID, matching the signature of the package-level accessor of the same
// name. The reader still HOLDS every event it has read — the filter applies to
// what it hands out, so a scope shared with a previous instance costs one pass
// per fold and nothing else. See ForIncarnation.
func (r *IncrementalReader) EventsForIncarnation(ctx context.Context, m memory.Memory, scope memory.Scope, sessionUID string) ([]lc.Event, error) {
	ordered, err := r.Read(ctx, m, scope)
	if err != nil {
		return nil, err
	}
	return justEvents(ForIncarnation(ordered, sessionUID)), nil
}

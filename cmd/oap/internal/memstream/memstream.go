// Package memstream polls the operator's memory HTTP API and emits new
// entries as they arrive. Stream/Fetch serve the turn-Kind transcript used
// by oap agent run / oap session show; StreamEntries serves raw entries of
// any Kind (e.g. tool_session follow-live).
package memstream

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
)

type Streamer struct {
	baseURL  string
	ns       string
	name     string
	token    string
	interval time.Duration
	http     *http.Client
}

func New(baseURL, namespace, name, token string, interval time.Duration) *Streamer {
	return &Streamer{
		baseURL:  baseURL,
		ns:       namespace,
		name:     name,
		token:    token,
		interval: interval,
		http:     &http.Client{Timeout: 10 * time.Second},
	}
}

const maxConsecutiveErrors = 5

// skipUndecodable reports an entry this streamer could not decode and chose to
// SKIP, degrading the transcript rather than failing the whole read.
//
// The situation is specific to append-only Kinds and is permanent. turn is BOTH
// append-only and session-written, and Memory.Put validates the Kind, the write
// authority and the ID PREFIX — nothing about the entry's SHAPE. Since
// turn.EntryToTurn Sscanfs the ID, an entry "turn-x" with EMPTY content is
// enough. Nothing can then remove it: per-entry Delete is refused
// unconditionally for an append-only Kind, and DeleteScope skips it. So the
// choice is between degrading one record and losing the scope forever, and the
// transcript is a RECORD rather than an input to a gate — skip.
//
// Reported, never swallowed (AGENTS.md): a skipped turn is a GAP in a
// tamper-evident log, and naming the writer is the only remedy left once the row
// cannot be deleted. cmd/oap installs no slog handler, so the default one carries
// these lines to the user's stderr.
//
// This duplicates pkg/memory/kinds/internal/undecodable.Skipped, deliberately and
// unhappily: that package is under an internal/ directory rooted at
// pkg/memory/kinds, so nothing in cmd/ can import it. Message text, field names
// and the scope-ID form are kept identical to it so one grep finds every
// disposition of an undecodable entry, wherever it was taken. Promoting that
// package out of internal/ should delete this.
func (s *Streamer) skipUndecodable(label string, e memory.Entry, err error) {
	publisher := "(unsigned)"
	if e.Provenance != nil {
		publisher = e.Provenance.Publisher
	}
	slog.Info(label+": skipping an undecodable entry — the read degrades rather than halting",
		"scope", s.ns+"/"+s.name, "entry", e.ID, "publisher", publisher, "err", err.Error())
}

// Stream polls /memory/turn/<ns>/<name> at the configured interval and emits
// each newly-appeared Turn on the returned channel. Turns are emitted in
// CreatedAt order (the order fetchEntries returns) — for a sequential
// single-writer transcript this equals (Index, Role) order; only Fetch
// re-sorts to (Index, Role) explicitly. The channel closes when ctx is
// canceled or an unrecoverable error occurs (reported on errCh).
func (s *Streamer) Stream(ctx context.Context) (<-chan memory.Turn, <-chan error) {
	entryCh, srcErrCh := s.streamEntries(ctx, func(c context.Context) ([]memory.Entry, error) {
		return s.fetchEntries(c, turn.KindName)
	})
	out := make(chan memory.Turn)
	errCh := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errCh)
		for e := range entryCh {
			tn, cerr := turn.EntryToTurn(e)
			if cerr != nil {
				// Skipped, not fatal — the same disposition turn.ReadAll takes
				// over these rows in-process. Halting here did not merely abandon
				// one command: this converter closed `out`, so oap agent run saw
				// its turn channel close and jumped to its terminal block, going
				// DARK for the rest of a run the agent was still working on. The
				// entry is undeletable (append-only), so re-running re-poisoned at
				// the same point forever.
				s.skipUndecodable("memstream.Stream", e, cerr)
				continue
			}
			select {
			case out <- tn:
			case <-ctx.Done():
				return
			}
		}
		// entryCh closed: forward any terminal error from the generic core.
		if err := <-srcErrCh; err != nil {
			errCh <- err
		}
	}()
	return out, errCh
}

// StreamEntries polls /memory/<kind>/<ns>/<name> at the configured interval
// and emits each newly-appeared Entry, in ascending CreatedAt order. It is
// the Kind-agnostic counterpart to Stream — the tool_session follow-live
// path uses it.
func (s *Streamer) StreamEntries(ctx context.Context, kind string) (<-chan memory.Entry, <-chan error) {
	return s.streamEntries(ctx, func(c context.Context) ([]memory.Entry, error) {
		return s.fetchEntries(c, kind)
	})
}

// Fetch performs a one-shot read of the session's full transcript. Useful
// for commands that don't want to stream (e.g. oap session operations).
func (s *Streamer) Fetch(ctx context.Context) ([]memory.Turn, error) {
	entries, err := s.fetchEntries(ctx, turn.KindName)
	if err != nil {
		return nil, err
	}
	out := make([]memory.Turn, 0, len(entries))
	for _, e := range entries {
		tn, cerr := turn.EntryToTurn(e)
		if cerr != nil {
			// Skipped, not fatal — see Stream. Erroring made the session's whole
			// transcript permanently unreadable by oap session logs / oap session
			// operations, with no operator remedy, since the row cannot be deleted.
			s.skipUndecodable("memstream.Fetch", e, cerr)
			continue
		}
		out = append(out, tn)
	}
	// fetchEntries sorts by CreatedAt; the turn contract (turn.Appender.ReadAll)
	// is (Index, Role) — re-sort so Stream's seen-count diffing and CLI
	// rendering see a stable transcript order.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Index != out[j].Index {
			return out[i].Index < out[j].Index
		}
		return out[i].Role < out[j].Role
	})
	return out, nil
}

// streamEntries is the shared poll/diff/backoff loop. It polls fetchFn at the
// configured interval, emits each entry that appeared since the last poll
// (seen-count diff — fetchFn must return a stable ascending order), and gives
// up after maxConsecutiveErrors consecutive failures.
func (s *Streamer) streamEntries(ctx context.Context, fetchFn func(context.Context) ([]memory.Entry, error)) (<-chan memory.Entry, <-chan error) {
	out := make(chan memory.Entry)
	errCh := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errCh)
		seen := 0
		consecutiveErrors := 0
		for {
			entries, err := fetchFn(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				consecutiveErrors++
				if consecutiveErrors >= maxConsecutiveErrors {
					errCh <- fmt.Errorf("memstream: %d consecutive errors; giving up: %w", consecutiveErrors, err)
					return
				}
				// Transient: back off and retry on the next interval.
				select {
				case <-ctx.Done():
					return
				case <-time.After(s.interval):
				}
				continue
			}
			consecutiveErrors = 0
			for _, e := range entries[seen:] {
				select {
				case out <- e:
				case <-ctx.Done():
					return
				}
			}
			if len(entries) > seen {
				seen = len(entries)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(s.interval):
			}
		}
	}()
	return out, errCh
}

// fetchEntries reads the scope's full set of entries for one Kind over the
// unified Kind-parameterized memory API. GET /memory/<kind>/<ns>/<name>
// returns a memory.QueryResult; the HTTP listing has no guaranteed order, so
// the result is sorted client-side into a deterministic total order —
// ascending by CreatedAt, then by Entry.ID — which streamEntries'
// seen-count diffing depends on (a non-total order over map-randomized
// input could reorder equal-CreatedAt entries between polls and drop one).
func (s *Streamer) fetchEntries(ctx context.Context, kind string) ([]memory.Entry, error) {
	url := fmt.Sprintf("%s/memory/%s/%s/%s", s.baseURL, kind, s.ns, s.name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("memstream: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("memstream: %s status %d", url, resp.StatusCode)
	}
	var res memory.QueryResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
	}
	entries := res.Entries
	sort.Slice(entries, func(i, j int) bool {
		if !entries[i].CreatedAt.Equal(entries[j].CreatedAt) {
			return entries[i].CreatedAt.Before(entries[j].CreatedAt)
		}
		// Tiebreaker: equal CreatedAt (events recorded in the same
		// instant — a burst from one parse chunk) must still sort
		// deterministically across polls, or streamEntries' seen-count
		// diff drops or re-emits entries. Entry IDs are unique.
		return entries[i].ID < entries[j].ID
	})
	return entries, nil
}

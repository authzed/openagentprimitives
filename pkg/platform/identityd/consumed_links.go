package identityd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// consumedLinkStore enforces single-use semantics on signed deep-links:
// markConsumed returns true on the first call for a given link, false on every
// subsequent call within ttl. It is the ONLY thing enforcing single-use — if
// Mallory forwards Alice's portal link, only one of them gets the cookie.
//
// Its state must therefore outlive the process: identityd restarts on every
// deploy, eviction and OOM, and a purely in-process marker silently re-arms
// every already-consumed link for the remainder of its 30-minute signed
// validity. Single-replica does not help — one replica still restarts. Hence
// the repo's cache-in-front-of-a-durable-record shape: the cache answers repeat
// hits, the backend is the authority on whether THIS call was the first.
type consumedLinkStore struct {
	mu      sync.Mutex
	entries map[string]time.Time // link digest → consumed-at (process-local cache)
	ttl     time.Duration
	// backend is the durable record. Nil is a supported degraded mode (unit
	// tests, a dev server with no cluster): single-use then holds only within
	// this process.
	backend consumedLinkBackend
}

// consumedLinkBackend is the durable half. markConsumed must be atomic —
// record key if absent, report whether THIS call recorded it — so two callers
// racing the same link cannot both be told they were first. It is handed a
// digest, never a raw link; see linkDigest.
type consumedLinkBackend interface {
	markConsumed(ctx context.Context, key string, at time.Time, ttl time.Duration) (bool, error)
}

func newConsumedLinkStore(ttl time.Duration, backend consumedLinkBackend) *consumedLinkStore {
	return &consumedLinkStore{entries: map[string]time.Time{}, ttl: ttl, backend: backend}
}

// linkDigest reduces a raw signed link to an opaque, fixed-length key.
//
// Hashing is load-bearing: the raw link is bearer-shaped — anyone holding it
// can bootstrap the cookie — so persisting it verbatim would turn the durable
// record into a credential store readable by every reader of that object. A
// digest suffices for the equality check, and its hex form is a valid key.
func linkDigest(linkRaw string) string {
	sum := sha256.Sum256([]byte(linkRaw))
	return hex.EncodeToString(sum[:])
}

// markConsumed records the link as consumed if it isn't already, returning true
// only if THIS call was the first to consume it. Concurrent-safe.
//
// Fail-closed: a backend failure returns (false, err), never (true, nil).
// Reporting a storage outage as a successful first use is exactly the replay
// this store prevents; the caller surfaces a retryable "couldn't verify this
// link" rather than the misleading "already used".
func (s *consumedLinkStore) markConsumed(ctx context.Context, linkRaw string) (bool, error) {
	key := linkDigest(linkRaw)
	now := time.Now()

	// Cache fast path: a link this process already saw consumed is settled and
	// needs no round trip. A miss is NOT authoritative — the backend decides.
	s.mu.Lock()
	s.gcLocked(now)
	_, cached := s.entries[key]
	s.mu.Unlock()
	if cached {
		return false, nil
	}

	first := true
	if s.backend != nil {
		var err error
		first, err = s.backend.markConsumed(ctx, key, now, s.ttl)
		if err != nil {
			return false, fmt.Errorf("consumed-link store: %w", err)
		}
	}

	// Cache the outcome either way: a link the backend reports as already
	// consumed is just as settled as one this process consumed itself.
	s.mu.Lock()
	s.entries[key] = now
	s.mu.Unlock()
	return first, nil
}

// gcLocked drops entries at or past ttl. A non-positive ttl expires everything
// immediately.
func (s *consumedLinkStore) gcLocked(now time.Time) {
	cutoff := now.Add(-s.ttl)
	for k, t := range s.entries {
		if !t.After(cutoff) {
			delete(s.entries, k)
		}
	}
}

// Package backoff is the shared per-key exponential-backoff tracker used by
// controllers (agentidentity, useridentity), which run the identical
// JIT-refresh loop over different identity kinds.
package backoff

import (
	"sync"
	"time"
)

// BaseDelay is the first-failure delay; subsequent failures double it up to
// the configured cap.
const BaseDelay = 30 * time.Second

// backoffEntry tracks the failure count per (identity, credential), plus when
// the last failure happened. The timestamp is what makes the delay enforceable
// on the ATTEMPT: a reconcile can arrive from any trigger (a sibling
// credential's Secret write re-firing the Secret watch, a spec edit, a
// resync), so "how long since this key last failed" is the only question a
// caller can ask that does not depend on its own requeue having been honoured.
type backoffEntry struct {
	failures    int
	lastFailure time.Time
}

// Backoff is a goroutine-safe per-key exponential-backoff tracker. Key
// format is the caller's responsibility (the reconcilers use
// "namespace/identityName/credentialName").
//
// Entries live only in memory; on operator-pod restart or leader handoff the
// next failure starts from BaseDelay again. The reconcilers accept this as
// the failure-mode trade-off (see the design spec, "Status-write interaction").
type Backoff struct {
	mu  sync.Mutex
	e   map[string]*backoffEntry
	cap time.Duration
	now func() time.Time
}

// NewBackoff returns a tracker that caps the per-key delay at cap.
func New(cap time.Duration) *Backoff {
	return &Backoff{e: map[string]*backoffEntry{}, cap: cap, now: time.Now}
}

// SetClock replaces the tracker's clock. Test seam only — production code
// never calls it, and New installs time.Now. It exists because the delays this
// package hands out are tens of seconds to minutes: without it, a test that
// wanted to observe a retry AFTER a backoff elapsed would have to sleep for
// it.
func (b *Backoff) SetClock(now func() time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.now = now
}

// RecordFailure increments the key's failure count, stamps the failure time,
// and returns the next delay to wait before retrying.
func (b *Backoff) RecordFailure(key string) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	ent, ok := b.e[key]
	if !ok {
		ent = &backoffEntry{}
		b.e[key] = ent
	}
	ent.failures++
	ent.lastFailure = b.now()
	return b.delayFor(ent.failures)
}

// RecordSuccess clears the key's failure count.
func (b *Backoff) RecordSuccess(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.e, key)
}

// NextDelay returns the delay that would apply on the next reconcile if the
// key is in a failing state, or 0 if no entry exists. It is the FULL delay for
// the current failure count, ignoring how much of it has already elapsed —
// callers deciding whether to act want Remaining, not this.
func (b *Backoff) NextDelay(key string) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	ent, ok := b.e[key]
	if !ok {
		return 0
	}
	return b.delayFor(ent.failures)
}

// Remaining reports how much of the key's current backoff is left: 0 when the
// key has no entry (never failed, or a success cleared it) or when the delay
// has fully elapsed, meaning the caller may retry now.
//
// This is the enforcement primitive. A caller that gates its retry on
// Remaining honours the backoff no matter what woke it up, which a caller that
// only sets its own retry deadline cannot: something else can always wake it
// sooner.
func (b *Backoff) Remaining(key string) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	ent, ok := b.e[key]
	if !ok {
		return 0
	}
	left := b.delayFor(ent.failures) - b.now().Sub(ent.lastFailure)
	if left < 0 {
		return 0
	}
	return left
}

// delayFor returns BaseDelay * 2^(failures-1), capped at b.cap. Caller must
// hold b.mu.
func (b *Backoff) delayFor(failures int) time.Duration {
	if failures <= 0 {
		return 0
	}
	d := BaseDelay
	for i := 1; i < failures; i++ {
		d *= 2
		if d >= b.cap {
			return b.cap
		}
	}
	return d
}

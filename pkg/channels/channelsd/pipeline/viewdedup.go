package pipeline

import (
	"sync"
	"time"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// viewDedupTTL bounds how long a delivered view_message result is remembered
// for idempotency: long enough to cover a client's Retry after a reply-timeout,
// short enough to bound memory (and to let a genuinely-new message reusing a
// stale id through, which clients never do — ids are per-send UUIDs).
const viewDedupTTL = 5 * time.Minute

// viewDedup remembers recent successful view_message results keyed by the
// client's idempotency id, so a retried send whose first reply was lost to a
// transient timeout returns the SAME decision instead of re-delivering the
// message to the agent. Concurrency-safe; a retry racing the original's
// in-flight delivery is not deduped here (both deliver) — the common case is a
// Retry fired only AFTER the first send's reply timed out, by which point the
// first delivery has completed and cached its result.
type viewDedup struct {
	mu  sync.Mutex
	now func() time.Time
	m   map[string]viewDedupEntry
}

type viewDedupEntry struct {
	res channelevents.ViewMessageResultPayload
	at  time.Time
}

func newViewDedup(now func() time.Time) *viewDedup {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &viewDedup{now: now, m: map[string]viewDedupEntry{}}
}

// get returns the cached result for id when present and unexpired, evicting it
// lazily otherwise. An empty id (dedup disabled) always misses.
func (d *viewDedup) get(id string) (channelevents.ViewMessageResultPayload, bool) {
	if id == "" {
		return channelevents.ViewMessageResultPayload{}, false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.m[id]
	if !ok {
		return channelevents.ViewMessageResultPayload{}, false
	}
	if d.now().Sub(e.at) > viewDedupTTL {
		delete(d.m, id)
		return channelevents.ViewMessageResultPayload{}, false
	}
	return e.res, true
}

// put remembers a delivered result for id (no-op for an empty id). It sweeps
// expired entries first so the map stays bounded to the live TTL window.
func (d *viewDedup) put(id string, res channelevents.ViewMessageResultPayload) {
	if id == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	cutoff := d.now()
	for k, e := range d.m {
		if cutoff.Sub(e.at) > viewDedupTTL {
			delete(d.m, k)
		}
	}
	d.m[id] = viewDedupEntry{res: res, at: d.now()}
}

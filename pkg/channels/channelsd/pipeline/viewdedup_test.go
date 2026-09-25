package pipeline

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

func TestViewDedup(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	d := newViewDedup(clock)

	res := channelevents.ViewMessageResultPayload{Outcome: "routed", RequesterCanonicalID: "user:a"}

	// Miss before put; empty id always misses (dedup disabled).
	_, ok := d.get("req-1")
	assert.False(t, ok, "unknown id misses")
	_, ok = d.get("")
	assert.False(t, ok, "empty id never dedups")
	d.put("", res) // no-op

	// Put then get returns the cached result (the idempotent replay).
	d.put("req-1", res)
	got, ok := d.get("req-1")
	assert.True(t, ok, "a retried id returns the cached result")
	assert.Equal(t, res, got)

	// Distinct ids are independent.
	_, ok = d.get("req-2")
	assert.False(t, ok)

	// Past the TTL the entry expires and misses (so a re-delivery would happen).
	now = now.Add(viewDedupTTL + time.Second)
	_, ok = d.get("req-1")
	assert.False(t, ok, "an entry older than the TTL is evicted and misses")
}

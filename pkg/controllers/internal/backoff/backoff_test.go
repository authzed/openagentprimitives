package backoff

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestBackoff_DelayProgression collapses what were originally three
// separate tests (FirstFailure / GrowsExponentially / CapsAtMax) into
// one table-driven sweep. Each case drives a fresh backoff up to the
// target failure count and asserts the returned delay matches the
// expected schedule (30s, 60s, 120s, 240s, then capped at 5m).
func TestBackoff_DelayProgression(t *testing.T) {
	cases := []struct {
		name     string
		failures int
		want     time.Duration
	}{
		{"first failure → base delay", 1, 30 * time.Second},
		{"second failure → 2× base", 2, 60 * time.Second},
		{"third failure → 4× base", 3, 120 * time.Second},
		{"fourth failure → 8× base", 4, 240 * time.Second},
		{"fifth failure → capped (16× base would be 8m)", 5, 5 * time.Minute},
		{"sixth failure → stays at cap", 6, 5 * time.Minute},
		{"tenth failure → stays at cap", 10, 5 * time.Minute},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := New(5 * time.Minute)
			var got time.Duration
			for i := 0; i < tc.failures; i++ {
				got = b.RecordFailure("k")
			}
			assert.Equal(t, tc.want, got, "RecordFailure result")
			assert.Equal(t, tc.want, b.NextDelay("k"),
				"NextDelay should equal the last RecordFailure result without bumping the counter")
		})
	}
}

func TestBackoff_SuccessClearsEntry(t *testing.T) {
	b := New(5 * time.Minute)
	b.RecordFailure("k")
	b.RecordFailure("k")
	b.RecordSuccess("k")

	assert.Equal(t, time.Duration(0), b.NextDelay("k"),
		"NextDelay after success should be 0")
	assert.Equal(t, 30*time.Second, b.RecordFailure("k"),
		"post-success first failure should restart at base delay")
}

func TestBackoff_NextDelayWithoutEntry(t *testing.T) {
	b := New(5 * time.Minute)
	assert.Equal(t, time.Duration(0), b.NextDelay("never-seen"),
		"NextDelay for absent key should be 0")
}

// TestBackoff_Remaining pins the reader callers gate their retry on. Unlike
// NextDelay it accounts for elapsed time, which is what lets a caller honour
// the delay no matter what woke it up.
func TestBackoff_Remaining(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(b *Backoff, at *time.Time)
		want    time.Duration
	}{
		{
			name:    "never failed → 0, retry allowed now",
			prepare: func(b *Backoff, at *time.Time) {},
			want:    0,
		},
		{
			name:    "failed just now → the full base delay is left",
			prepare: func(b *Backoff, at *time.Time) { b.RecordFailure("k") },
			want:    30 * time.Second,
		},
		{
			name: "10s into a 30s delay → 20s left",
			prepare: func(b *Backoff, at *time.Time) {
				b.RecordFailure("k")
				*at = at.Add(10 * time.Second)
			},
			want: 20 * time.Second,
		},
		{
			name: "delay fully elapsed → 0, retry allowed now",
			prepare: func(b *Backoff, at *time.Time) {
				b.RecordFailure("k")
				*at = at.Add(31 * time.Second)
			},
			want: 0,
		},
		{
			name: "long past the delay → 0, never negative",
			prepare: func(b *Backoff, at *time.Time) {
				b.RecordFailure("k")
				*at = at.Add(72 * time.Hour)
			},
			want: 0,
		},
		{
			name: "second failure restarts the clock at the doubled delay",
			prepare: func(b *Backoff, at *time.Time) {
				b.RecordFailure("k")
				*at = at.Add(30 * time.Second)
				b.RecordFailure("k")
			},
			want: 60 * time.Second,
		},
		{
			name: "success clears the entry → 0",
			prepare: func(b *Backoff, at *time.Time) {
				b.RecordFailure("k")
				b.RecordSuccess("k")
			},
			want: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			at := time.Now()
			b := New(5 * time.Minute)
			b.SetClock(func() time.Time { return at })
			tc.prepare(b, &at)
			assert.Equal(t, tc.want, b.Remaining("k"), "Remaining")
		})
	}
}

// TestBackoff_RemainingIsPerKey pins that one key's failure never gates
// another's retry — the reconcilers key per (identity, credential), and a
// revoked credential must not hold up its healthy siblings.
func TestBackoff_RemainingIsPerKey(t *testing.T) {
	b := New(5 * time.Minute)
	b.RecordFailure("ns/id/revoked")

	assert.Positive(t, b.Remaining("ns/id/revoked"), "the failing key is inside its backoff")
	assert.Equal(t, time.Duration(0), b.Remaining("ns/id/healthy"),
		"a sibling credential that never failed must be free to refresh")
}

func TestBackoff_ConcurrentAccessIsSafe(t *testing.T) {
	b := New(5 * time.Minute)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.RecordFailure("k1")
			b.NextDelay("k1")
			b.RecordSuccess("k1")
		}()
	}
	wg.Wait()
	// If we got here without -race firing, we're good.
}

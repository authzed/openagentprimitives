package sandbox

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestToolProgressTick(t *testing.T) {
	start := time.Unix(1000, 0)
	threshold := 3 * time.Second
	interval := 3 * time.Second
	cases := []struct {
		name        string
		now         time.Time
		lastEmit    time.Time
		wantEmit    bool
		wantElapsed int
	}{
		{"below threshold: no emit", start.Add(2 * time.Second), time.Time{}, false, 2},
		{"at threshold, never emitted: emit", start.Add(3 * time.Second), time.Time{}, true, 3},
		{"within interval since last emit: no emit", start.Add(5 * time.Second), start.Add(3 * time.Second), false, 5},
		{"interval elapsed since last emit: emit", start.Add(6 * time.Second), start.Add(3 * time.Second), true, 6},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			emit, elapsed := toolProgressTick(tc.now, start, tc.lastEmit, threshold, interval)
			assert.Equal(t, tc.wantEmit, emit)
			assert.Equal(t, tc.wantElapsed, int(elapsed.Seconds()))
		})
	}
}

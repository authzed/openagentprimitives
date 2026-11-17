package sessionschedule

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNextNominal(t *testing.T) {
	anchor := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		spec   Spec
		after  time.Time
		want   time.Time
		wantOK bool
	}{
		{"interval: the next slot on the anchor's phase", Spec{Kind: "interval", Timezone: "UTC", IntervalSeconds: 3600, MaxRuns: 3, RunWindowSeconds: 60}, anchor.Add(150 * time.Minute), anchor.Add(3 * time.Hour), true},
		{"interval: past the series' own MaxRuns, still nominal", Spec{Kind: "interval", Timezone: "UTC", IntervalSeconds: 3600, MaxRuns: 2, RunWindowSeconds: 60}, anchor.Add(10*time.Hour + time.Minute), anchor.Add(11 * time.Hour), true},
		{"daily: the next day at the anchor's local time", Spec{Kind: "daily", Timezone: "America/New_York", MaxRuns: 5, RunWindowSeconds: 60}, anchor, anchor.Add(24 * time.Hour), true},
		{"before the anchor: the anchor itself", Spec{Kind: "daily", Timezone: "UTC", MaxRuns: 5, RunWindowSeconds: 60}, anchor.Add(-time.Hour), anchor, true},
		{"once: no next slot", Spec{Kind: "once", Timezone: "UTC", MaxRuns: 1, RunWindowSeconds: 60}, anchor, time.Time{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok, err := NextNominal(tc.spec, anchor, tc.after)
			require.NoError(t, err)
			assert.Equal(t, tc.wantOK, ok)
			assert.True(t, tc.want.Equal(got), "got %s, want %s", got, tc.want)
		})
	}
}

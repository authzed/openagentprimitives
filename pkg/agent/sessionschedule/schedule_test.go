package sessionschedule

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func instant(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	require.NoError(t, err)
	return v
}
func TestResolveCalendarAndQuietHours(t *testing.T) {
	tests := []struct {
		name, start, end string
		spec             Spec
		want             []string
	}{
		{"spring gap skips", "2026-03-07T07:30:00Z", "2026-03-11T00:00:00Z", Spec{Kind: "daily", Timezone: "America/New_York", MaxRuns: 3, RunWindowSeconds: 60}, []string{"2026-03-07T07:30:00Z", "2026-03-09T06:30:00Z", "2026-03-10T06:30:00Z"}},
		{"fall fold runs once", "2026-10-31T05:30:00Z", "2026-11-04T00:00:00Z", Spec{Kind: "daily", Timezone: "America/New_York", MaxRuns: 3, RunWindowSeconds: 60}, []string{"2026-10-31T05:30:00Z", "2026-11-01T05:30:00Z", "2026-11-02T06:30:00Z"}},
		{"weekly local days", "2026-10-03T13:00:00Z", "2026-10-13T00:00:00Z", Spec{Kind: "weekly", Timezone: "America/New_York", Weekdays: []int{1, 3}, MaxRuns: 3, RunWindowSeconds: 60}, []string{"2026-10-05T13:00:00Z", "2026-10-07T13:00:00Z", "2026-10-12T13:00:00Z"}},
		{"overnight coalesces", "2026-10-04T02:00:00Z", "2026-10-05T00:00:00Z", Spec{Kind: "interval", Timezone: "America/New_York", IntervalSeconds: 3600, MaxRuns: 4, RunWindowSeconds: 60, QuietHours: []QuietHours{{Start: "22:00", End: "08:00"}}}, []string{"2026-10-04T12:00:00Z"}},
		{"starting weekday owns overnight", "2026-10-04T03:00:00Z", "2026-10-05T00:00:00Z", Spec{Kind: "interval", Timezone: "America/New_York", IntervalSeconds: 43200, MaxRuns: 2, RunWindowSeconds: 60, QuietHours: []QuietHours{{Start: "22:00", End: "08:00", Weekdays: []int{6}}}}, []string{"2026-10-04T12:00:00Z", "2026-10-04T15:00:00Z"}},
		{"quiet end skips missing hour", "2026-03-08T06:00:00Z", "2026-03-09T00:00:00Z", Spec{Kind: "once", Timezone: "America/New_York", MaxRuns: 1, RunWindowSeconds: 60, QuietHours: []QuietHours{{Start: "00:00", End: "02:30"}}}, []string{"2026-03-08T07:00:00Z"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			windows, err := Resolve(tt.spec, instant(t, tt.start), instant(t, tt.end))
			require.NoError(t, err)
			var got []string
			for _, w := range windows {
				got = append(got, w.DueAt.Format(time.RFC3339))
				assert.True(t, w.ExpiresAt.After(w.DueAt))
			}
			assert.Equal(t, tt.want, got)
		})
	}
}
func TestWindowEndsAtQuietHours(t *testing.T) {
	s := Spec{Kind: "once", Timezone: "UTC", MaxRuns: 1, RunWindowSeconds: 3600, QuietHours: []QuietHours{{Start: "22:00", End: "08:00"}}}
	windows, err := Resolve(s, instant(t, "2026-10-03T21:59:30Z"), instant(t, "2026-10-04T12:00:00Z"))
	require.NoError(t, err)
	require.Len(t, windows, 1)
	assert.Equal(t, instant(t, "2026-10-03T22:00:00Z"), windows[0].ExpiresAt)
}
func TestInvalidSchedulesFailClosed(t *testing.T) {
	base := Spec{Kind: "interval", Timezone: "UTC", MaxRuns: 3, IntervalSeconds: 60, RunWindowSeconds: 60}
	tests := []struct {
		name   string
		change func(*Spec)
	}{
		{"unknown policy", func(s *Spec) { s.Kind = "typo" }},
		{"host timezone", func(s *Spec) { s.Timezone = "Local" }},
		{"missing timezone", func(s *Spec) { s.Timezone = "" }},
		{"unknown timezone", func(s *Spec) { s.Timezone = "somewhere" }},
		{"unbounded", func(s *Spec) { s.MaxRuns = 0 }},
		{"too many runs", func(s *Spec) { s.MaxRuns = 101 }},
		{"interval too fast", func(s *Spec) { s.IntervalSeconds = 59 }},
		{"weekly missing days", func(s *Spec) { s.Kind = "weekly"; s.IntervalSeconds = 0 }},
		{"duplicate days", func(s *Spec) { s.Kind = "weekly"; s.IntervalSeconds = 0; s.Weekdays = []int{1, 1} }},
		{"equal quiet endpoints", func(s *Spec) { s.QuietHours = []QuietHours{{Start: "12:00", End: "12:00"}} }},
		{"invalid clock", func(s *Spec) { s.QuietHours = []QuietHours{{Start: "24:00", End: "08:00"}} }},
		{"invalid weekday", func(s *Spec) { s.QuietHours = []QuietHours{{Start: "22:00", End: "08:00", Weekdays: []int{7}}} }},
		{"full day quiet", func(s *Spec) {
			s.QuietHours = []QuietHours{{Start: "00:00", End: "12:00"}, {Start: "12:00", End: "00:00"}}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := base
			tt.change(&s)
			_, err := Resolve(s, instant(t, "2026-10-03T12:00:00Z"), instant(t, "2026-10-04T12:00:00Z"))
			require.Error(t, err)
		})
	}
	_, err := Resolve(base, instant(t, "2026-10-03T12:00:00Z"), instant(t, "2026-11-04T12:00:00Z"))
	require.Error(t, err)
}

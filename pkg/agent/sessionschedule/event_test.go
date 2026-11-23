package sessionschedule

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestEventWindowKeepsDeadlineAcrossQuietHours(t *testing.T) {
	for _, tc := range []struct {
		name, ready, deadline, due, expiry, timezone string
		quiet                                        []QuietHours
		ok                                           bool
	}{
		{name: "fractional quiet boundary", ready: "2026-10-04T00:00:59.999Z", deadline: "2026-10-04T00:10:59.999Z", due: "2026-10-04T00:05:00Z", expiry: "2026-10-04T00:10:59.999Z", timezone: "UTC", quiet: []QuietHours{{Start: "00:00", End: "00:05"}}, ok: true},
		{name: "deadline exclusive", ready: "2026-10-04T00:00:00Z", deadline: "2026-10-04T00:05:00Z", timezone: "UTC", quiet: []QuietHours{{Start: "00:00", End: "00:05"}}},
		{name: "next quiet caps run", ready: "2026-10-04T00:03:00Z", deadline: "2026-10-04T00:15:00Z", due: "2026-10-04T00:03:00Z", expiry: "2026-10-04T00:05:00Z", timezone: "UTC", quiet: []QuietHours{{Start: "00:05", End: "00:10"}}, ok: true},
		{name: "overnight weekday", ready: "2026-10-05T00:30:00Z", deadline: "2026-10-05T02:00:00Z", due: "2026-10-05T01:00:00Z", expiry: "2026-10-05T02:00:00Z", timezone: "UTC", quiet: []QuietHours{{Start: "23:00", End: "01:00", Weekdays: []int{0}}}, ok: true},
		{name: "fall back uses real deadline", ready: "2026-11-01T05:50:00Z", deadline: "2026-11-01T06:20:00Z", timezone: "America/New_York", quiet: []QuietHours{{Start: "01:00", End: "02:00"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parse := func(v string) time.Time {
				result, err := time.Parse(time.RFC3339Nano, v)
				require.NoError(t, err)
				return result
			}
			w, ok, err := EventWindow(tc.timezone, tc.quiet, parse(tc.ready), parse(tc.deadline))
			require.NoError(t, err)
			require.Equal(t, tc.ok, ok)
			if ok {
				require.Equal(t, parse(tc.due), w.DueAt)
				require.Equal(t, parse(tc.expiry), w.ExpiresAt)
			}
		})
	}
}

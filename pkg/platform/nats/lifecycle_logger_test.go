package nats

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The reconnect line's duration is what turns "channelsd flapped" into "these
// are the seconds in which replies were dropped".
func TestLifecycleLoggerReportsOutageDuration(t *testing.T) {
	base := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)

	cases := []struct {
		name       string
		outage     time.Duration
		disconnect bool
		wantOutage string
	}{
		{
			name:       "reconnect after a 45s gap: line carries 45s",
			outage:     45 * time.Second,
			disconnect: true,
			wantOutage: "45s",
		},
		{
			name:       "reconnect after a multi-minute gap: line carries the full duration",
			outage:     3*time.Minute + 20*time.Second,
			disconnect: true,
			wantOutage: "3m20s",
		},
		{
			name:       "reconnect with no recorded disconnect (initial connect retry): duration reported as unknown, never as zero",
			disconnect: false,
			wantOutage: outageUnknown,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logged []map[string]any
			now := base
			ll := &lifecycleLogger{
				name: "channelsd",
				now:  func() time.Time { return now },
				log: func(_ string, args ...any) {
					kv := map[string]any{}
					for i := 0; i+1 < len(args); i += 2 {
						kv[args[i].(string)] = args[i+1]
					}
					logged = append(logged, kv)
				},
			}

			if tc.disconnect {
				ll.onDisconnect(errors.New("connection reset"))
				now = base.Add(tc.outage)
			}
			ll.onReconnect()

			require.NotEmpty(t, logged)
			last := logged[len(logged)-1]
			assert.Equal(t, tc.wantOutage, last["outage"])
			assert.Equal(t, "channelsd", last["conn"])
		})
	}
}

// A second disconnect must re-arm the window rather than measure from the
// first, so a flapping connection reports each gap instead of one ever-growing
// number.
func TestLifecycleLoggerReArmsPerOutage(t *testing.T) {
	base := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	now := base
	var outages []any
	ll := &lifecycleLogger{
		name: "channelsd",
		now:  func() time.Time { return now },
		log: func(_ string, args ...any) {
			for i := 0; i+1 < len(args); i += 2 {
				if args[i] == "outage" {
					outages = append(outages, args[i+1])
				}
			}
		},
	}

	ll.onDisconnect(nil)
	now = now.Add(10 * time.Second)
	ll.onReconnect()

	now = now.Add(time.Hour) // healthy stretch: must not be counted as outage
	ll.onDisconnect(nil)
	now = now.Add(5 * time.Second)
	ll.onReconnect()

	// A clean shutdown an hour into healthy operation. Reporting the last
	// outage's start here would tell an operator the bus had been down for
	// 1h5m when it had been up the whole time — a worse lie than saying
	// nothing, because it invites a hunt for a gap that never existed.
	now = now.Add(time.Hour)
	ll.onClosed("none")

	assert.Equal(t, []any{"10s", "5s", outageUnknown}, outages,
		"each outage is measured from its own disconnect; a healthy connection has no open window to report")
}

package slack

import (
	"errors"
	"testing"
	"time"

	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	slackapi "github.com/slack-go/slack"
)

func TestDeliveryLag(t *testing.T) {
	// 100.5 is exactly representable in float64, so the arithmetic is exact.
	base := time.Unix(0, 100_500_000_000) // 100.5s since epoch
	cases := []struct {
		name    string
		eventTS string
		now     time.Time
		wantOK  bool
		wantMs  int64
	}{
		{name: "normal lag: 4.5s late, ok", eventTS: "100.5", now: base.Add(4500 * time.Millisecond), wantOK: true, wantMs: 4500},
		{name: "zero lag: received instantly, ok", eventTS: "100.5", now: base, wantOK: true, wantMs: 0},
		{name: "clock skew: event stamped in the future, negative but ok", eventTS: "100.5", now: base.Add(-2 * time.Second), wantOK: true, wantMs: -2000},
		{name: "empty ts: not ok", eventTS: "", now: base, wantOK: false},
		{name: "unparseable ts: not ok", eventTS: "not-a-ts", now: base, wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lag, ok := deliveryLag(tc.eventTS, tc.now)
			assert.Equal(t, tc.wantOK, ok)
			if tc.wantOK {
				assert.Equal(t, tc.wantMs, lag.Milliseconds())
			}
		})
	}
}

func TestDeliveryLag_RealSlackTS(t *testing.T) {
	// A real Slack ts (microsecond precision) still parses and yields a
	// plausible lag within float rounding tolerance.
	stamped := time.Unix(1783106899, 312089000)
	lag, ok := deliveryLag("1783106899.312089", stamped.Add(2500*time.Millisecond))
	require.True(t, ok)
	assert.InDelta(t, 2500, lag.Milliseconds(), 1)
}

func TestInboundEventTS(t *testing.T) {
	mk := func(data interface{}) slackevents.EventsAPIEvent {
		return slackevents.EventsAPIEvent{InnerEvent: slackevents.EventsAPIInnerEvent{Data: data}}
	}
	cases := []struct {
		name string
		api  slackevents.EventsAPIEvent
		want string
	}{
		{name: "app_mention: returns its ts", api: mk(&slackevents.AppMentionEvent{TimeStamp: "111.1"}), want: "111.1"},
		{name: "message: returns its ts", api: mk(&slackevents.MessageEvent{TimeStamp: "222.2"}), want: "222.2"},
		{name: "other inner event: empty", api: mk(&slackevents.AppHomeOpenedEvent{}), want: ""},
		{name: "nil inner data: empty", api: mk(nil), want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, inboundEventTS(tc.api))
		})
	}
}

func TestSocketErrorDetail(t *testing.T) {
	cases := []struct {
		name string
		data interface{}
		want string
	}{
		{name: "bad message: reports cause", data: &socketmode.ErrorBadMessage{Cause: errors.New("bad json")}, want: "bad json"},
		{name: "bad message: no cause", data: &socketmode.ErrorBadMessage{}, want: "bad message (no cause)"},
		{name: "write failed: reports cause", data: &socketmode.ErrorWriteFailed{Cause: errors.New("write eof")}, want: "write eof"},
		{name: "incoming error (implements error): reports its Error()", data: &slackapi.IncomingEventError{ErrorObj: errors.New("stream reset")}, want: "stream reset"},
		{name: "plain error: reports its Error()", data: errors.New("boom"), want: "boom"},
		{name: "nil data: sentinel", data: nil, want: "(no detail)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, socketErrorDetail(tc.data))
		})
	}
}

// pkg/channels/channelkinds/slack/inbound_observability.go
//
// Small, pure helpers that make the Slack inbound path observable:
//
//   - deliveryLag: how long a Slack event sat between Slack stamping it and
//     channelsd receiving it. This is the signal that distinguishes a slow
//     ack caused by Slack-side event delivery from one caused by our own
//     processing — the two are indistinguishable without it.
//   - inboundEventTS: pulls the event ts off the inner event so deliveryLag
//     has something to measure against.
//   - socketErrorDetail: extracts a human-readable cause from a socketmode
//     error event's Data payload so transport errors are logged, not dropped
//     (AGENTS.md: never silently drop errors).
package slack

import (
	"fmt"
	"strconv"
	"time"

	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

// deliveryLag reports now − eventTS, where eventTS is a Slack timestamp
// ("epoch.microseconds", e.g. "1783106899.312089"). The bool is false when
// eventTS is empty or unparseable, so callers can skip logging rather than
// emit a bogus number. The lag may be negative under clock skew; that is
// reported faithfully rather than clamped — a negative value is itself a
// useful signal that the channelsd node clock trails Slack's.
func deliveryLag(eventTS string, now time.Time) (time.Duration, bool) {
	if eventTS == "" {
		return 0, false
	}
	secs, err := strconv.ParseFloat(eventTS, 64)
	if err != nil {
		return 0, false
	}
	stamped := time.Unix(0, int64(secs*float64(time.Second)))
	return now.Sub(stamped), true
}

// inboundEventTS returns the Slack ts of an inbound EventsAPI event for the
// message-bearing inner events channelsd acts on (app_mention, message).
// Other inner event types (app_home_opened, assistant_thread_*, …) have no
// user-message ts worth measuring delivery lag against, so they return "".
func inboundEventTS(api slackevents.EventsAPIEvent) string {
	switch ev := api.InnerEvent.Data.(type) {
	case *slackevents.AppMentionEvent:
		return ev.TimeStamp
	case *slackevents.MessageEvent:
		return ev.TimeStamp
	default:
		return ""
	}
}

// socketErrorDetail extracts a human-readable cause from a socketmode error
// event's Data payload. The three transport-error event types carry different
// shapes: ErrorBadMessage / ErrorWriteFailed are *structs* with a Cause error
// field, while IncomingError delivers a *slack.IncomingEventError that itself
// implements error. The error case therefore comes AFTER the two struct cases
// so the more specific shapes win.
func socketErrorDetail(data interface{}) string {
	switch d := data.(type) {
	case *socketmode.ErrorBadMessage:
		if d != nil && d.Cause != nil {
			return d.Cause.Error()
		}
		return "bad message (no cause)"
	case *socketmode.ErrorWriteFailed:
		if d != nil && d.Cause != nil {
			return d.Cause.Error()
		}
		return "write failed (no cause)"
	case error:
		return d.Error()
	case nil:
		return "(no detail)"
	default:
		return fmt.Sprintf("%+v", d)
	}
}

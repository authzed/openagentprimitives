package channelevents

import "github.com/authzed/openagentprimitives/pkg/platform/nats/subjects"

// ChannelHistoryRequestSubject returns the per-session NATS subject the
// read_channel_history tool requests on. Kept distinct from the thread
// HistoryRequestSubject so the whole-channel path has its own responder and
// queue group.
func ChannelHistoryRequestSubject(natsSubjectPrefix string) string {
	return subjects.PrefixOf(natsSubjectPrefix).ChannelHistory()
}

// ChannelHistorySubscribeSubject is the wildcard the channel-history responder
// subscribes to. Derived from the same leaf name
// ChannelHistoryRequestSubject publishes on, so the two cannot drift.
const ChannelHistorySubscribeSubject = subjects.AnyChannelHistory

// ChannelHistoryQueueGroup ensures exactly one responder per request across a
// replicated channelsd.
const ChannelHistoryQueueGroup = "channelsd-channel-history"

// ParseChannelHistorySubject extracts ns + name from a channel-history request
// subject ("ap.session.<ns>.<name>.channel_history.request") — the inverse of
// ChannelHistoryRequestSubject.
func ParseChannelHistorySubject(subject string) (ns, name string, ok bool) {
	return subjects.ParseChannelHistory(subject)
}

// ChannelHistoryRequest is the read_channel_history request payload. The
// session is identified by the subject, not here; the channel is derived from
// the session server-side, never from this payload.
type ChannelHistoryRequest struct {
	// Limit caps returned messages; zero means the responder's own default.
	Limit int `json:"limit,omitempty"`
	// LookbackDays bounds how far back to read; zero means the responder's
	// own default window.
	LookbackDays int    `json:"lookbackDays,omitempty"`
	Since        string `json:"since,omitempty"`        // RFC3339; honored only when the kind supports date range
	Until        string `json:"until,omitempty"`        // RFC3339
	BeforeCursor string `json:"beforeCursor,omitempty"` // opaque pagination cursor
}

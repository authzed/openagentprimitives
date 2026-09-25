package channelevents

import "github.com/authzed/openagentprimitives/pkg/platform/nats/subjects"

// HistoryRequestSubject returns the per-session NATS subject the
// read_thread_history tool requests on. natsSubjectPrefix is the
// session's ChannelBinding.NATSSubjectPrefix ("ap.session.<ns>.<name>").
// Scoping the subject per session — rather than one global subject —
// keeps it consistent with every other channelevents subject and means
// the responder derives the session identity from the subject, not from
// caller-supplied payload fields.
func HistoryRequestSubject(natsSubjectPrefix string) string {
	return subjects.PrefixOf(natsSubjectPrefix).History()
}

// HistorySubscribeSubject is the wildcard subject channelsd's history
// responder subscribes to — every session's history request subject. Derived
// from the same leaf name HistoryRequestSubject publishes on, so the two
// cannot drift.
const HistorySubscribeSubject = subjects.AnyHistory

// HistoryQueueGroup is the queue group for the history responder so a
// replicated channelsd has exactly one responder per request.
const HistoryQueueGroup = "channelsd-history"

// ParseHistorySubject extracts the namespace and session name from a
// history request subject ("ap.session.<ns>.<name>.history.request") — the
// inverse of HistoryRequestSubject.
func ParseHistorySubject(subject string) (ns, name string, ok bool) {
	return subjects.ParseHistory(subject)
}

// HistoryRequest is the read_thread_history request payload. The session
// is identified by the subject (see HistoryRequestSubject), not here.
type HistoryRequest struct {
	BeforeCursor string `json:"beforeCursor,omitempty"` // empty -> responder defaults to BackfilledFromTS
	// Limit caps returned messages; zero means the responder's own default.
	Limit int `json:"limit,omitempty"`
}

// HistoryResponseMessage is one prior message in a HistoryResponse.
type HistoryResponseMessage struct {
	AuthorDisplayName string `json:"authorDisplayName"`
	Text              string `json:"text"`
	// TS is the kind-opaque message cursor (Slack: the message ts), the same
	// grammar HistoryRequest.BeforeCursor takes.
	TS string `json:"ts"`
}

// HistoryResponse is the read_thread_history reply payload. A non-empty
// Error means the request could not be served; Messages is then empty.
type HistoryResponse struct {
	// Messages are oldest-first; empty means the page held nothing (not an
	// error — Error carries that).
	Messages []HistoryResponseMessage `json:"messages"`
	// OldestCursor is the oldest returned message's TS — what to send as the
	// next BeforeCursor. Empty when Messages is empty.
	OldestCursor string `json:"oldestCursor"`
	// HasMore reports that older messages exist beyond OldestCursor.
	HasMore bool `json:"hasMore"`
	// Error is the human-readable reason the request could not be served.
	// Empty on success — the tool surfaces it rather than returning silence.
	Error string `json:"error,omitempty"`
}

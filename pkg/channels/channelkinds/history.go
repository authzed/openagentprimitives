package channelkinds

import (
	"context"
	"time"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// ConversationReader is implemented by channel kinds whose transport can
// retrieve prior messages for a conversation (Slack threads). Optional and
// discovered by type assertion, not part of the Kind interface: kinds without
// retrievable history simply do not implement it, and the pipeline and the
// read_thread_history tool no-op when the assertion fails.
type ConversationReader interface {
	// ReadHistory returns prior messages for channelKey, oldest-first.
	// deps carries the Channel CR and credentials Secret (same shape as
	// LookupUser). The implementation is pure transport: it returns
	// kind-native identifiers and facts and never touches SpiceDB or
	// canonicalisation.
	ReadHistory(ctx context.Context, deps Deps, channelKey string, opts ReadHistoryOpts) (HistoryPage, error)
}

// ReadHistoryOpts bounds a ReadHistory call. AfterTS / BeforeTS are
// kind-opaque ordering cursors (a Slack `ts` for Slack); an empty cursor
// means "unbounded on that side". Limit caps the number of messages
// returned; 0 means the kind's default.
type ReadHistoryOpts struct {
	AfterTS  string
	BeforeTS string
	Limit    int
}

// HistoryPage is the result of ReadHistory.
type HistoryPage struct {
	// Messages is chronological, oldest-first.
	Messages []HistoryMessage
	// HasMore is true when older messages exist before the returned
	// window (the window was Limit-capped). The pipeline surfaces this
	// to the agent as an "earlier messages omitted" note.
	HasMore bool
	// NextCursor is a kind-native opaque pagination cursor for the next
	// (older) page, set by cursor-paginating kinds (Slack conversations.history
	// returns response_metadata.next_cursor). Empty when the kind paginates by
	// message TS instead (the thread reader). Callers prefer NextCursor when
	// non-empty, else fall back to the oldest message's TS.
	NextCursor string
}

// HistoryMessage is one prior message. The reader returns raw facts;
// the pipeline turns AuthorExternalID/AuthorEmail into a canonical
// SpiceDB subject via identity.Principal.Canonical().
type HistoryMessage struct {
	AuthorExternalID  string // kind-native user id (Slack user id)
	AuthorDisplayName string // human-readable name for transcript attribution
	AuthorEmail       string // for canonicalisation; "" when unavailable
	Text              string
	TS                string // kind-opaque ordering cursor (Slack ts)
	FromApp           bool   // posted by a bot/app — excluded from participants
	// FromSelf marks a message this agent's OWN bot identity posted. It is a
	// strict subset of FromApp, and the kind sets it (only the kind knows its
	// own bot id). The distinction matters on catch-up: the agent's own replies
	// must not be re-seeded (they are already in its memory), but a THIRD-PARTY
	// app — an alerting webhook, a CI bot — is posting content the agent has
	// never seen and must not be dropped.
	FromSelf bool
}

// ChannelHistoryReader is implemented by channel kinds that can read the prior
// history of an ENTIRE channel (not just one thread). Optional and discovered
// by type-assertion, exactly like ConversationReader. Kept distinct from
// ConversationReader so the security-sensitive whole-channel path is visibly
// separate and can advertise bounds + its authz object.
type ChannelHistoryReader interface {
	// ChannelHistoryBounds returns the static limits this kind enforces.
	// MUST NOT perform I/O — read at validation/assembly time.
	ChannelHistoryBounds() ChannelHistoryBounds

	// ReadChannelHistory reads whole-channel history for the channel identified
	// by binding, oldest-first, bounded by opts. The caller (channelsd
	// responder) has already clamped opts to (CR ceiling ∩ ChannelHistoryBounds)
	// and derived the channel from the session — the implementation extracts its
	// kind-native channel id from binding (never from tool args). Pure transport:
	// returns kind-native identifiers and never touches SpiceDB.
	ReadChannelHistory(ctx context.Context, deps Deps, binding *spiceboxv1alpha1.ChannelBinding, opts ChannelHistoryOpts) (HistoryPage, error)

	// ChannelViewSubjectRef returns the SpiceDB subject-set ref whose members
	// are authorized to VIEW the channel identified by binding (e.g.
	// "slack_channel:<id>#view"), used by the info-leakage-on read-time check.
	// ok=false when the kind cannot derive one (the responder then fails closed
	// under info-leakage-on).
	ChannelViewSubjectRef(binding *spiceboxv1alpha1.ChannelBinding) (subjectSetRef string, ok bool)
}

// ChannelHistoryBounds are a kind's static channel-history limits.
type ChannelHistoryBounds struct {
	MaxLookback       time.Duration // hard ceiling on how far back reads reach
	MaxMessages       int           // hard ceiling on messages per read
	SupportsDateRange bool          // kind honors NotBefore/NotAfter as dates
}

// ChannelHistoryOpts bounds a ReadChannelHistory call. Time bounds are wall
// clock (the responder converts lookback/date-range into these); the kind
// converts them into its native cursor format. BeforeCursor is a kind-opaque
// pagination cursor from a prior page's NextCursor.
type ChannelHistoryOpts struct {
	NotBefore    time.Time // oldest message time to return (zero => kind default)
	NotAfter     time.Time // newest message time (zero => now)
	Limit        int       // max messages (already clamped)
	BeforeCursor string    // opaque pagination cursor (older page)
}

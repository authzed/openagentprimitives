package slack

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	slackapi "github.com/slack-go/slack"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/notice"
)

// BuildSlackThreadURL constructs a Slack deep link that opens the
// originating thread directly in either the desktop or web client.
// Returns "" when any of the inputs is empty (e.g. a non-thread DM
// channel — there's nothing to link to).
//
// The URL format is the canonical app.slack.com deep link Slack uses
// for "Copy link to thread" in its own UI. It does NOT require the
// workspace subdomain (which we don't reliably have); team_id is
// sufficient. See:
//
//	https://app.slack.com/client/<TEAM>/<CHAN>/thread/<CHAN>-<TS>
//
// Used by forkRootText/forkRootCache below (the restart-fork first-send
// path's new-thread backlink + parent-thread notice).
func BuildSlackThreadURL(teamID, channelID, threadTS string) string {
	if teamID == "" || channelID == "" || threadTS == "" {
		return ""
	}
	return fmt.Sprintf("https://app.slack.com/client/%s/%s/thread/%s-%s",
		teamID, channelID, channelID, threadTS)
}

// forkRootNotice renders the FIRST message of a forked thread. It is posted at
// fork time, before the runner starts, so it becomes the thread root and every
// later message threads beneath it. That ordering is the point: a reader
// arriving cold sees where this conversation came from, not a status caption.
//
// The body carries Slack link syntax, which is safe because this renderer is
// Slack's own — the notice is built and drawn inside this package and never
// crosses a wire to a channel that could not render it.
func forkRootNotice(parentTeamID, parentChannelID, parentThreadTS string, startedAt time.Time) *notice.Notice {
	url := BuildSlackThreadURL(parentTeamID, parentChannelID, parentThreadTS)
	// A zero startedAt means the thread_ts did not parse. Say nothing about when
	// rather than render the zero instant — "posted Mon 1 Jan 0001" is worse than
	// no date at all. The link is independent of the timestamp, so it survives.
	when := ""
	if !startedAt.IsZero() {
		when = ", posted " + startedAt.UTC().Format("Mon 2 Jan 2006 15:04 MST")
	}
	body := fmt.Sprintf("This picks up from an earlier conversation%s.", when)
	if url != "" {
		body = fmt.Sprintf("This picks up from <%s|an earlier conversation>%s.", url, when)
	}
	return notice.New(categories.ThreadContinuedFrom, notice.Args{
		Lead:     "Continued from an earlier conversation",
		Body:     body,
		Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceParticipants},
	})
}

// threadMovedNotice is posted in the OLD thread so a reader there can follow
// the conversation forward. Terminal: nothing further arrives in the thread
// they are looking at.
func threadMovedNotice(newURL string) *notice.Notice {
	return notice.New(categories.ThreadMoved, notice.Args{
		Lead:     "This conversation continues in a new thread",
		Body:     fmt.Sprintf("The session was restarted. Follow it in <%s|the new thread>.", newURL),
		Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceParticipants},
	})
}

// slackTSTime converts a Slack "<epoch>.<micros>" timestamp to a time.Time.
// Returns the zero time when ts is not parseable; callers render a link-free
// fallback rather than a wrong date.
func slackTSTime(ts string) time.Time {
	var sec, frac int64
	if _, err := fmt.Sscanf(ts, "%d.%d", &sec, &frac); err != nil {
		return time.Time{}
	}
	return time.Unix(sec, 0).UTC()
}

// forkRootCache creates a forked thread's root message exactly once, no matter
// which sender reaches the channel first. The agent's reply and the tool-session
// streaming bubble race here; whoever wins posts the link-back framing message,
// and everyone else threads beneath it. Without this the bubble could establish
// the root and the framing was skipped for the life of the thread.
type forkRootCache struct {
	mu    sync.Mutex
	roots map[string]*forkRootEntry // "ns/name" → that session's root
}

// forkRootEntry is one session's resolution slot. resolve serializes the
// resolvers FOR THAT SESSION — the "one root, ever" guarantee — and also
// guards ts/done, which are only ever read or written while it is held.
type forkRootEntry struct {
	resolve sync.Mutex
	ts      string // thread root ts ("" = not a fork child)
	done    bool   // ts is authoritative; skip the lookup
}

func newForkRootCache() *forkRootCache {
	return &forkRootCache{roots: map[string]*forkRootEntry{}}
}

// entryFor returns the per-session slot, creating it on first use. The map
// mutex is held only for this map access — never across the I/O below.
func (c *forkRootCache) entryFor(key string) *forkRootEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.roots[key]
	if !ok {
		e = &forkRootEntry{}
		c.roots[key] = e
	}
	return e
}

// ensure returns the thread-root ts for a fork child, posting the link-back
// framing message on first call. Returns "" when sess is not a fork child, when
// the annotation is malformed, or when the post fails — callers then fall back
// to their existing behavior rather than dropping the message.
//
// The K8s Get and the Slack posts happen under a PER-SESSION lock: correctness
// (one root, ever) matters more than concurrent sends racing to create two
// threads, so two senders for the same session take turns and the second reuses
// the first's answer. The cache itself is a per-Kind singleton shared by every
// session, so the lock must not be — ensure is reached from the outbound relay's
// dispatcher, the status watchdog's tick, and the session watcher, on any send
// whose resolved thread_ts is empty. One process-wide lock would let a single
// slow or rate-limited Slack post freeze the fork-root path for every unrelated
// session behind it.
func (c *forkRootCache) ensure(ctx context.Context, cli slackClient, k8s client.Client, sess channelkinds.SessionInfo, channelID string) string {
	if k8s == nil || cli == nil {
		return ""
	}
	key := sess.Namespace + "/" + sess.Name
	entry := c.entryFor(key)
	entry.resolve.Lock()
	defer entry.resolve.Unlock()
	if entry.done {
		return entry.ts
	}

	var as spiceboxv1alpha1.AgentSession
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: sess.Namespace, Name: sess.Name}, &as); err != nil {
		log.FromContext(ctx).Info("slack: forkRoot: get session failed (best-effort, continuing)",
			"session", key, "err", err.Error())
		return ""
	}
	forkedFrom := as.Annotations[spiceboxv1alpha1.AnnotationForkedFromThread]
	if forkedFrom == "" {
		entry.done = true // ordinary session; never look again
		return ""
	}
	parts := strings.SplitN(forkedFrom, ":", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		log.FromContext(ctx).Info("slack: forkRoot: malformed forked-from-thread annotation (best-effort, skipping)",
			"session", key, "annotation", forkedFrom)
		entry.done = true
		return ""
	}
	parentTeamID, parentChannelID, parentThreadTS := parts[0], parts[1], parts[2]

	sessRef := channelevents.SessionRef{Namespace: sess.Namespace, Name: sess.Name}
	ts, err := postNoticeReturningTS(ctx, cli, channelID,
		forkRootNotice(parentTeamID, parentChannelID, parentThreadTS, slackTSTime(parentThreadTS)),
		sessRef, "notice-fork-root-"+sess.Name)
	if err != nil {
		log.FromContext(ctx).Info("slack: forkRoot: posting the link-back root failed (best-effort, continuing)",
			"session", key, "channelID", channelID, "err", err.Error())
		return "" // do NOT cache: a later send retries
	}
	entry.ts, entry.done = ts, true

	// Backward link in the OLD thread, so a reader there can follow forward.
	// Best-effort: the child's reply must never be gated on it.
	if newURL := BuildSlackThreadURL(parentTeamID, channelID, ts); newURL != "" {
		if _, berr := postNoticeReturningTS(ctx, cli, parentChannelID,
			threadMovedNotice(newURL), sessRef, "notice-thread-moved-"+sess.Name,
			slackapi.MsgOptionTS(parentThreadTS)); berr != nil {
			log.FromContext(ctx).Info("slack: forkRoot: forward-link in parent thread failed (best-effort, continuing)",
				"session", key, "parentChannelID", parentChannelID, "err", berr.Error())
		}
	}
	return ts
}

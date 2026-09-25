// pkg/channels/channelkinds/slack/thread_title.go
//
// threadTitleSender renders KindThreadTitle envelopes (an agent-chosen
// conversation title, plus an optional leading emoji). Slack has no
// per-channel-thread title surface, so the sender picks the best available
// substitute:
//
//   - A bot-rooted channel thread (the listener cached a starter message's
//     coordinates for this session) → edit the starter message in place
//     (chat.update) with a title-forward rendering, keeping the original
//     "started <!date…>" clause so the timestamp keeps live-rendering.
//   - A DM (assistant thread, channel ID starts with "D") with no starter →
//     the native assistant.threads.setTitle, folding the emoji into the
//     title text since setTitle has no separate glyph slot.
//   - Neither → a logged no-op; there is no surface to carry a title.
//
// Two per-session guards keep this idempotent under replay/out-of-order
// delivery: an ordering guard drops envelopes whose Seq is stale relative to
// the last applied one from the same SessionUID, and a redundancy guard skips
// the API call entirely when (title, emoji) is unchanged from the last
// applied pair.
package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	slackapi "github.com/slack-go/slack"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// defaultStarterGlyph is the leading glyph used when the agent sets a title
// without an emoji override.
const defaultStarterGlyph = "🤖"

// titleState is the last-applied (title, emoji) pair for a session, plus the
// ordering keys needed to detect stale/out-of-order envelopes.
type titleState struct {
	title, emoji, uid string
	seq               uint64
	applied           bool
}

// threadTitleSender is package-internal; construct via newThreadTitleSender
// (production) or a direct struct literal (tests, injecting a fake client +
// a fresh starterCache).
type threadTitleSender struct {
	deps     channelkinds.Deps
	client   slackClient
	starters *starterCache

	mu   sync.Mutex
	last map[string]titleState
}

func newThreadTitleSender(deps channelkinds.Deps) *threadTitleSender {
	return &threadTitleSender{
		deps:   deps,
		client: newSlackAPIClient(deps.Secret),
		last:   map[string]titleState{},
	}
}

// Send conforms to channelkinds.Sender.
func (s *threadTitleSender) Send(ctx context.Context, sess channelkinds.SessionInfo, env channelevents.Envelope) (channelkinds.SubChannelSendResult, error) {
	logger := log.FromContext(ctx)
	if s.client == nil {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("slack thread_title: bot-token missing from credentials Secret")
	}
	if sess.Channel == nil {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("slack thread_title: session has no channel binding")
	}

	var pl channelevents.ThreadTitlePayload
	if err := json.Unmarshal(env.Payload, &pl); err != nil {
		logger.Info("slack: thread_title bad payload (dropping)", "session", sess.Name, "err", err.Error())
		return channelkinds.SubChannelSendResult{}, nil
	}
	key := sess.Namespace + "/" + sess.Name

	// Ordering + redundancy guards, both keyed on the session's last-applied
	// state. Held under s.mu only long enough to read/decide; the actual
	// Slack API call happens outside the lock.
	s.mu.Lock()
	prev, ok := s.last[key]
	if ok && prev.applied && prev.uid == env.SessionUID && env.Seq != 0 && env.Seq <= prev.seq {
		s.mu.Unlock()
		logger.Info("slack: thread_title stale envelope dropped",
			"session", sess.Name, "seq", env.Seq, "lastSeq", prev.seq)
		return channelkinds.SubChannelSendResult{}, nil
	}
	if ok && prev.applied && prev.title == pl.Title && prev.emoji == pl.Emoji {
		// Same (title, emoji) as last applied: skip the API call, but still
		// advance the ordering bookkeeping so a later, genuinely-new title
		// isn't compared against a stale seq.
		s.last[key] = titleState{title: pl.Title, emoji: pl.Emoji, uid: env.SessionUID, seq: env.Seq, applied: true}
		s.mu.Unlock()
		return channelkinds.SubChannelSendResult{}, nil
	}
	s.mu.Unlock()

	channelID := sess.Channel.External["channel_id"]
	threadTS := effectiveOutboundThreadTS(sess)
	glyph := defaultStarterGlyph
	if strings.TrimSpace(pl.Emoji) != "" {
		glyph = pl.Emoji
	}

	applied := false
	switch {
	case s.hasStarter(key):
		coords, _ := s.starters.get(key)
		text := formatTitledStarter(glyph, pl.Title, coords.AgentName, coords.StartedUnix)
		if _, _, _, err := s.client.UpdateMessageContext(ctx, coords.ChannelID, coords.MessageTS,
			slackapi.MsgOptionText(text, false)); err != nil {
			logger.Info("slack: thread_title chat.update failed (best-effort, continuing)",
				"session", sess.Name, "channelID", coords.ChannelID, "ts", coords.MessageTS, "err", err.Error())
		} else {
			applied = true
		}
	case strings.HasPrefix(channelID, "D"):
		titleText := pl.Title
		if glyph != defaultStarterGlyph { // emoji explicitly set: setTitle has no separate glyph slot
			titleText = glyph + " " + pl.Title
		}
		if err := s.client.SetAssistantThreadsTitleContext(ctx, slackapi.AssistantThreadsSetTitleParameters{
			ChannelID: channelID, ThreadTS: threadTS, Title: titleText,
		}); err != nil {
			logger.Info("slack: thread_title setTitle failed (best-effort, continuing)",
				"session", sess.Name, "channelID", channelID, "threadTS", threadTS, "err", err.Error())
		} else {
			applied = true
		}
	default:
		logger.Info("slack: thread_title no-op — no cached starter and no DM surface to title",
			"session", sess.Name, "channelID", channelID)
	}

	s.mu.Lock()
	s.last[key] = titleState{title: pl.Title, emoji: pl.Emoji, uid: env.SessionUID, seq: env.Seq, applied: applied || (ok && prev.applied)}
	s.mu.Unlock()
	return channelkinds.SubChannelSendResult{}, nil
}

// hasStarter reports whether a starter message's coordinates are cached for
// this session key.
func (s *threadTitleSender) hasStarter(key string) bool {
	if s.starters == nil {
		return false
	}
	_, ok := s.starters.get(key)
	return ok
}

// formatTitledStarter renders the title-forward replacement for the starter
// message. The started clause reuses the ORIGINAL unix timestamp (captured
// when the starter was first posted) so Slack's <!date…> token keeps
// rendering the true start time, not the moment of this edit.
//
// glyph and title are made inert first, and the ORDER is the property. Both
// come from the set_thread_title META TOOL, so the model chooses both strings,
// and this line is posted through MsgOptionText(_, false) — mrkdwn, which
// slack-go does not escape. Unescaped, an agent could title its own thread
// `<https://attacker.example/x|Connect your account>` and have the message
// pinned at the top of the thread, styled as the platform's own header, render
// a genuine-looking link. (The agent's ordinary REPLY text is deliberately
// live — its message is its message, and slackifyText even un-escapes entities
// there. A title is not a message: it is chrome this kind composes.)
//
// Escaping the composed line instead would neutralise `<!date^…|just now>`,
// the token THIS function writes so each reader sees the start time in their
// own locale, and every reader would see the raw token instead.
//
// agentName is left live: it is an AgentClass CR name, DNS-1123-constrained by
// the API server, so it cannot contain & < > at all.
func formatTitledStarter(glyph, title, agentName string, startedUnix int64) string {
	return fmt.Sprintf("%s *%s*\n· %s · started <!date^%d^{date_short_pretty} at {time}|just now>",
		escapeSlackText(glyph), escapeSlackText(title), agentName, startedUnix)
}

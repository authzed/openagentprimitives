package slack

import (
	"context"
	"fmt"
	"strings"

	slackapi "github.com/slack-go/slack"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// historyClient is the slack-go subset history.go needs. The concrete
// *slackapi.Client satisfies it; tests inject a double via
// historyClientFactory. It is a separate interface from sender.go's
// slackClient because conversations.replies and users.info are not on
// that (outbound-message) surface.
type historyClient interface {
	GetConversationRepliesContext(ctx context.Context, params *slackapi.GetConversationRepliesParameters) ([]slackapi.Message, bool, string, error)
	GetUserInfoContext(ctx context.Context, user string, _ ...slackapi.GetUserInfoOption) (*slackapi.User, error)
	GetConversationHistoryContext(ctx context.Context, params *slackapi.GetConversationHistoryParameters) (*slackapi.GetConversationHistoryResponse, error)
	GetBotInfoContext(ctx context.Context, params slackapi.GetBotInfoParameters) (*slackapi.Bot, error)
}

// historyClientFactory builds a historyClient from Deps. Overridden in
// tests. The real factory builds a bot-token slack-go client directly —
// conversations.replies and users.info need only the bot token.
var historyClientFactory = func(deps channelkinds.Deps) historyClient {
	if deps.Secret == nil {
		return nil
	}
	tok, ok := deps.Secret.Data[SecretKeyBotToken]
	if !ok || len(tok) == 0 {
		return nil
	}
	return slackapi.New(string(tok))
}

// ReadHistory implements channelkinds.ConversationReader for Slack.
// channelKey has the form "thread:<channel_id>:<thread_ts>".
func (k *Kind) ReadHistory(
	ctx context.Context, deps channelkinds.Deps, channelKey string,
	opts channelkinds.ReadHistoryOpts,
) (channelkinds.HistoryPage, error) {
	channelID, threadTS, err := parseThreadKey(channelKey)
	if err != nil {
		return channelkinds.HistoryPage{}, err
	}
	cli := historyClientFactory(deps)
	if cli == nil {
		return channelkinds.HistoryPage{}, fmt.Errorf("slack ReadHistory: no API client (credentials Secret missing bot-token?)")
	}

	raw, hasMore, _, err := cli.GetConversationRepliesContext(ctx, &slackapi.GetConversationRepliesParameters{
		ChannelID: channelID,
		Timestamp: threadTS,
		Oldest:    opts.AfterTS,
		Latest:    opts.BeforeTS,
		Limit:     opts.Limit,
		Inclusive: false,
	})
	if err != nil {
		return channelkinds.HistoryPage{}, fmt.Errorf("slack conversations.replies: %w", err)
	}

	out := resolveAuthors(ctx, cli, raw, selfBotUserID(deps), k.resolveInstalledTeamID(ctx, cli))
	return channelkinds.HistoryPage{Messages: out, HasMore: hasMore}, nil
}

// selfBotUserID returns the Slack user id of THIS channel's bot, or "" when it
// has not been resolved yet. Only the kind can answer "is this message mine?",
// which is why HistoryMessage.FromSelf is set here rather than inferred by the
// pipeline. An empty id means nothing is marked FromSelf — the safe direction:
// the agent may re-read its own reply, but no third-party content is dropped.
func selfBotUserID(deps channelkinds.Deps) string {
	if deps.Channel == nil || deps.Channel.Spec.Slack == nil {
		return ""
	}
	return deps.Channel.Spec.Slack.BotUserID
}

// resolveAuthors turns raw slack messages into channelkinds.HistoryMessages,
// resolving each distinct human author's display name + email once per call
// via users.info. App/bot messages get a label from the identity Slack
// attached to the message itself (see authorLabel) — never a users.info call,
// and never a user id. A users.info failure for one author degrades that
// message to id-only attribution; it never fails the page.
//
// Message content comes from messageText, not m.Text: alert webhooks post
// their entire payload in blocks/attachments with an empty top-level text.
// installedTeamID is the workspace this bot token belongs to, and it decides
// whether an author's profile email may be used as an authorization identity —
// the same emailTrusted gate the live inbound path applies. Empty fails closed:
// nothing can be shown to be a member, so no email is trusted.
func resolveAuthors(ctx context.Context, cli historyClient, raw []slackapi.Message, selfBotUserID, installedTeamID string) []channelkinds.HistoryMessage {
	resolved := map[string]*slackapi.User{}
	bots := map[string]string{}
	out := make([]channelkinds.HistoryMessage, 0, len(raw))
	for i := range raw {
		m := raw[i]
		fromApp := m.BotID != "" || m.SubType == "bot_message" || m.User == ""
		hm := channelkinds.HistoryMessage{
			AuthorExternalID: m.User,
			Text:             messageText(m),
			TS:               m.Timestamp,
			FromApp:          fromApp,
			// Slack stamps our own bot's posts with its user id; a third-party
			// webhook carries none at all. Guard on a non-empty selfBotUserID so
			// an unresolved bot id can never make every app message "ours".
			FromSelf: fromApp && selfBotUserID != "" && m.User == selfBotUserID,
		}
		if fromApp {
			// Apps carry no user id to resolve; label them from whatever
			// identity Slack did attach so the transcript never renders the
			// message as "unknown". AuthorExternalID stays empty on purpose —
			// participant grants key off it and an app is not a participant.
			hm.AuthorDisplayName = authorLabel(m)
			// Slack attaches neither username nor bot_profile to a
			// webhook-posted message, so authorLabel fell back to the raw bot
			// id. bots.info is the only place a readable name exists; resolve
			// it once per distinct bot, exactly like users.info above.
			if m.BotID != "" && hm.AuthorDisplayName == m.BotID {
				if name := resolveBotName(ctx, cli, bots, m.BotID); name != "" {
					hm.AuthorDisplayName = name
				}
			}
		} else {
			u, ok := resolved[m.User]
			if !ok {
				fetched, ferr := cli.GetUserInfoContext(ctx, m.User)
				switch {
				case ferr != nil:
					// Degrading to id-only attribution is deliberate — one
					// unresolvable author must not fail the page. Logging it is
					// not: a revoked users:read scope and a workspace where
					// nobody set a display name both render as bare user ids,
					// and only one of them is a problem to fix.
					log.FromContext(ctx).Info("slack: users.info failed; attributing the message to its user id",
						"user", m.User, "err", ferr.Error())
				default:
					u = fetched
				}
				// Cached either way, including the nil: a failed lookup must not
				// be retried once per message from the same author.
				resolved[m.User] = u
			}
			if u != nil {
				hm.AuthorDisplayName = u.RealName
				if hm.AuthorDisplayName == "" {
					hm.AuthorDisplayName = u.Name
				}
				// The SAME gate the live inbound path applies. The canonical
				// SpiceDB subject is derived from this email whenever it is
				// non-empty, and thread-adoption backfill mints slot grants and
				// participant standing from that canonical — so a foreign,
				// guest, stranger, bot or deleted account's self-asserted
				// address must not reach it. Without this the history path
				// trusted an email the live path deliberately refuses, and the
				// pipeline consuming it believed the kind had already gated it.
				//
				// The display name is unaffected: it is presentation, not
				// identity, and dropping it would make the transcript
				// unreadable for exactly these participants.
				if emailTrusted(u, installedTeamID) {
					hm.AuthorEmail = u.Profile.Email
				}
			}
		}
		out = append(out, hm)
	}
	return out
}

// resolveBotName returns a bot's display name via bots.info, memoised in cache
// so a page with N messages from one bot costs one API call, not N. A negative
// result is cached too — an unresolvable bot must not be retried per message.
//
// Returns "" when the name cannot be determined, leaving the caller's bot-id
// fallback in place: attribution degrades, the page never fails. The failure is
// logged rather than swallowed, because a systematically missing scope
// (bots.info reads the workspace user directory, so it needs users:read) is
// otherwise invisible — every alert just keeps rendering as a raw bot id with
// nothing explaining why.
func resolveBotName(ctx context.Context, cli historyClient, cache map[string]string, botID string) string {
	if name, ok := cache[botID]; ok {
		return name
	}
	var name string
	b, err := cli.GetBotInfoContext(ctx, slackapi.GetBotInfoParameters{Bot: botID})
	switch {
	case err != nil:
		log.FromContext(ctx).Info("slack: bots.info failed; attributing the message to its bot id",
			"botID", botID, "err", err.Error())
	case b != nil:
		name = b.Name
	}
	cache[botID] = name
	return name
}

// parseThreadKey splits "thread:<channel_id>:<thread_ts>". The channel id
// has no colon; the thread ts is the remainder.
func parseThreadKey(channelKey string) (channelID, threadTS string, err error) {
	rest, ok := strings.CutPrefix(channelKey, "thread:")
	if !ok {
		return "", "", fmt.Errorf("slack ReadHistory: channelKey %q is not a thread key", channelKey)
	}
	channelID, threadTS, ok = strings.Cut(rest, ":")
	if !ok || channelID == "" || threadTS == "" {
		return "", "", fmt.Errorf("slack ReadHistory: malformed thread key %q", channelKey)
	}
	return channelID, threadTS, nil
}

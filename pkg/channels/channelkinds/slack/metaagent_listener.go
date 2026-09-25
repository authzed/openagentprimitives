// pkg/channels/channelkinds/slack/metaagent_listener.go
//
// Metaagent mention routing (F1/F4). When an app_mention contains
// <@METAAGENT_BOT_USER_ID>, the listener publishes a raw JSON payload to
// ap.session.<ns>.<name>.in.metaagent_request (consumed by authzd) rather
// than dispatching to the session runner via handleChannelMessage.
//
// The session must already be active in the thread; if not, an ephemeral
// notice is posted to the mentioner.
package slack

import (
	"context"
	"fmt"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	slackapi "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkey"
)

// SetMetaagentBotUserID configures the metaagent Slack bot user-id on this
// listener. Called by internal/cmd/channelsd at startup with the value of the
// METAAGENT_SLACK_BOT_USER_ID env var. An empty string disables the feature.
func (l *slackListener) SetMetaagentBotUserID(id string) {
	l.metaagentBotUserID = id
}

// handleMetaagentMention routes an app_mention that targets the metaagent bot.
// It resolves the active session for the current thread, then publishes a
// MetaagentRequestPayload on that session's in.metaagent_request subject.
//
// If no active session exists for the thread (the channel-key produces no
// match), it posts an ephemeral notice to the mentioner and drops the event.
func (l *slackListener) handleMetaagentMention(ctx context.Context, ev *slackevents.AppMentionEvent) {
	logger := log.FromContext(ctx).WithValues(
		"user", ev.User,
		"channel", ev.Channel,
		"threadTS", ev.ThreadTimeStamp,
	)

	// Resolve the anchor: prefer an existing thread_ts so in-thread replies
	// resolve correctly; fall back to the message's own ts when it is a
	// top-level @mention starting a new thread root.
	anchor := ev.ThreadTimeStamp
	if anchor == "" {
		anchor = ev.TimeStamp
	}

	channelKey := "thread:" + ev.Channel + ":" + anchor
	keyHash := channelkey.LabelValue(channelKey)

	if l.deps.K8sClient == nil || l.deps.Channel == nil {
		logger.Info("metaagent_mention: no K8s client or channel wired; dropping")
		return
	}

	// Resolve the active session for this thread via the label index.
	var sessions spiceboxv1alpha1.AgentSessionList
	if err := l.deps.K8sClient.List(ctx, &sessions,
		client.InNamespace(l.deps.Channel.Namespace),
		client.MatchingLabels{
			spiceboxv1alpha1.LabelChannelName: l.deps.Channel.Name,
			spiceboxv1alpha1.LabelChannelKey:  keyHash,
		},
	); err != nil {
		logger.Info("metaagent_mention: session list failed", "err", err.Error())
		l.postEphemeralNotice(ctx, ev.Channel, ev.User, anchor,
			"⚠️ Internal error looking up agent session. Please try again.")
		return
	}

	// Find the most-recently-created active session.
	var active *spiceboxv1alpha1.AgentSession
	for i := range sessions.Items {
		s := &sessions.Items[i]
		switch s.Status.Phase {
		case spiceboxv1alpha1.AgentSessionPhasePending,
			spiceboxv1alpha1.AgentSessionPhaseRunning,
			spiceboxv1alpha1.AgentSessionPhaseIdle,
			spiceboxv1alpha1.AgentSessionPhaseAwaitingApproval,
			spiceboxv1alpha1.AgentSessionPhaseAwaitingDecision:
			if active == nil || s.CreationTimestamp.After(active.CreationTimestamp.Time) {
				active = s
			}
		}
	}
	if active == nil {
		logger.Info("metaagent_mention: no active session in thread; posting notice")
		primaryBotMention := "<@" + l.botUserID + ">"
		l.postEphemeralNotice(ctx, ev.Channel, ev.User, anchor,
			fmt.Sprintf("No active agent session in this thread. Start a session with %s first.", primaryBotMention))
		return
	}

	cleanText := stripMetaagentMention(ev.Text, l.metaagentBotUserID)

	// Canonicalize the requester to the SpiceDB subject form "user:<canonical>"
	// BEFORE publishing: authzd's mid-session manage_scope gate checks
	// agentsession#manage_scope@user:<canonical>, so a raw Slack user_id would
	// never match the canonical started_by tuple and would deny every owner.
	// Cold-start already passes a canonical subject; this brings mid-session into
	// line (channelsd is where every other inbound subject is canonicalized).
	requester, cerr := l.resolveCanonicalForSlackUser(ctx, ev.User)
	if cerr != nil {
		logger.Info("metaagent_mention: canonicalize requester failed; dropping",
			"err", cerr.Error())
		l.postEphemeralNotice(ctx, ev.Channel, ev.User, anchor,
			"⚠️ Could not verify your identity for a scope change. Please try again.")
		return
	}

	if err := channelevents.PublishMetaagentIn(l.deps.NATSPublish,
		active.Namespace, active.Name, channelevents.KindMetaagentRequest,
		channelevents.MetaagentRequestPayload{Requester: requester, Text: cleanText},
	); err != nil {
		logger.Info("metaagent_mention: publish metaagent_request failed",
			"session", active.Namespace+"/"+active.Name, "err", err.Error())
		// The publish failed: authzd never receives the scope-change request,
		// so the user's @-mention would otherwise silently do nothing. Tell
		// the mentioner their request didn't go through so they can retry.
		l.postEphemeralNotice(ctx, ev.Channel, ev.User, anchor,
			"⚠️ Couldn't reach the agent to record your scope-change request. Please try again.")
		return
	}
	logger.Info("metaagent_mention: published metaagent_request",
		"session", active.Namespace+"/"+active.Name,
		"requester", ev.User)
}

// postEphemeralNotice posts a one-line ephemeral message to userID in
// channelID, threaded under threadTS when non-empty. Best-effort: errors are
// logged and swallowed — the notice path must not block the event loop.
func (l *slackListener) postEphemeralNotice(ctx context.Context, channelID, userID, threadTS, text string) {
	sc := l.ephemeralSurface()
	if sc == nil {
		return
	}
	opts := []slackapi.MsgOption{slackapi.MsgOptionText(text, false)}
	if threadTS != "" {
		opts = append(opts, slackapi.MsgOptionTS(threadTS))
	}
	if _, err := sc.PostEphemeralContext(ctx, channelID, userID, opts...); err != nil {
		log.FromContext(ctx).Info("metaagent_mention: post ephemeral notice failed (best-effort)",
			"channelID", channelID, "userID", userID, "err", err.Error())
	}
}

// stripMetaagentMention removes all occurrences of <@botUserID> from text and
// trims leading/trailing whitespace from the result.
func stripMetaagentMention(text, botUserID string) string {
	return strings.TrimSpace(strings.ReplaceAll(text, "<@"+botUserID+">", ""))
}

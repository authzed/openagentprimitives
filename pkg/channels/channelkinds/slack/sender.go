// pkg/channels/channelkinds/slack/sender.go
//
// Slack outbound: renders runner-emitted user_message envelopes to chat.postMessage,
// and renders ephemeral inbound denials to chat.postEphemeral.
//
// Routing data consumed from session.spec.channel.external:
//   - channel_id  (required) — D... for DMs, C.../G... for public/private threads
//   - thread_ts   (optional) — set for thread sessions, absent for DMs
//
// ThreadTS resolution for setStatus calls:
//
//	The sender uses the LastInboundTSAnnotationKey annotation (stamped on every
//	inbound by the listener) as the primary source of thread_ts, falling back
//	to External["thread_ts"] if the annotation is absent. The annotation is
//	always up to date with the most recent inbound, so it is the preferred
//	source even for threaded sessions (where both values would agree).
package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	slackapi "github.com/slack-go/slack"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// slackClient is the subset of slack-go's API we use; an interface so tests
// can mock without dragging the real SDK transport.
type slackClient interface {
	PostMessageContext(ctx context.Context, channelID string, opts ...slackapi.MsgOption) (channel string, ts string, err error)
	PostEphemeralContext(ctx context.Context, channelID, userID string, opts ...slackapi.MsgOption) (string, error)
	UpdateMessageContext(ctx context.Context, channelID, ts string, opts ...slackapi.MsgOption) (string, string, string, error)
	OpenConversationContext(ctx context.Context, params *slackapi.OpenConversationParameters) (*slackapi.Channel, bool, bool, error)
	SetAssistantThreadsStatusContext(ctx context.Context, params slackapi.AssistantThreadsSetStatusParameters) error
	// SetAssistantThreadsTitleContext sets the assistant thread's title via
	// assistant.threads.setTitle. Used by the sender to reflect an
	// AgentSession's derived/updated title in the Slack thread header.
	SetAssistantThreadsTitleContext(ctx context.Context, params slackapi.AssistantThreadsSetTitleParameters) error
	// UploadFileContext is slack-go v0.23's renamed UploadFileV2Context; it
	// drives the files.uploadV2 / completeUploadExternal flow internally.
	// One file per call — multi-attachment messages loop in the sender.
	UploadFileContext(ctx context.Context, params slackapi.UploadFileParameters) (*slackapi.FileSummary, error)
	GetUserByEmailContext(ctx context.Context, email string) (*slackapi.User, error)
	GetUsersContext(ctx context.Context, options ...slackapi.GetUsersOption) ([]slackapi.User, error)
	// GetUserInfoContext resolves a user id to a profile. The sender needs the
	// direction the other two lookups do not: quoted message text carries ids,
	// not emails, and an id is what a reader cannot read (see userNamer).
	GetUserInfoContext(ctx context.Context, user string) (*slackapi.User, error)
	// OpenViewContext opens a Slack modal via views.open. Used by the
	// approval-flow's Show Details button to surface the full args /
	// MCP description / op-session refs out-of-thread.
	OpenViewContext(ctx context.Context, triggerID string, view slackapi.ModalViewRequest) (*slackapi.ViewResponse, error)
	// GetPermalinkContext returns a Slack permalink for a message/thread.
	// Used by the live-view click handler to build a "back to thread" link.
	GetPermalinkContext(ctx context.Context, params *slackapi.PermalinkParameters) (string, error)
}

// StreamFinalTargetConsumer retrieves the streaming bubble ts for the
// most-recently-finalized stream on a thread. Implemented by StreamDeltaSink.
// Tests stub it.
type StreamFinalTargetConsumer interface {
	ConsumeFinalTarget(channelID, threadTS string) string
}

// slackSender implements channelkinds.Sender for Slack Web API.
type slackSender struct {
	deps         channelkinds.Deps
	client       slackClient     // injectable for tests
	planMessages *planMessageMap // per-process (session, planName) → message ts
	// streamSink is the sink shared with the streaming path. The sender calls
	// ConsumeFinalTarget to retrieve the streaming bubble ts so it can
	// chat.update the bubble with the polished final reply rather than posting
	// fresh. Nil in direct-construction tests, which is treated as "no
	// streaming occurred" (falls through to chat.postMessage).
	streamSink StreamFinalTargetConsumer
	// toolSessionSeen is the shared set of sessions that streamed tool
	// output. Injected by Kind; nil in direct-construction tests,
	// which is treated as "no tool sessions".
	toolSessionSeen *toolSessionSeen
	// settingsSeen tracks sessions whose first agent message already
	// carried the "Show settings" button. Injected by Kind; nil in
	// direct-construction tests, which is treated as "always first" so the
	// button is included.
	settingsSeen *toolSessionSeen
	// forkRoot creates a restart-fork child's thread root exactly once,
	// shared with toolSessionSender so whichever one reaches the channel
	// first wins. Injected by Kind; nil in direct-construction tests,
	// treated as no fork children.
	forkRoot *forkRootCache
	// statusCache remembers the most recent assistant.threads.setStatus
	// payload per session. Slack auto-clears the indicator whenever
	// the agent posts a chat.postMessage in the assistant thread —
	// including the first plan_update post. We re-issue setStatus
	// from this cache after such posts so the user-visible "is
	// thinking…" caption doesn't blink to empty until the next
	// update_status call.
	statusMu    sync.Mutex
	statusCache map[string]cachedStatus
	// toolProgress is the per-session in-flight set of running SYNC tools,
	// keyed sessionKey -> callID -> entry. Aggregated from KindToolProgress
	// ticks; composed onto the agent caption by sendToolProgress. Distinct
	// from statusCache (the agent's own caption) — this augments, never
	// overwrites it.
	toolProgMu   sync.Mutex
	toolProgress map[string]map[string]toolProgEntry
}

// cachedStatus holds the inputs needed to re-call
// SetAssistantThreadsStatusContext after a chat.postMessage clears
// it. ChannelID/ThreadTS are the resolved values used the first
// time, so re-issue is exactly equivalent to the original call.
type cachedStatus struct {
	channelID, threadTS string
	status              string
	// loading is the bare loading-message caption, without a spinner frame;
	// re-issue paths fan it back out via animatedLoadingMessages.
	loading string
}

func newSlackSender(deps channelkinds.Deps) *slackSender {
	return &slackSender{
		deps:         deps,
		client:       newSlackAPIClient(deps.Secret),
		planMessages: newPlanMessageMap(),
		statusCache:  make(map[string]cachedStatus),
		toolProgress: make(map[string]map[string]toolProgEntry),
	}
}

// newSlackAPIClient constructs a real slack-go client from the bot-token in
// the Secret. Returns a no-op client if the Secret is missing or malformed —
// the Send call will then surface the misconfiguration as an error.
func newSlackAPIClient(sec *corev1.Secret) slackClient {
	// When a test has installed a shared fake via InstallTestTransport, it must
	// back BOTH the listener and sender factories — the fake structurally
	// satisfies both listenerAPIClient and slackClient (see the compile-time
	// proofs in fakeslack_seam_test.go for slackClient/historyClient and
	// fakeslack_transport_seam_test.go for listenerAPIClient/socketSource), so
	// the comma-ok assertion only fails if a future override value doesn't
	// implement slackClient.
	if testClientOverride != nil {
		if sc, ok := testClientOverride.(slackClient); ok {
			return sc
		}
	}
	if sec == nil {
		return nil
	}
	tok, ok := sec.Data[SecretKeyBotToken]
	if !ok || len(tok) == 0 {
		return nil
	}
	return slackapi.New(string(tok))
}

// maybeAppendMentionFooter appends a one-line "@-mention to reply" hint
// when the session's binding is mention_only (an adopted thread, where a
// bare reply does not route to the agent). Default-routing sessions are
// left untouched.
func maybeAppendMentionFooter(text, routingMode string) string {
	if routingMode != "mention_only" {
		return text
	}
	return text + "\n\n_@-mention me to reply._"
}

func (s *slackSender) Send(ctx context.Context, sess channelkinds.SessionInfo, env channelevents.Envelope) (channelkinds.SubChannelSendResult, error) {
	if s.client == nil {
		return channelkinds.SubChannelSendResult{}, errors.New("slack sender: bot-token missing from credentials Secret")
	}
	if sess.Channel == nil {
		return channelkinds.SubChannelSendResult{}, errors.New("slack sender: session has no channel binding")
	}

	channelID := sess.Channel.External["channel_id"]
	threadTS := effectiveOutboundThreadTS(sess)
	if channelID == "" {
		return channelkinds.SubChannelSendResult{}, errors.New("slack sender: external.channel_id missing on session.spec.channel")
	}
	// A binding with no thread_ts is a fresh channel (cron first-send, or a
	// restart-fork child whose thread_ts was cleared at fork time). Both need
	// write-back below; recorded before the fork-root guard below can mutate
	// threadTS to a non-empty root.
	needsWriteBack := threadTS == ""

	// A fork child has no thread yet. Create it with the link-back framing
	// message so the first thing anyone reads is where this came from — not a
	// status caption. forkRootTS is kept alongside the mutated threadTS so the
	// attachments and streaming-bubble branches below can report the root even
	// though their own "did I just create a root" checks (threadTS == "") no
	// longer fire once threadTS has been reassigned here.
	var forkRootTS string
	if threadTS == "" && s.forkRoot != nil {
		if root := s.forkRoot.ensure(ctx, s.client, s.deps.K8sClient, sess, channelID); root != "" {
			forkRootTS = root
			threadTS = root
		}
	}

	switch env.Kind {
	case channelevents.KindNotification:
		return channelkinds.SubChannelSendResult{}, s.sendNotification(ctx, sess, channelID, threadTS, env)
	case channelevents.KindTurnProgress:
		return channelkinds.SubChannelSendResult{}, s.sendTurnProgress(ctx, sess, channelID, threadTS, env)
	case channelevents.KindToolProgress:
		return channelkinds.SubChannelSendResult{}, s.sendToolProgress(ctx, sess, channelID, threadTS, env)
	case channelevents.KindOperationActivity:
		return channelkinds.SubChannelSendResult{}, s.sendOperationActivity(ctx, sess, channelID, threadTS, env)
	case channelevents.KindPlanUpdate:
		return s.sendPlanUpdate(ctx, sess, channelID, threadTS, env)
	case channelevents.KindUserMessage:
		// fall through to user_message path below
	default:
		// Reserved kinds (permission_request etc.) are not yet wired.
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("slack sender: unsupported envelope kind %q", env.Kind)
	}

	var pl channelevents.OutboundUserMessagePayload
	if err := json.Unmarshal(env.Payload, &pl); err != nil {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("slack sender: unmarshal payload: %w", err)
	}
	if pl.Text == "" {
		return channelkinds.SubChannelSendResult{}, errors.New("slack sender: empty text payload")
	}
	// LLMs occasionally emit HTML-escaped sequences (e.g. "&amp;") that
	// Slack would render literally; unescape so users see real "&".
	pl.Text = slackifyText(pl.Text)

	// Adopted (mention_only) thread: a bare reply does not route to the
	// agent, so append an @-mention hint. No-op for default-routing
	// sessions.
	pl.Text = maybeAppendMentionFooter(pl.Text, sess.Channel.RoutingMode)

	// Attachments: when the runner emitted refs to ArtifactRender CRs, fetch
	// the bytes and upload via files.uploadV2 (slack-go's UploadFileContext).
	// Text rides on the first upload's InitialComment so the user sees one
	// message + N files. Any fetch/upload error short-circuits and surfaces
	// to channelsd's relay, which republishes the failure on the dead-letter
	// path the standard text-only failures already use. This path supersedes
	// the streaming-bubble chat.update + chat.postMessage paths below — when
	// attachments are present the streaming bubble (if any) remains in-thread
	// until Slack auto-clears the assistant.threads setStatus indicator on the
	// first file landing; the visual blip is acceptable for v1 (most
	// attachment paths are DM, where no streaming bubble is posted).
	if len(pl.Attachments) > 0 {
		res, err := s.sendWithAttachments(ctx, sess, channelID, threadTS, pl)
		if err == nil && forkRootTS != "" && len(res.External) == 0 {
			// ensure() created this thread; sendWithAttachments only captures a
			// root it created itself. Report ours so the relay patches the
			// child's binding + LabelChannelKey.
			res = channelkinds.SubChannelSendResult{External: map[string]string{
				"thread_ts": forkRootTS, "channel_id": channelID,
			}}
		}
		return res, err
	}

	// Consult the stream sink for the streaming bubble ts. When streaming
	// occurred for this thread/turn, the sink lazily posted a bubble on the
	// first flushable delta and chat.update'd it for subsequent deltas. The
	// sender chat.updates that same bubble with the polished final reply so
	// the user sees one coherent message (not a separate reply below the
	// streaming text). ConsumeFinalTarget clears the ts so a follow-up turn
	// doesn't reuse a stale bubble.
	//
	// Falls through to a fresh chat.postMessage when:
	//   - no streaming occurred (DM/sidebar path, or sink not configured)
	//   - tool-session output was streamed (the bubble sits above those
	//     messages; appending below is cleaner than editing above them)
	logger := log.FromContext(ctx)
	var ts string
	if s.streamSink != nil {
		ts = s.streamSink.ConsumeFinalTarget(channelID, threadTS)
	}
	if ts != "" && s.toolSessionSeen != nil && s.toolSessionSeen.saw(sess.Namespace+"/"+sess.Name) {
		logger.Info("slack: tool output streamed; appending final reply instead of editing the streaming bubble",
			"session", sess.Name)
		ts = ""
	}
	if ts != "" {
		_, _, _, err := s.client.UpdateMessageContext(ctx, channelID, ts,
			slackapi.MsgOptionText(responseReadyEmoji+" "+pl.Text, false))
		if err == nil {
			// chat.update doesn't auto-clear assistant.threads.setStatus the
			// way chat.postMessage does. Explicitly clear so the "is
			// thinking…" indicator goes away once the user can see the
			// answer.
			logger.Info("slack: user_message via chat.update (streaming bubble consumed)",
				"session", sess.Name, "channelID", channelID, "bubbleTS", ts)
			s.clearStatus(ctx, sess, channelID, threadTS)
			s.forgetWatchdog(sess)
			// First-send capture: a streaming bubble posted into a fresh
			// channel (no inbound thread_ts) IS the thread root — mirrors the
			// chat.postMessage write-back below, since chat.update never
			// otherwise reports its ts back to channelsd's outbound relay. A
			// fork child's bubble instead posts UNDER the link-back root
			// ensure() created, so report THAT root, not the bubble's own ts —
			// needsWriteBack (captured before the fork-root guard could mutate
			// threadTS) is the right "is this a first send" test here, since
			// threadTS itself is no longer "" once the guard has run.
			if needsWriteBack {
				rootTS := forkRootTS
				if rootTS == "" {
					rootTS = ts // plain first-send: the bubble itself is the root
				}
				if rootTS != "" {
					return channelkinds.SubChannelSendResult{
						External: map[string]string{"thread_ts": rootTS, "channel_id": channelID},
					}, nil
				}
			}
			return channelkinds.SubChannelSendResult{}, nil
		}
		// chat.update genuinely failed (bubble deleted, message too old,
		// etc.). Falling through to chat.postMessage WILL produce a
		// duplicate if Slack actually accepted the update — older slack-go
		// surfaces non-fatal warnings as err, masking a successful API
		// call. Log loudly so we can see when this happens; the worst-of-
		// both is silent dual-render which we observed in production.
		logger.Info("slack: chat.update failed; falling through to chat.postMessage (may produce duplicate if update succeeded server-side)",
			"session", sess.Name, "channelID", channelID, "bubbleTS", ts, "error", err)
	}

	// Post the final reply as a chat.postMessage. Slack auto-clears the
	// assistant.threads.setStatus indicator when the bot's message lands.
	//
	// Block layout:
	//   1. Section block — the agent's reply text (mrkdwn).
	//   2. Context block (conditional) — muted clamp-warning line when the
	//      effective budget was clamped by policy (best-effort fetch).
	//   3. Action block (conditional) — "Show settings" button, included only
	//      on the session's first agent message; reachable anytime
	//      thereafter via the App Home "Agent settings" button.
	sessRef := sess.Namespace + "/" + sess.Name
	includeSettings := s.settingsSeen == nil || !s.settingsSeen.saw(sessRef)
	blocks := s.buildUserMessageBlocks(ctx, sess, pl.Text, sessRef, includeSettings, logger)
	opts := []slackapi.MsgOption{
		slackapi.MsgOptionText(pl.Text, false), // plain-text fallback for push notifications
		slackapi.MsgOptionBlocks(blocks...),
	}
	if threadTS != "" {
		opts = append(opts, slackapi.MsgOptionTS(threadTS))
	}

	usedBlocks := true
	postedChannelID, postedTS, err := s.client.PostMessageContext(ctx, channelID, opts...)
	if err != nil && isUnsupportedBlocksErr(err) {
		// The reply's Block Kit rendering was rejected (e.g. a single
		// unbreakable token longer than Slack's 3000-char section limit, or a
		// reply so long it overflowed the 50-block-per-message cap). Don't drop
		// the content: re-post as a plain-text message, which isn't subject to
		// block validation, so the user still receives the answer — degraded
		// (no Show-settings button), but delivered. Logged loudly so the
		// degraded path is visible in operator logs.
		logger.Info("slack: user_message blocks rejected; retrying as plain text (degraded, no Show-settings button)",
			"session", sess.Name, "channelID", channelID, "err", err.Error())
		usedBlocks = false
		plainOpts := []slackapi.MsgOption{slackapi.MsgOptionText(pl.Text, false)}
		if threadTS != "" {
			plainOpts = append(plainOpts, slackapi.MsgOptionTS(threadTS))
		}
		postedChannelID, postedTS, err = s.client.PostMessageContext(ctx, channelID, plainOpts...)
	}
	if err != nil {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("slack postMessage: %w", err)
	}
	// Mark the once-guard only after a successful block post: the
	// plain-text degraded retry above never carried the button, and a
	// failed post must not consume the session's one shot at it.
	if includeSettings && usedBlocks && s.settingsSeen != nil {
		s.settingsSeen.mark(sessRef)
	}
	logger.Info("slack: user_message via chat.postMessage",
		"session", sess.Name, "channelID", channelID, "threadTS", threadTS,
		"postedTS", postedTS)
	s.forgetWatchdog(sess)
	// First-send capture: when the inbound binding had no thread_ts (cron-
	// spawned session posting into a fresh thread, or a restart-fork child
	// whose thread_ts was cleared at fork time), a thread root now exists —
	// either the fork-root guard above created it, or (the plain cron case)
	// this chat.postMessage's own return-ts IS the new thread root. Hand it
	// back to channelsd so the outbound relay can patch the channel binding
	// and label on the session CR — subsequent agent posts will then thread
	// under the same root, and the inbound pipeline will find this session
	// when a user replies in-thread.
	if needsWriteBack && postedTS != "" {
		ch := postedChannelID
		if ch == "" {
			ch = channelID
		}
		rootTS := threadTS // set by the fork-root guard when this is a fork child
		if rootTS == "" {
			rootTS = postedTS // plain cron first-send: the reply itself is the root
		}
		return channelkinds.SubChannelSendResult{
			External: map[string]string{
				"thread_ts":  rootTS,
				"channel_id": ch,
			},
		}, nil
	}
	return channelkinds.SubChannelSendResult{}, nil
}

// buildUserMessageBlocks composes the Block Kit block set for the agent's
// final user_message chat.postMessage. Layout:
//
//  1. Section — the reply text.
//  2. Context (conditional) — muted "Budget capped by policy" line when the
//     session's effectiveSettings shows a budget clamp. Best-effort: a
//     K8sClient.Get failure is logged and skipped; the message always delivers.
//  3. Action (conditional) — "Show settings" button, included only when
//     includeSettings is true (the session's first agent message). Settings
//     stay reachable anytime via the App Home "Agent settings" button, so
//     the clamp line (when present) references whichever surface applies.
func (s *slackSender) buildUserMessageBlocks(
	ctx context.Context,
	sess channelkinds.SessionInfo,
	text, sessRef string,
	includeSettings bool,
	logger interface{ Info(string, ...any) },
) []slackapi.Block {
	// A section block's mrkdwn is capped at 3000 chars; a single unbounded
	// block makes chat.postMessage reject the whole reply with invalid_blocks.
	// Split the reply across as many section blocks as it needs, each under the
	// limit, preserving all content.
	var blocks []slackapi.Block
	for _, chunk := range chunkForSlackSection(text, slackSectionMaxRunes) {
		blocks = append(blocks, slackapi.NewSectionBlock(
			slackapi.NewTextBlockObject("mrkdwn", chunk, false, false),
			nil, nil,
		))
	}

	// Muted clamp-warning context line — best-effort; never blocks the send.
	if s.deps.K8sClient != nil {
		var as spiceboxv1alpha1.AgentSession
		if err := s.deps.K8sClient.Get(ctx, types.NamespacedName{
			Namespace: sess.Namespace, Name: sess.Name,
		}, &as); err != nil {
			logger.Info("slack: buildUserMessageBlocks: get session failed (best-effort, skipping clamp line)",
				"session", sess.Name, "err", err.Error())
		} else if isSettingsClamped(as.Status.EffectiveSettings) {
			clampText := "⚙︎ Budget capped by policy — tap _Show settings_ to see effective limits."
			if !includeSettings {
				clampText = "⚙︎ Budget capped by policy — see *Agent settings* in the app's Home tab for effective limits."
			}
			blocks = append(blocks, slackapi.NewContextBlock("",
				slackapi.NewTextBlockObject("mrkdwn", clampText, false, false),
			))
		}
	}

	// Show settings button — first agent message of the session only;
	// settings stay reachable anytime via the App Home "Agent settings"
	// button.
	if includeSettings {
		blocks = append(blocks, slackapi.NewActionBlock("show_settings_actions",
			settingsButton(sessRef),
		))
	}

	return blocks
}

// forgetWatchdog drops the watchdog's tracking for this session — used after
// a final user_message has been delivered (the user has their answer, no
// silence alarm needed). Safe with nil Deps.TouchSetStatus / nil hooks.
// Also drops any cached assistant.threads.setStatus payload — the agent
// has finished this turn and the caption is dismissed for good — and clears
// the in-flight tool set, tying its lifecycle to the same session-done signal
// so a lost Done tick (e.g. runner crash) can't leave a phantom entry forever.
func (s *slackSender) forgetWatchdog(sess channelkinds.SessionInfo) {
	if s.deps.ForgetSetStatus != nil {
		s.deps.ForgetSetStatus(sess.Namespace, sess.Name)
	}
	s.statusMu.Lock()
	delete(s.statusCache, sess.Namespace+"/"+sess.Name)
	s.statusMu.Unlock()
	s.toolProgMu.Lock()
	// Turn-boundary reset for in-flight tools; fires at reply-delivery/idle, not turn-start,
	// and backstops a lost Done tick (e.g., runner context cancel).
	delete(s.toolProgress, sess.Namespace+"/"+sess.Name)
	s.toolProgMu.Unlock()
}

// rememberStatus stores the most recent setStatus payload per session
// so it can be re-issued after Slack auto-clears the indicator (see
// cachedStatus). Concurrent-safe: notification and plan paths can
// race but writes commute (last-writer-wins is correct since each
// call already replaced the previous indicator on Slack's side).
func (s *slackSender) rememberStatus(sess channelkinds.SessionInfo, c cachedStatus) {
	s.statusMu.Lock()
	s.statusCache[sess.Namespace+"/"+sess.Name] = c
	s.statusMu.Unlock()
}

// recallStatus returns the most recent cached setStatus for the
// session, or zero value + false when none has been published.
func (s *slackSender) recallStatus(sess channelkinds.SessionInfo) (cachedStatus, bool) {
	s.statusMu.Lock()
	c, ok := s.statusCache[sess.Namespace+"/"+sess.Name]
	s.statusMu.Unlock()
	return c, ok
}

// restoreStatus re-issues SetAssistantThreadsStatusContext from
// cache. Used by code paths that just performed a chat.postMessage
// in the assistant thread — Slack auto-clears the indicator on
// every such post and we want it back. Best-effort: errors are
// logged and swallowed; the worst case is the indicator stays
// blank until the next update_status, which matches today's
// behavior.
func (s *slackSender) restoreStatus(ctx context.Context, sess channelkinds.SessionInfo) {
	c, ok := s.recallStatus(sess)
	if !ok || c.threadTS == "" {
		return
	}
	if err := s.client.SetAssistantThreadsStatusContext(ctx, slackapi.AssistantThreadsSetStatusParameters{
		ChannelID:       c.channelID,
		ThreadTS:        c.threadTS,
		Status:          c.status,
		LoadingMessages: animatedLoadingMessages(c.loading),
	}); err != nil {
		log.FromContext(ctx).Info("slack: setStatus restore failed (best-effort, continuing)",
			"error", err, "session", sess.Name, "channelID", c.channelID)
	}
}

// clearStatus pushes an empty assistant.threads.setStatus call to dismiss the
// indicator after the agent's final reply lands. Slack auto-clears on
// chat.postMessage but not on chat.update; an explicit setStatus("") covers
// the chat.update path and is harmless on chat.postMessage paths.
// fallbackThreadTS is consulted when the K8s annotation lookup yields
// nothing — e.g., DM threads where setStatus uses the assistant-thread
// ts the caller resolved up front.
// Best-effort: errors are logged and swallowed.
func (s *slackSender) clearStatus(ctx context.Context, sess channelkinds.SessionInfo, channelID, fallbackThreadTS string) {
	threadTS := s.resolveThreadTS(ctx, sess, fallbackThreadTS)
	if threadTS == "" {
		return
	}
	if err := s.client.SetAssistantThreadsStatusContext(ctx, slackapi.AssistantThreadsSetStatusParameters{
		ChannelID:       channelID,
		ThreadTS:        threadTS,
		Status:          "",
		LoadingMessages: []string{}, // symmetric clear: dismiss the loading caption too
	}); err != nil {
		log.FromContext(ctx).Info("slack: clearStatus failed (best-effort, continuing)",
			"error", err, "session", sess.Name, "channelID", channelID)
	}
}

// sendWithAttachments resolves each AttachmentRef to its bytes via the
// AssetFetcher, then uploads to Slack via files.uploadV2 (slack-go's
// UploadFileContext). files.uploadV2 returns no message ts, so an upload
// alone can never establish a thread root: once at least one attachment is
// confirmed uploadable, and threadTS == "" (a fresh channel or a fork child
// whose thread_ts was cleared), the text is posted via chat.postMessage
// FIRST to create the root, and the uploads carry an empty InitialComment
// (the text already landed — never duplicate it). When threadTS != "", the
// in-thread shape is unchanged: text rides on the first upload's
// InitialComment, one message, no extra post. The root is created only once
// we know an upload will actually be attempted — the AssetFetcher-missing
// and every-fetch-failed fallbacks below collapse to a single
// chat.postMessage either way, so pre-creating a root for them would just
// duplicate that post.
//
// Best-effort delivery: when one or more attachment fetches or uploads
// fail, the function falls back to a plain chat.postMessage carrying the
// text plus a footer listing the failed attachments. The user always sees
// the agent's reply text — silent message loss is the worst outcome,
// dropped attachments with a visible error indicator is acceptable. The
// returned SubChannelSendResult carries the newly-created root (empty when
// none was created) on every return path from that point on, including the
// upload-failure fallback — a fork child must not lose its captured root
// just because an upload failed.
func (s *slackSender) sendWithAttachments(
	ctx context.Context,
	sess channelkinds.SessionInfo,
	channelID, threadTS string,
	pl channelevents.OutboundUserMessagePayload,
) (channelkinds.SubChannelSendResult, error) {
	logger := log.FromContext(ctx)
	var failures []string
	var res channelkinds.SubChannelSendResult

	if s.deps.AssetFetcher == nil {
		logger.Info("slack sender: no AssetFetcher configured; falling back to text-only",
			"session", sess.Name, "attachments", len(pl.Attachments))
		for _, att := range pl.Attachments {
			failures = append(failures, fmt.Sprintf("%s (no asset fetcher configured)", att.RenderName))
		}
		// Reached before any thread root is created, so the text is still unposted.
		return res, s.postFallbackText(ctx, sess, channelID, threadTS, pl, failures, false)
	}

	type fetched struct {
		att channelevents.AttachmentRef
		ab  channelkinds.AssetBytes
	}
	got := make([]fetched, 0, len(pl.Attachments))
	for _, att := range pl.Attachments {
		ab, err := s.deps.AssetFetcher.Fetch(ctx, sess.Namespace, sess.Name, att.RenderName)
		if err != nil {
			logger.Info("slack sender: fetch attachment failed",
				"session", sess.Name, "render", att.RenderName, "err", err.Error())
			failures = append(failures, fmt.Sprintf("%s (fetch failed)", att.RenderName))
			continue
		}
		got = append(got, fetched{att: att, ab: ab})
	}

	if len(got) == 0 {
		// Every attachment failed; deliver the text plus a footer. Also reached
		// before the thread root is created, so the text is still unposted.
		return res, s.postFallbackText(ctx, sess, channelID, threadTS, pl, failures, false)
	}

	// At least one attachment will be uploaded. files.uploadV2 returns no
	// message ts, so an upload can never establish a thread root itself —
	// create it now via chat.postMessage when none exists yet.
	initialComment := pl.Text
	if threadTS == "" {
		_, rootTS, err := s.client.PostMessageContext(ctx, channelID, slackapi.MsgOptionText(pl.Text, false))
		if err != nil {
			return res, fmt.Errorf("slack postMessage (attachment thread root): %w", err)
		}
		threadTS = rootTS
		initialComment = "" // already posted; do not duplicate
		res = channelkinds.SubChannelSendResult{
			External: map[string]string{"thread_ts": rootTS, "channel_id": channelID},
		}
	}

	initialText := initialComment
	if len(failures) > 0 {
		initialText = initialComment + "\n\n_⚠ Some attachment(s) couldn't be delivered: " +
			strings.Join(failures, ", ") + ". See channelsd logs for details._"
	}
	for i, item := range got {
		// Prefer the runner-supplied filename hint over the fetcher's
		// Content-Disposition copy when available — the runner already
		// echoed it from the ArtifactRender CR's status.outputFilename. The
		// one exception: the fetcher now hits the bundle route
		// (internal/cmd/channelsd/internal/assetfetch), which zips an html primary
		// server-side when it has resolvable artifact: refs — a decision
		// made AFTER the runner minted its hint, at respond_to_user call
		// time. When that happened, item.ab.MIME is application/zip even
		// though the hint still names the un-zipped primary (e.g.
		// "report.html"); the fetcher's own filename must win so Slack sees
		// bytes and filename that actually agree.
		filename := item.att.Filename
		if item.ab.MIME == "application/zip" {
			filename = item.ab.Filename
		}
		if filename == "" {
			filename = item.ab.Filename
		}
		params := slackapi.UploadFileParameters{
			Reader:          bytes.NewReader(item.ab.Bytes),
			FileSize:        len(item.ab.Bytes),
			Filename:        filename,
			Channel:         channelID,
			ThreadTimestamp: threadTS,
			AltTxt:          item.att.AltText,
		}
		// Only the first upload carries the message text; subsequent
		// uploads must not repeat it or the user sees N copies of the
		// reply line in-thread.
		if i == 0 {
			params.InitialComment = initialText
		}
		if _, err := s.client.UploadFileContext(ctx, params); err != nil {
			if i == 0 {
				// First upload failed. In-thread, nothing user-facing has
				// landed yet. On a first send, the root text already landed
				// via the chat.postMessage above — either way, fall back to
				// a plain chat.postMessage carrying the failure footer,
				// threaded into (possibly newly created) threadTS, and keep
				// returning res so the caller doesn't lose the captured root.
				logger.Info("slack sender: first uploadFileV2 failed; falling back to text-only",
					"session", sess.Name, "render", item.att.RenderName, "err", err.Error())
				failures = append(failures, fmt.Sprintf("%s (upload failed)", item.att.RenderName))
				for _, remaining := range got[1:] {
					failures = append(failures, fmt.Sprintf("%s (upload skipped)", remaining.att.RenderName))
				}
				// On a first send the thread root above already carried pl.Text
				// (initialComment was emptied to avoid duplicating it there), so
				// the fallback must add only the failure notice.
				return res, s.postFallbackText(ctx, sess, channelID, threadTS, pl, failures, initialComment == "")
			}
			// Subsequent upload failed; the first upload + InitialComment
			// already landed. Re-posting the text would duplicate the
			// user's view; record and continue.
			logger.Info("slack sender: subsequent uploadFileV2 failed; partial delivery accepted",
				"session", sess.Name, "render", item.att.RenderName, "err", err.Error())
			failures = append(failures, fmt.Sprintf("%s (upload failed mid-batch)", item.att.RenderName))
		}
	}
	logger.Info("slack: user_message via files.uploadV2",
		"session", sess.Name, "channelID", channelID, "threadTS", threadTS,
		"attachments", len(got), "failed_attachments", len(failures))
	s.forgetWatchdog(sess)
	return res, nil
}

// postFallbackText posts pl.Text with a delivery-failure footer as a plain
// chat.postMessage. Used when attachment fetch / upload fails — the agent's
// reply still reaches the user along with an indicator that something went
// wrong (operators can grep channelsd logs for the underlying cause).
// Errors here surface to the relay; the alternative is silent message loss.
//
// textAlreadyPosted is true when a first send has already created the thread
// root carrying pl.Text. The fallback then contributes only the failure notice:
// repeating the text beneath it would show the user their reply twice.
func (s *slackSender) postFallbackText(
	ctx context.Context, sess channelkinds.SessionInfo, channelID, threadTS string,
	pl channelevents.OutboundUserMessagePayload, failures []string, textAlreadyPosted bool,
) error {
	text := pl.Text
	if textAlreadyPosted {
		text = ""
	}
	if len(failures) > 0 {
		footer := "_⚠ Attachment delivery failed: " +
			strings.Join(failures, ", ") + ". See channelsd logs for details._"
		if text == "" {
			text = footer
		} else {
			text += "\n\n" + footer
		}
	}
	opts := []slackapi.MsgOption{slackapi.MsgOptionText(text, false)}
	if threadTS != "" {
		opts = append(opts, slackapi.MsgOptionTS(threadTS))
	}
	if _, _, err := s.client.PostMessageContext(ctx, channelID, opts...); err != nil {
		return fmt.Errorf("slack postMessage (attachment fallback): %w", err)
	}
	log.FromContext(ctx).Info("slack: user_message via chat.postMessage (attachment fallback)",
		"session", sess.Name, "channelID", channelID, "threadTS", threadTS,
		"failed_attachments", len(failures))
	s.forgetWatchdog(sess)
	return nil
}

// sendNotification handles a notification envelope (mid-flight status update
// from the agent). Calls assistant.threads.setStatus so the native Slack AI
// "thinking" indicator updates in place. Best-effort: if setStatus fails (e.g.
// missing assistant:write scope, DM without an accepted thread_ts), the error
// is logged and swallowed — the user will see silence until the real reply
// lands, which is acceptable.
func (s *slackSender) sendNotification(ctx context.Context, sess channelkinds.SessionInfo, channelID, threadTS string, env channelevents.Envelope) error {
	var pl channelevents.NotificationPayload
	if err := json.Unmarshal(env.Payload, &pl); err != nil {
		return fmt.Errorf("slack sender: unmarshal notification payload: %w", err)
	}
	if pl.Text == "" {
		// Sentinel: an empty Notification means "tear down the indicator
		// and stop tracking this session." channelsd publishes this when
		// a session transitions to Idle without a respond_to_user (the
		// runner's no-tool-use recovery path) so a stuck "⚠️ Taking
		// longer" status from an earlier alarm doesn't sit there forever.
		s.clearStatus(ctx, sess, channelID, threadTS)
		s.forgetWatchdog(sess)
		return nil
	}
	// Unescape HTML entities the LLM may have emitted (`&amp;`, `&lt;`,
	// etc.). Slack would render those literally; users want the real
	// glyphs.
	pl.Text = slackifyText(pl.Text)
	pl.Short = slackifyText(pl.Short)

	// Size guard. Some callers send a caption verbatim with no regard for
	// Slack's surface widths — notably update_plan, which mirrors a plan's
	// in_progress item label (up to 200 chars) into the status indicator.
	// A caption past the thread-top's clean-render width looks broken, so
	// truncate the real text to fit (rune-aware, with an ellipsis) rather
	// than swapping in a generic placeholder — a generic "Working on the
	// current step…" is more confusing than a clipped-but-real caption,
	// because it hides which step the agent is actually on. Well-formed
	// update_status captions (≤100, with their own Short) stay under this
	// and pass through untouched. pl.Short is left as the caller set it; the
	// loading-message logic below fits it independently.
	if utf8.RuneCountInString(pl.Text) > statusMaxRunes {
		pl.Text = fitRunes(pl.Text, statusMaxRunes)
	}

	// Resolve the effective thread_ts: prefer the LastInboundTS annotation
	// (always fresh, reflects the most recent inbound) and fall back to
	// External["thread_ts"] for threaded sessions that predate the annotation.
	effectiveThreadTS := s.resolveThreadTS(ctx, sess, threadTS)

	// Set both Status and LoadingMessages so the thread-top indicator and
	// Slack's bottom-of-channel typing indicator render the same text.
	// Without LoadingMessages, Slack rotates the bottom indicator through
	// built-in defaults and the two surfaces diverge. The caption is fanned
	// out across spinner frames (see animatedLoadingMessages) so Slack's own
	// rotation animates the bottom surface between our setStatus calls.
	// Pick the loading-message caption. Prefer the agent-supplied Short
	// verbatim when it fits the API's <51-char limit; otherwise truncate
	// the full Text so the channel surface still has a label rather than
	// rotating Slack's built-in defaults.
	loading := fitLoadingMessage(pl.Text)
	if pl.Short != "" {
		if utf8.RuneCountInString(pl.Short) <= loadingMessageMaxRunes {
			loading = pl.Short
		} else {
			loading = fitLoadingMessage(pl.Short)
		}
	}
	// The agent published this status: that is forward progress, and any
	// expected-duration hint it carries is the agent's own claim. Both are
	// independent of whether Slack renders the indicator, so apply them BEFORE
	// the cosmetic call. Gating them on it let a thread Slack refuses to
	// setStatus on (invalid_thread_ts, e.g. a non-assistant thread) starve the
	// silence clock, tripping a bogus "appears to have stalled" notice on a
	// perfectly healthy agent — once per user message.
	if s.deps.TouchSetStatus != nil {
		s.deps.TouchSetStatus(sess.Namespace, sess.Name)
	}
	if pl.ExpectedDurationSeconds > 0 && s.deps.ExtendSetStatus != nil {
		s.deps.ExtendSetStatus(sess.Namespace, sess.Name, time.Duration(pl.ExpectedDurationSeconds)*time.Second)
	}
	if err := s.client.SetAssistantThreadsStatusContext(ctx, slackapi.AssistantThreadsSetStatusParameters{
		ChannelID:       channelID,
		ThreadTS:        effectiveThreadTS,
		Status:          pl.Text,
		LoadingMessages: animatedLoadingMessages(loading),
	}); err != nil {
		log.FromContext(ctx).Info("slack: setStatus failed (best-effort, continuing)",
			"error", err,
			"session", sess.Name,
			"channelID", channelID,
			"threadTS", effectiveThreadTS,
			"fallbackThreadTS", threadTS,
			"statusLen", len(pl.Text),
			"statusPreview", truncateForLog(pl.Text, 80),
		)
		return nil
	}
	// Cache the most recent status so subsequent chat.postMessage
	// paths (notably the first plan_update post) can re-issue it
	// after Slack auto-clears the indicator. Success-only: the cache
	// mirrors what Slack is actually displaying.
	s.rememberStatus(sess, cachedStatus{
		channelID: channelID, threadTS: effectiveThreadTS,
		status: pl.Text, loading: loading,
	})
	return nil
}

// sendOperationActivity renders a KindOperationActivity envelope onto the
// Slack assistant status line. By the time the envelope reaches this sender,
// the channelsd relay's watchdog machine has already resolved
// payload.CompactLine to its EffectiveLine: on a normal tick that's the
// runner-derived "op ‣ reason" line; on a Cleared/revert tick it's the
// remembered update_status caption, or "" when there is genuinely nothing to
// show. Render-only — the watchdog's silence timer was already re-armed by
// the relay's fold, so this does not touch TouchSetStatus/ExtendSetStatus.
//
// Unlike sendNotification's empty-Text sentinel (which tears the indicator
// down and forgets the watchdog — used when a turn truly ends with no
// reply), an empty CompactLine here is a plain no-op: it means "nothing to
// show yet," not "clear what's showing." Tearing down on a bare empty tick
// would blank a perfectly good in-flight caption.
func (s *slackSender) sendOperationActivity(ctx context.Context, sess channelkinds.SessionInfo, channelID, threadTS string, env channelevents.Envelope) error {
	var pl channelevents.OperationActivityPayload
	if err := json.Unmarshal(env.Payload, &pl); err != nil {
		return fmt.Errorf("slack sender: unmarshal operation_activity payload: %w", err)
	}
	line := pl.CompactLine
	if line == "" {
		return nil
	}
	// Same size guard as sendNotification: truncate the real line to fit
	// Slack's clean-render width rather than dropping it or swapping in a
	// generic placeholder.
	line = fitRunes(line, statusMaxRunes)

	effectiveThreadTS := s.resolveThreadTS(ctx, sess, threadTS)
	// Skip the API call when the indicator already shows exactly this line:
	// the runner re-ticks activity every few seconds, and a redundant
	// setStatus replaces loading_messages — restarting Slack's client-side
	// spinner rotation, which shows up as the spinner flashing backwards.
	// Safe against a cleared indicator: the cache is success-only and every
	// teardown path drops it (forgetWatchdog), so "cached == line" means the
	// line is actually showing.
	if cached, ok := s.recallStatus(sess); ok &&
		cached.channelID == channelID && cached.threadTS == effectiveThreadTS && cached.status == line {
		return nil
	}
	loading := fitLoadingMessage(line)
	if err := s.client.SetAssistantThreadsStatusContext(ctx, slackapi.AssistantThreadsSetStatusParameters{
		ChannelID:       channelID,
		ThreadTS:        effectiveThreadTS,
		Status:          line,
		LoadingMessages: animatedLoadingMessages(loading),
	}); err != nil {
		log.FromContext(ctx).Info("slack: operation_activity setStatus failed (best-effort, continuing)",
			"error", err,
			"session", sess.Name,
			"channelID", channelID,
			"threadTS", effectiveThreadTS,
			"statusLen", len(line),
			"statusPreview", truncateForLog(line, 80),
		)
		return nil
	}
	// Cache the same way sendNotification does, so a subsequent
	// chat.postMessage that auto-clears the indicator re-issues this line
	// rather than the last update_status caption.
	s.rememberStatus(sess, cachedStatus{
		channelID: channelID, threadTS: effectiveThreadTS,
		status: line, loading: loading,
	})
	return nil
}

// resolveThreadTS returns the best available thread_ts for setStatus calls.
// It reads LastInboundTSAnnotationKey from the AgentSession (always set on
// every inbound, both DM and threaded) and falls back to the External value
// passed by the caller.
func (s *slackSender) resolveThreadTS(ctx context.Context, sess channelkinds.SessionInfo, fallback string) string {
	if s.deps.K8sClient == nil {
		return fallback
	}
	var as spiceboxv1alpha1.AgentSession
	if err := s.deps.K8sClient.Get(ctx, types.NamespacedName{
		Namespace: sess.Namespace, Name: sess.Name,
	}, &as); err != nil {
		return fallback
	}
	if ts := as.Annotations[LastInboundTSAnnotationKey]; ts != "" {
		return ts
	}
	return fallback
}

// truncateForLog returns at most n bytes of s, suffixed with "…" when
// truncation occurred. Safe for non-secret diagnostic strings.
func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// responseReadyEmoji is the leading glyph chat.update writes onto the
// listener-posted placeholder when delivering the agent's first concrete
// reply. The placeholder is initially "💭" (thought_balloon, "still
// thinking"); chat.update transforms the message to
// "<responseReadyEmoji> <reply text>" so the visual shift from
// "thinking" to "answering" is obvious in-thread.
const responseReadyEmoji = "💬"

// loadingMessageMaxRunes is Slack's hard limit on each
// assistant.threads.setStatus#loading_messages[i] entry. Per the API the
// value must be "less than 51 characters" — verified by direct curl
// against the API (returns invalid_arguments + the json-pointer to the
// offending entry when exceeded).
const loadingMessageMaxRunes = 50

// statusMaxRunes bounds the thread-top assistant.threads.setStatus#status
// caption. Slack imposes no documented hard cap here, but ~100 runes is the
// width that renders cleanly (see NotificationPayload.Text); sendNotification
// truncates longer captions — e.g. a verbatim plan in_progress label — to this
// width (rune-aware, with an ellipsis) so the real text is preserved rather
// than replaced by a generic placeholder.
const statusMaxRunes = 100

// genericStatusCaption is the placeholder the turn-progress surface shows when
// there is no cached caption to wrap (sendTurnProgress, before the agent's
// first update_status). It is NOT used as an over-length fallback — that path
// truncates the real caption instead (see sendNotification). Kept short enough
// to also fit the loading_messages surface, so both Slack status surfaces stay
// consistent.
const genericStatusCaption = "Working on the current step…"

// loadingSpinnerFrames is the spinner glyph set baked into the
// loading_messages array. Slack's client rotates through the array on its
// own — no further setStatus calls — so prefixing each entry with a frame
// yields an animated spinner with zero API-rate-limit pressure, which is
// what makes a unicode spinner affordable on this surface at all. But Slack
// documents NO order or interval for that rotation, and every setStatus
// replaces the array (restarting the rotation wherever Slack pleases), so a
// sequential spinner (⠋⠙⠹…) renders out of order and visibly jumps
// backwards. Four half-circles at 90° steps read as rotation across ANY
// pairwise transition, making the animation order-proof.
var loadingSpinnerFrames = []string{"◐", "◓", "◑", "◒"}

// animatedLoadingMessages fans caption out into one loading_messages entry
// per spinner frame ("<frame> <caption>"). The caption is fitted once, up
// front, to the per-entry rune limit minus the frame + separator, so every
// entry carries identical text and only the leading glyph varies — Slack's
// rotation then reads as animation rather than as changing messages.
func animatedLoadingMessages(caption string) []string {
	capt := fitRunes(caption, loadingMessageMaxRunes-2) // 2 = frame rune + space
	msgs := make([]string, len(loadingSpinnerFrames))
	for i, f := range loadingSpinnerFrames {
		msgs[i] = f + " " + capt
	}
	return msgs
}

// fitLoadingMessage returns s truncated to fit Slack's loading_messages
// rune limit. If s already fits, it's returned unchanged; otherwise we
// keep the first (loadingMessageMaxRunes-1) runes and append "…".
// Truncation is rune-aware so we don't split multi-byte characters.
func fitLoadingMessage(s string) string {
	runes := []rune(s)
	if len(runes) <= loadingMessageMaxRunes {
		return s
	}
	return string(runes[:loadingMessageMaxRunes-1]) + "…"
}

// slackifyText prepares LLM-emitted text for Slack rendering. The
// canonical issue: LLMs occasionally emit HTML-escaped sequences
// (`&amp;`, `&lt;`, `&gt;`, `&quot;`) inside their plain-text output —
// especially under instructions to "be safe" with mark-up — and Slack
// renders those literally rather than as the original `&`, `<`, `>`,
// `"`. Unescaping at this boundary keeps statuses and replies readable
// without forcing every agent prompt to know about HTML entities.
func slackifyText(s string) string {
	return html.UnescapeString(s)
}

// PostEphemeral renders a "visible only to the would-be poster" reply.
// Used by the listener for SpiceDB denials. Not part of the Sender interface
// because ephemeral isn't NATS-driven; the listener calls it directly when
// the inbound pipeline returns OutcomeDeniedByPermission.
func PostEphemeral(ctx context.Context, c slackClient, channelID, userID, text string) error {
	if c == nil {
		return errors.New("slack: client not configured")
	}
	_, err := c.PostEphemeralContext(ctx, channelID, userID, slackapi.MsgOptionText(text, false))
	return err
}

// OpenIM resolves a Slack user_id to a DM channel ID (e.g., "D01234..."),
// opening the conversation if it doesn't exist. Used by the listener at
// DM session creation time to record the DM channel_id on session.spec.channel.external.
func OpenIM(ctx context.Context, c slackClient, userID string) (string, error) {
	if c == nil {
		return "", errors.New("slack: client not configured")
	}
	ch, _, _, err := c.OpenConversationContext(ctx, &slackapi.OpenConversationParameters{
		Users: []string{userID},
	})
	if err != nil {
		return "", fmt.Errorf("conversations.open: %w", err)
	}
	return ch.ID, nil
}

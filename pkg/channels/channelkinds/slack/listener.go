// pkg/channels/channelkinds/slack/listener.go
//
// Slack inbound: Socket Mode subscription that translates Slack events into
// channelkinds.InboundEvent + invokes the shared pipeline.
//
// Routing rules (spec §6.5):
//
//	app_mention                        → always handled (new or existing session)
//	message.im                         → always handled (per-user DM session)
//	message.channels|message.groups    → only when thread_ts matches a known thread
//	                                     (app_mention dedup via event-id LRU)
//
// Channel-key formats:
//
//	#channel @mention:  thread:<channel_id>:<thread_ts || message_ts>
//	DM:                 dm:<user_id>
package slack

import (
	"container/list"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	slackapi "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelfeatures"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkey"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/x/besteffort"
)

// listenerEventCacheSize is the LRU capacity for recent event dedup keys.
// app_mention and message.channels for the same Slack message have different
// event_ids but share (channel, ts); we dedup on a synthetic
// "<type>:<channel>:<ts>" key so both paths converge to a single delivery.
//
// LastInboundTSAnnotationKey records the effective thread_ts of the most
// recent inbound on the AgentSession. Stamped by the listener on every inbound
// (threaded and DM alike). The Slack sender reads this to resolve the
// thread_ts for assistant.threads.setStatus calls; the annotation is always
// fresh because it is updated on every inbound.
const LastInboundTSAnnotationKey = "slack.agentprimitives.authzed.com/last-inbound-ts"

// LastInboundCanonicalIDAnnotationKey records the canonicalID of the user who
// sent the most recent inbound message. Read by the runner's authz.Check at
// tool-dispatch time when AgentClass.spec.toolAuthSubject is currentRequester
// (the default). Stamped on every inbound alongside LastInboundTS. Empty
// string means no canonical ID was available for the inbound (kubectl-driven
// sessions, legacy paths).
const LastInboundCanonicalIDAnnotationKey = "slack.agentprimitives.authzed.com/last-inbound-canonical-id"

const listenerEventCacheSize = 1024

// listenerIdentityCacheSize bounds the users.info LRU inside the listener.
const listenerIdentityCacheSize = 256

// slackListener implements channelkinds.Listener via Slack Socket Mode.
type slackListener struct {
	deps     channelkinds.Deps
	api      listenerAPIClient // Web API: auth.test, users.info, postEphemeral
	src      socketSource      // Socket Mode event source (nil until Start)
	idents   *IdentityCache    // user_id → email/team LRU
	seenEvts *eventIDCache     // dedup keys (app_mention vs message.*)
	threads  *threadIndex      // known "<channel>:<thread_ts>" strings

	// assistantThreads remembers the canonical thread_ts that Slack assigns
	// to each AI-assistant thread when the user opens the agent container or
	// DMs the bot. Keyed by "<channel_id>:<user_id>". Populated by
	// assistant_thread_started events; consumed by handleDM to pass the
	// correct thread_ts to assistant.threads.setStatus (Slack rejects any
	// other ts as invalid_thread_ts). In-memory only — Slack re-fires
	// assistant_thread_started on reconnect / new conversations, so a
	// channelsd restart doesn't lose user-visible context.
	assistantThreads   map[string]string
	assistantThreadsMu sync.Mutex

	// botUserID resolved from auth.test; overridable via Channel.Spec.Slack.BotUserID.
	botUserID string

	// crossAgentEnabled admits ANOTHER agent's messages into the pipeline.
	// Off by default, and it never admits our OWN posts in any state — see
	// crossagent.go, where the two are separated precisely so this flag cannot
	// open the self-loop.
	//
	// Resolved from the bound AgentClass's spec.crossAgentThreads.enabled on
	// the reconcile tick (refreshCrossAgent), under scopesMu, so an operator
	// turning it on or off — or rebinding the Channel to a different class —
	// takes effect without restarting channelsd. FALSE until a class has been
	// read successfully: the default before any answer must be the closed one,
	// because the risk here is admitting, not refusing.
	crossAgentEnabled bool

	// scopesMu guards grantedScopeHeader, grantedScopesKnown, the
	// grantedScopes re-read bookkeeping and the scopeFeatures* cache. Start
	// writes them on the listener's own goroutine, channelsd's reconcile tick
	// reads them from another (RefreshScopes), and the out-of-band re-read
	// writes them from a third — real concurrent access, not a theoretical one.
	scopesMu sync.Mutex

	// grantedScopeHeader is the X-OAuth-Scopes value from the last successful
	// auth.test. Cached so ScopesValid can be recomputed whenever the bound
	// AgentClass's capabilities change, without another Slack API call.
	grantedScopeHeader string

	// grantedScopesCheckedAt is when that header was last obtained from Slack,
	// or last ATTEMPTED. It throttles the re-read that notices scopes granted
	// (or removed) by re-installing the Slack app — see
	// scopeHeaderRecheckMissing in scopes.go for why the re-read exists and
	// why it is throttled at two different rates.
	grantedScopesCheckedAt time.Time

	// grantedScopesRechecking is set while a re-read is in flight, so
	// consecutive 5s ticks cannot pile overlapping auth.test calls onto a slow
	// or hanging Slack.
	grantedScopesRechecking bool

	// grantedScopesKnown records that auth.test has answered at least once.
	// It is what separates "Slack told us the token grants nothing" (worth
	// reporting — Slack sets the header on every successful response) from
	// "we have not asked yet" (nothing to say). Without it, an empty header
	// would be indistinguishable from an unstarted listener and would stamp a
	// false failure onto a healthy Channel.
	grantedScopesKnown bool

	// scopeFeatures caches the channel features resolved from the bound
	// AgentClass, with when it was read and which AgentClass name it was read
	// for. This is a TTL cache in front of the API server, NOT a record of what
	// was written: the condition itself is always compared against the live
	// object. See agentClassRefreshInterval for why the read is throttled and
	// scopeFeaturesFor for why a rebind bypasses the TTL.
	scopeFeatures    []channelfeatures.Feature
	scopeFeaturesAt  time.Time
	scopeFeaturesFor string

	// crossAgentWakeBudget is the bound class's declared budget of
	// agent-driven wakes per human turn, carried to the pipeline on each
	// admitted cross-agent inbound. Resolved beside crossAgentEnabled, from
	// the same class read, so the two can never describe different classes.
	crossAgentWakeBudget int

	// sawAppMention records whether this listener has EVER received an
	// app_mention event. A top-level channel @-mention that lands in the
	// message.channels handler (not app_mention) is normal when app_mention
	// works (it's the deduped duplicate); it only signals a missing
	// app_mention subscription when no app_mention has ever arrived. Set once,
	// read on the drop path — atomic so the socketmode goroutines don't race.
	sawAppMention atomic.Bool

	// appMentionHintGrace is how long scheduleMissingAppMentionHint waits for a
	// racing app_mention before it concludes the subscription is missing. A
	// channel @-mention makes Slack emit BOTH app_mention and message.channels
	// with no guaranteed delivery order, so a snapshot of sawAppMention on the
	// message.channels path can be a false negative until the matching
	// app_mention is processed. Zero uses defaultAppMentionHintGrace; tests set
	// a small value.
	appMentionHintGrace time.Duration

	// installedTeamID is the Slack workspace (team) ID that this bot is
	// installed into, resolved from auth.test at Start time. It gates
	// email-as-identity: only full members of this workspace are trusted for
	// email-based SpiceDB subjects; guests and foreign-workspace users fall
	// back to the unforgeable slack:<team>:<user> synthetic.
	installedTeamID string

	// metaagentApprovalRefs is the process-wide shared cache of
	// in-flight metaagent scope-approval requests. Populated by the
	// metaagent scope-approval sender; the Show Details handler reads
	// it. nil when the listener is constructed standalone.
	metaagentApprovalRefs *metaagentApprovalRefCache

	// starters is the process-wide shared cache of "started" bootstrap
	// message coordinates (ChannelID, MessageTS, AgentName, StartedUnix),
	// keyed by "<sessionNS>/<sessionName>". Written by postStarter after a
	// successful post; read by the thread_title sender to edit the starter
	// in place when emulating a thread title. nil when the listener is
	// constructed standalone (e.g. most unit tests) — postStarter guards
	// against a nil starters before writing.
	starters *starterCache

	// interactionDelivery is the process-wide shared cache of in-flight
	// generic Interaction-model deliveries (interaction_delivery.go),
	// written by interactionSender.sendRequest at prompt-delivery time. The
	// generic Show-Details handler (interaction_details.go's
	// handleInteractionDetailsAction) reads it for the request's cached
	// Details; a miss falls back to the durable memapproval record. nil
	// when the listener was constructed standalone (e.g. unit tests that
	// don't exercise the generic Show-Details path).
	interactionDelivery *interactionDeliveryStore

	// restartMem is the memory.Memory facade used by the "Restart from here"
	// shortcut to resolve channel_msg_ref entries and count turns to discard.
	// Nil when channelsd is started without restart memory configured; the
	// shortcut handler shows an error modal in that case rather than panicking.
	restartMem memory.Memory

	// preferences gives the App Home preferences pane first-party read/write
	// access to a verified user's own preferences. Nil when channelsd is
	// started without a preferences client configured; the pane degrades to
	// a "couldn't load" state in that case rather than panicking.
	preferences channelkinds.PreferencesClient

	// personalizableClasses answers "which AgentClasses has this canonical
	// user interacted with", gating which App Home cards get a preferences
	// section at all. Nil when channelsd is started without a SpiceDB
	// client configured; the tab then renders every card with no
	// preferences section, same as a user who has interacted with nothing.
	personalizableClasses channelkinds.PersonalizableClassLookup

	// restartSubmit is the channelsd-provided function that publishes a
	// RestartTrigger envelope to NATS. Wired via RegisterRestartTrigger
	// (channelkinds.RestartCapable). Nil until wired; handleRestartSubmit
	// returns an error rather than panicking when nil.
	restartSubmit channelkinds.RestartSubmitFunc

	// stop is closed by Stop() to signal the run goroutine to exit.
	stop chan struct{}

	// now returns the current time. Defaults to time.Now; overridable in tests
	// to freeze the clock and assert the starter message format.
	now func() time.Time

	// metaagentBotUserID is the Slack user-id of the metaagent bot
	// (authzd). When non-empty, any app_mention event whose text
	// contains <@metaagentBotUserID> is routed to the metaagent
	// instead of the session's runner. Set via SetMetaagentBotUserID;
	// read at listener start from METAAGENT_SLACK_BOT_USER_ID env.
	metaagentBotUserID string

	// responseURLPoster POSTs a JSON body to a Slack interactive
	// response_url. Defaults (when nil) to postToResponseURL with
	// http.DefaultClient; tests inject a recorder so the user-facing
	// failure surface raised when a click's NATS publish fails is
	// observable without real Slack infrastructure.
	responseURLPoster func(ctx context.Context, url string, body any) error

	// ephemeralClient is the Slack client used for the ephemeral
	// fallback when surfacing a click/mention failure to a user who
	// has no response_url (or whose response_url POST failed). Defaults
	// (when nil) to slackClientFromAPI(l.concreteAPIClient()); tests inject a
	// fake so the fallback ephemeral is observable.
	ephemeralClient slackClient
}

// postResponseURL surfaces a body to a Slack interactive response_url via the
// injectable poster (http.DefaultClient in prod, a recorder in tests).
func (l *slackListener) postResponseURL(ctx context.Context, url string, body any) error {
	if l.responseURLPoster != nil {
		return l.responseURLPoster(ctx, url, body)
	}
	return postToResponseURL(ctx, nil, url, body)
}

// ephemeralSurface returns the Slack client used for ephemeral failure
// notices, preferring a test-injected override and falling back to the live
// API client.
func (l *slackListener) ephemeralSurface() slackClient {
	if l.ephemeralClient != nil {
		return l.ephemeralClient
	}
	return slackClientFromAPI(l.concreteAPIClient())
}

// concreteAPIClient extracts the concrete *slackapi.Client from l.api for
// call sites (slackClientFromAPI, PostEphemeral) that require the concrete
// type rather than the listenerAPIClient interface. Uses the two-value type
// assertion so a nil (or differently-typed) l.api yields a nil
// *slackapi.Client rather than a panic — the nil-pointer case
// slackClientFromAPI / PostEphemeral already handle gracefully.
// newSocketSource (socket_source.go) does its own single-value assertion for
// socketmode.New.
//
// In tests where l.api is a fake (fakeslack), this returns nil, so
// listener-originated ephemerals/error-fallbacks (slackClientFromAPI(l.concreteAPIClient()))
// no-op rather than recording to the fake — happy-path e2e is unaffected, but
// a future error-path e2e would observe nothing.
func (l *slackListener) concreteAPIClient() *slackapi.Client {
	c, _ := l.api.(*slackapi.Client)
	return c
}

// surfaceClickFailure tells the clicking user that their button click could not
// be recorded, so a parked approval/decision isn't left hanging with no
// feedback. It prefers the click's own response_url (an ephemeral edit Slack
// scopes to that user); when no response_url is available it falls back to a
// channel-scoped ephemeral. Best-effort: a failure to surface is logged, never
// returned — the caller still returns the original publish error so the log +
// watchdog paths fire.
//
// msg is the user-facing one-liner (e.g. "Couldn't record your decision; please
// try again."). responseURL is cb.ResponseURL; channelID/userID drive the
// ephemeral fallback (typically cb.Container.ChannelID + cb.User.ID).
func (l *slackListener) surfaceClickFailure(ctx context.Context, responseURL, channelID, userID, msg string) {
	logger := log.FromContext(ctx)
	if responseURL != "" {
		if err := l.postResponseURL(ctx, responseURL,
			map[string]any{"replace_original": false, "text": msg}); err != nil {
			logger.Info("slack: surface click failure via response_url failed; trying ephemeral",
				"userID", userID, "err", err.Error())
		} else {
			return
		}
	}
	if channelID == "" {
		logger.Info("slack: surface click failure has no response_url or channel; user left without feedback",
			"userID", userID)
		return
	}
	if err := PostEphemeral(ctx, l.ephemeralSurface(), channelID, userID, msg); err != nil {
		logger.Info("slack: surface click failure via ephemeral failed",
			"channelID", channelID, "userID", userID, "err", err.Error())
	}
}

// Compile-time interface assertions.
var (
	_ channelkinds.Listener       = (*slackListener)(nil)
	_ channelkinds.SessionWatcher = (*slackListener)(nil)
)

// SessionUpdated learns a new slack-thread anchor from a session whose
// OutputChannel binds this slack Channel. Called by channelsd's
// session-attached NATS handler after the outbound relay patches
// session.spec.outputChannel.{Key,External} (T15's write-back).
//
// Without this hook, a cron-spawned thread's anchor only entered the
// listener's threadIndex on the next channelsd restart's startup walk;
// any human reply in the meantime got dropped at the threadIndex gate
// in handleEventsAPI. Calling threads.put on every event keeps the
// index live.
//
// Idempotent: threadIndex.put is a set Add. Safe for concurrent
// invocation: threadIndex guards its own state.
func (l *slackListener) SessionUpdated(_ context.Context, sess *spiceboxv1alpha1.AgentSession) {
	if sess == nil || sess.Spec.OutputChannel == nil {
		return
	}
	out := sess.Spec.OutputChannel
	if out.Kind != KindName {
		return
	}
	if out.Name != "" && l.deps.Channel != nil && out.Name != l.deps.Channel.Name {
		// Defensive: the dispatcher already filters by OutputChannelName,
		// but the listener should not learn threads bound to a different
		// slack Channel CR if a stray event slipped through.
		return
	}
	ext := out.External
	channelID := ext["channel_id"]
	threadTS := ext["thread_ts"]
	if channelID == "" || threadTS == "" {
		// No thread anchor yet — the binding still carries only the seeded
		// channel reference, so there is nothing to index. Expected before
		// the outbound relay's first-send write-back; a problem after it.
		//
		// LOG it: if this is the step that drops the event, the human symptom
		// arrives minutes later as a reply silently dropped at the threadIndex
		// gate ("unowned thread"), with nothing connecting the two. A bare
		// return here left that chain undiagnosable.
		log.FromContext(context.Background()).V(1).Info(
			"slack: session-attached carried no thread anchor; not indexing",
			"channel", channelID, "thread", threadTS,
			"outputChannel", out.Name)
		return
	}
	// Cron-spawned (OutputChannel) threads are bot-originated — routing
	// mode default ("").
	l.threads.put(channelID+":"+threadTS, "")
	// Log the SUCCESS path, naming which listener took the entry. Two slack
	// Channels sharing one Slack app each keep their own threadIndex, and the
	// dispatcher hands this event only to the listener named by
	// OutputChannelName — so an entry can land on a listener that never
	// processes the corresponding inbound. Logging only the skip paths made
	// that indistinguishable from "the event never arrived".
	log.FromContext(context.Background()).V(1).Info("slack: indexed cron thread",
		"listener", listenerChannelName(l), "channel", channelID, "thread", threadTS)
}

func newSlackListener(deps channelkinds.Deps) *slackListener {
	return &slackListener{
		deps:                  deps,
		api:                   newSlackAPIClientFull(deps.Secret),
		idents:                NewIdentityCache(listenerIdentityCacheSize),
		seenEvts:              newEventIDCache(listenerEventCacheSize),
		threads:               newThreadIndex(),
		assistantThreads:      map[string]string{},
		restartMem:            deps.Memory,
		preferences:           deps.Preferences,
		personalizableClasses: deps.PersonalizableClasses,
	}
}

// Start resolves the bot's own user ID, opens the Socket Mode connection, and
// starts the event-dispatch goroutine. It blocks only until the connection
// handshake completes (RunContext is launched in the background).
func (l *slackListener) Start(ctx context.Context) error {
	if l.api == nil {
		return fmt.Errorf("slack listener: bot-token or app-token missing from credentials Secret")
	}

	auth, err := l.api.AuthTestContext(ctx)
	if err != nil {
		return fmt.Errorf("slack auth.test: %w", err)
	}
	l.botUserID = auth.UserID
	l.installedTeamID = auth.TeamID

	// Surface missing bot scopes on the Channel CR so operators see
	// the problem in `kubectl get channels` rather than discovering
	// it the first time an agent attaches an artifact and the user
	// reads "Attachment delivery failed". X-OAuth-Scopes is set by
	// Slack on every successful response; an unexpectedly-empty
	// header is treated as "all required scopes missing" by
	// missingScopes (rather than silently passing).
	//
	// The header is cached so channelsd's reconcile tick can recompute the
	// condition against the bound agent's CURRENT capabilities without
	// another auth.test on every one of them. The cache itself is refreshed
	// from Slack on a much slower interval, which is what lets a scope granted
	// by re-installing the app be noticed at all — see scopes.go.
	l.recordGrantedScopes(auth.Header.Get("X-OAuth-Scopes"))
	l.refreshScopesValid(ctx, l.deps.Channel)

	// Operator override (rare); allows pinning when multi-workspace tokens share
	// a single auth.test result that doesn't match the installed bot user.
	if cfg := l.deps.Channel.Spec.Slack; cfg != nil && cfg.BotUserID != "" {
		l.botUserID = cfg.BotUserID
	}

	// Rebuild the thread index from existing AgentSessions for this Channel
	// so message.channels/groups events for previously-seen threads route
	// correctly after a channelsd restart. Without this, a user's reply
	// (without re-@mentioning) is dropped silently and the agent appears
	// stuck.
	l.repopulateThreadIndex(ctx)

	l.src = newSocketSource(l.api)
	l.stop = make(chan struct{})

	go l.run(ctx)
	return nil
}

// repopulateThreadIndex restores the in-memory thread index from existing
// AgentSessions labeled with this Channel's name. Each session's
// spec.channel.external["thread_ts"] (when set) is the thread anchor;
// non-thread (DM) sessions are skipped — DMs route via message.im which
// doesn't consult the thread index.
//
// A second walk picks up cron-spawned sessions: those carry
// InputChannel=bento (no LabelChannelKind=slack) and only learn their
// slack thread anchor after the outbound relay patches OutputChannel
// on first send (T15). Without indexing those threads here, a human
// reply to a cron-spawned thread arrives at the slack listener after
// a channelsd restart and gets dropped at the threadIndex gate.
//
// The startup walk is the durability backstop for the live NATS-driven
// updates (channelevents.SessionAttachedSubject → SessionUpdated): if a
// channelsd restart drops a SessionAttached event, the walk on the next
// Start() picks up every patched OutputChannel thread anchor in
// existence. Without the NATS path the walk alone would have a window
// where new cron-spawned threads between restarts dropped at the
// threadIndex gate; with it, the walk is "second line of defense" only.
func (l *slackListener) repopulateThreadIndex(ctx context.Context) {
	if l.deps.K8sClient == nil || l.deps.Channel == nil {
		return
	}
	// Walk 1: sessions whose InputChannel binds to THIS slack Channel.
	// (Standard human-initiated slack threads.)
	var sessions spiceboxv1alpha1.AgentSessionList
	if err := l.deps.K8sClient.List(ctx, &sessions,
		client.InNamespace(l.deps.Channel.Namespace),
		client.MatchingLabels{
			spiceboxv1alpha1.LabelChannelName: l.deps.Channel.Name,
			spiceboxv1alpha1.LabelChannelKind: KindName,
		},
	); err != nil {
		log.FromContext(ctx).Info("slack: repopulateThreadIndex list failed",
			"channel", l.deps.Channel.Name,
			"walk", "1",
			"err", err.Error())
		return
	}
	for i := range sessions.Items {
		sess := &sessions.Items[i]
		// Index ALL threads, including those whose last session is
		// Succeeded/Failed. Terminal-session state is a per-conversation
		// concern, but thread routing is "did the bot ever get involved
		// in this thread?" — once yes, always yes. The pipeline correctly
		// creates a fresh session when the user replies to a thread whose
		// last session is terminal (filter excludes Succeeded/Failed in
		// the existing-session lookup).
		if sess.Spec.InputChannel == nil {
			continue
		}
		ext := sess.Spec.InputChannel.External
		if ext == nil {
			continue
		}
		channelID := ext["channel_id"]
		threadTS := ext["thread_ts"]
		if channelID == "" || threadTS == "" {
			continue
		}
		// Carry the session's authoritative routing mode into the index
		// so adopted (mention_only) threads survive a channelsd restart.
		l.threads.put(channelID+":"+threadTS, sess.Spec.InputChannel.RoutingMode)
	}

	// Walk 2: cron-spawned sessions whose OutputChannel binds to THIS
	// slack Channel CR. These carry LabelChannelKind=<input kind>
	// (e.g. "bento") rather than slack, so they're invisible to walk 1.
	// We list namespace-wide and filter Go-side on OutputChannel
	// because there's no label that indexes the *output* channel name.
	var outSessions spiceboxv1alpha1.AgentSessionList
	if err := l.deps.K8sClient.List(ctx, &outSessions,
		client.InNamespace(l.deps.Channel.Namespace),
	); err != nil {
		log.FromContext(ctx).Info("slack: repopulateThreadIndex list failed",
			"channel", l.deps.Channel.Name,
			"walk", "2",
			"err", err.Error())
		return
	}
	for i := range outSessions.Items {
		sess := &outSessions.Items[i]
		out := sess.Spec.OutputChannel
		if out == nil || out.Kind != KindName || out.Name != l.deps.Channel.Name {
			continue
		}
		ext := out.External
		if ext == nil {
			continue
		}
		channelID := ext["channel_id"]
		threadTS := ext["thread_ts"]
		if channelID == "" || threadTS == "" {
			// Pre-first-send cron sessions have channel_id only (or
			// nothing) and no thread_ts; nothing to index yet.
			continue
		}
		// Cron-spawned threads are bot-originated — routing mode default.
		l.threads.put(channelID+":"+threadTS, "")
	}
}

// Stop signals the run goroutine to exit. Idempotent.
func (l *slackListener) Stop(_ context.Context) error {
	if l.stop != nil {
		select {
		case <-l.stop:
			// already closed; nothing to do
		default:
			close(l.stop)
		}
	}
	return nil
}

// run drives the Socket Mode event loop. It starts RunContext (which blocks
// and reconnects internally) in a goroutine, and dispatches events from
// l.src.Events() in a second goroutine. Both exit when ctx or l.stop fires.
func (l *slackListener) run(ctx context.Context) {
	// Dispatch goroutine: reads events from the channel.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-l.stop:
				return
			case evt, ok := <-l.src.Events():
				if !ok {
					return
				}
				l.handle(ctx, evt)
			}
		}
	}()

	// RunContext blocks and handles reconnection internally.
	// We ignore the error — on ctx.Done() it returns a context error, which
	// is expected. Genuine disconnects are reconnected by socketmode internally.
	_ = l.src.RunContext(ctx)
}

// handle dispatches a single socketmode event.
func (l *slackListener) handle(ctx context.Context, evt socketmode.Event) {
	log.FromContext(ctx).Info("slack: socketmode event", "type", evt.Type)
	switch evt.Type {
	case socketmode.EventTypeEventsAPI:
		apiEvt, ok := evt.Data.(slackevents.EventsAPIEvent)
		if !ok {
			return
		}
		// Ack before processing; Slack requires acknowledgement within 3 s.
		if evt.Request != nil {
			l.src.Ack(*evt.Request)
		}
		l.handleEventsAPI(ctx, apiEvt)

	case socketmode.EventTypeInteractive:
		cb, ok := evt.Data.(slackapi.InteractionCallback)
		if !ok {
			return
		}
		if evt.Request != nil {
			l.src.Ack(*evt.Request)
		}
		if err := l.onInteraction(ctx, cb); err != nil {
			log.FromContext(ctx).Error(err, "slack: handle interaction")
		}

	case socketmode.EventTypeConnecting,
		socketmode.EventTypeConnected,
		socketmode.EventTypeDisconnect,
		socketmode.EventTypeHello:
		// Connection lifecycle; no action required for v1.

	case socketmode.EventTypeErrorBadMessage,
		socketmode.EventTypeErrorWriteFailed,
		socketmode.EventTypeIncomingError:
		// socketmode reconnects automatically, but the error itself must be
		// surfaced, not swallowed. These are rare (~2 per 9h in prod), so
		// logging the cause is the only thing that makes a recurring transport
		// fault greppable.
		log.FromContext(ctx).Info("slack: socketmode transport error",
			"type", evt.Type, "detail", socketErrorDetail(evt.Data))

	default:
		// Unhandled event types.
	}
}

// onInteraction processes a Slack interactive payload (button clicks, etc.).
// Handles block_actions families:
//
//   - "show_settings" — "Show settings" button on agent messages; opens the
//     effectiveSettings modal. Discriminated by v:"show_settings".
//   - discInteractionDetails ("interaction_details") — the Show-Details button
//     buildInteractionRequestBlocks appends whenever the request payload carries
//     Details; see handleInteractionDetailsAction (interaction_details.go). It
//     is the sole Show-Details affordance: every decision category renders
//     through the unified Interaction model.
//   - discInteraction ("interaction") decision buttons — multiplayer-session
//     approvals (permission_request), tool_approval, info_leakage, and every
//     other decision category (identity_choice, …); see
//     handleInteractionDecisionClick below.
//   - "metaagent_<approve|deny|show_details>_<requestId>" — dynamic scope
//     approvals for authzd (the metaagent orchestrator); publishes a raw
//     JSON payload to ap.session.<ns>.<name>.in.metaagent_approval_applied.
//     Session ns/name is carried in the button value JSON as `s:"ns/name"`.
//   - "home:pref:<ns>/<class>:<key>" / "home:pref_save:<ns>/<class>" — App
//     Home preference edits; see handleHomePrefClick
//     (app_home_pref_interaction.go).
//
// Unknown ActionIDs are silently skipped.
func (l *slackListener) onInteraction(ctx context.Context, cb slackapi.InteractionCallback) error {
	// "Restart from here" message action: user picked it from the ⋮ More Actions
	// menu on a thread message. Opens the restart confirmation modal. Handled
	// before the block_actions gate because it's a distinct interaction type.
	if cb.Type == slackapi.InteractionTypeMessageAction && cb.CallbackID == "ap_restart_from_here" {
		return l.handleRestartShortcut(ctx, cb)
	}
	// "Restart from here" modal submit: user confirmed the edited text and
	// clicked Restart. Re-resolves the session + cut and calls restartSubmit.
	if cb.Type == slackapi.InteractionTypeViewSubmission && cb.View.CallbackID == RestartModalCallbackID {
		return l.handleRestartSubmit(ctx, cb)
	}
	if cb.Type != slackapi.InteractionTypeBlockActions {
		return nil
	}
	// Show-Details button — a sibling of the decision buttons
	// buildInteractionRequestBlocks renders whenever
	// InteractionRequestPayload.Details is populated (tool_approval today).
	// Discriminated by `v:"interaction_details"`. Opens a modal; no envelope
	// publish. Checked BEFORE handleInteractionDecisionClick so a Details click
	// can never fall through into the decision-publish path.
	if handled, err := l.handleInteractionDetailsAction(ctx, cb); err != nil {
		return err
	} else if handled {
		return nil
	}
	// "Show settings" button on agent messages, discriminated by
	// `v:"show_settings"`. Opens the effectiveSettings modal.
	if handled, err := l.handleShowSettingsAction(ctx, cb); err != nil {
		return err
	} else if handled {
		return nil
	}
	if handled, err := l.handleSessionInstructionsAction(ctx, cb); err != nil {
		return err
	} else if handled {
		return nil
	}
	// metaagent scope-approval buttons. Discriminated by action_id
	// prefix "metaagent_". handleMetaagentApprovalAction returns true
	// when it recognized and processed the click; false falls through.
	if handled, err := l.handleMetaagentApprovalAction(ctx, cb); err != nil {
		return err
	} else if handled {
		return nil
	}
	// Decision-action buttons: identity_choice's agent/userPassthrough/cancel,
	// permission_request's Approve/Deny, queued_messages' "Interrupt & Send
	// Now", and any other ActionKindDecision category generically.
	// Discriminated by `v:"interaction"` in the button value JSON.
	// handleInteractionDecisionClick returns true when it recognized and
	// processed (or failed to publish) a click; false falls through.
	if handled, err := l.handleInteractionDecisionClick(ctx, cb); err != nil {
		return err
	} else if handled {
		return nil
	}
	// live-view "View live" button: mint a fresh signed link on each click and
	// reply with an ephemeral Open button. Discriminated by action_id ==
	// liveViewActionID + value.v == "live_view".
	if handled, err := l.handleLiveViewClick(ctx, cb, slackClientFromAPI(l.concreteAPIClient())); err != nil {
		return err
	} else if handled {
		return nil
	}
	// App Home preference edits: a "home:pref:" control change (bool
	// checkbox / enum static_select) or a "home:pref_save:" button.
	// Discriminated by action_id prefix — see app_home_pref_interaction.go.
	// Commits through the clicker's OWN canonical subject and republishes
	// the Home tab; never reads a subject off cb.
	if handled, err := l.handleHomePrefClick(ctx, cb); err != nil {
		return err
	} else if handled {
		return nil
	}
	// The resurface-welded "Interrupt & Send Now" button, provider_error_retry's
	// Retry click, and the enqueue-ack's own interrupt button need no leg here:
	// all three ride the Interaction model (ActionKindDecision, encoded
	// v:"interaction") and are handled by handleInteractionDecisionClick above,
	// which publishes interaction_decision(<category>) so the pipe's category
	// DeciderPolicy re-checks the clicker's standing server-side (fail-closed).
	return nil
}

// handleInteractionDecisionClick recognizes a discInteraction ("interaction")
// block_action — a decision-action button interactionSender rendered for an
// ActionKindDecision action (identity_choice's 3-way
// agent/userPassthrough/cancel today; any decision-kind category generically,
// since the button value carries Category). It publishes a
// KindInteractionDecision envelope on the inbound subject, and channelsd's
// decision pipe validates the decider's standing per the category's
// DeciderPolicy before republishing Applied — which
// interactionSender.sendDecisionApplied edits back in place via the ResponseRef
// this click sets below. Returns (true, nil) when it recognized and processed
// (or failed to publish) the click; (false, nil) to fall through to the next
// discriminator handler.
//
// This handler runs NO client-side authz gate: the decision pipe IS the
// fail-closed standing check. The clicker's email and team are still enriched
// via resolveIdentity, and that is CRITICAL — DecideRequester canonicalizes the
// decider to user:<base64(email)> before comparing against the cached request's
// Audience.Requester, so an email-less Decider can never canonicalize
// (identity.Canonical fails closed) and even the correct user's click is
// silently rejected as "not the addressee".
func (l *slackListener) handleInteractionDecisionClick(ctx context.Context, cb slackapi.InteractionCallback) (bool, error) {
	if len(cb.ActionCallback.BlockActions) == 0 {
		return false, nil
	}
	raw := cb.ActionCallback.BlockActions[0].Value
	if raw == "" {
		return false, nil
	}
	v, ok := decodeApprovalButtonValue(raw)
	if !ok || v.V != discInteraction {
		// Not a discInteraction click — fall through to whichever handler (or
		// the slice-1 path) owns this discriminator.
		return false, nil
	}
	ns, name, ok := strings.Cut(v.S, "/")
	if !ok || ns == "" || name == "" {
		return true, fmt.Errorf("interaction decision: malformed session ref %q", v.S)
	}
	if v.C == "" {
		return true, fmt.Errorf("interaction decision: button value missing category (requestRef=%s)", v.R)
	}
	clickerExt := l.resolveIdentity(ctx, cb.User.ID)
	pl := channelevents.InteractionDecisionPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: ns, Name: name},
		Category:        v.C,
		RequestRef:      v.R,
		ActionID:        v.D,
		Decider: channelevents.ExternalIdentity{
			Kind:       "slack",
			ExternalID: identity.RawExternalID(cb.User.ID),
			Email:      clickerExt.Email,
			TeamScope:  clickerExt.TeamScope,
		},
		// ResponseRef round-trips to Applied.ResponseRef (the decision pipe
		// copies it verbatim) so interactionSender.sendDecisionApplied can edit
		// THIS clicker's own ephemeral/DM in place rather than posting a fresh
		// message or falling back to the session-initiator notice.
		ResponseRef: cb.ResponseURL,
	}
	if err := channelevents.PublishIn(l.deps.NATSPublish, ns, name,
		channelevents.KindInteractionDecision, pl); err != nil {
		// The publish failed: the runner's awaited interaction decision never
		// arrives and the paused session hangs. Tell the clicker their choice
		// didn't register so they can retry.
		l.surfaceClickFailure(ctx, cb.ResponseURL, cb.Container.ChannelID, cb.User.ID,
			"⚠️ Couldn't record your choice — the agent wasn't reachable. Please try clicking again.")
		return true, fmt.Errorf("interaction decision: publish: %w", err)
	}
	log.FromContext(ctx).Info("slack: interaction decision click published",
		"category", v.C, "requestRef", v.R, "actionId", v.D, "session", v.S,
		"decider", cb.User.ID, "deciderEmail", clickerExt.Email)
	return true, nil
}

// handleLiveViewClick recognizes a "View live" block_action (action_id ==
// liveViewActionID, value.v == "live_view") and replies with a fresh, signed
// live-view link. Minting at click time (rather than baking a 30-minute URL
// into the button) keeps the button working for the life of the session.
//
// Returns (true, nil) on a recognized-and-handled click (including the
// user-surfaced failure cases). Returns (false, nil) when this is not a
// live-view click (caller falls through). Per AGENTS.md every error path logs
// and/or surfaces an ephemeral to the clicker.
//
// sc is the Slack client to use for getPermalink + postEphemeral; production
// passes slackClientFromAPI(l.concreteAPIClient()), tests pass a fake.
func (l *slackListener) handleLiveViewClick(ctx context.Context, cb slackapi.InteractionCallback, sc slackClient) (bool, error) {
	if len(cb.ActionCallback.BlockActions) == 0 {
		return false, nil
	}
	logger := log.FromContext(ctx)
	for _, a := range cb.ActionCallback.BlockActions {
		if a.ActionID != liveViewActionID {
			continue
		}
		var v liveViewButtonValue
		if err := json.Unmarshal([]byte(a.Value), &v); err != nil {
			return true, fmt.Errorf("live_view click: decode value: %w", err)
		}
		if v.V != "live_view" {
			continue
		}
		if v.A == "" || v.S == "" {
			return true, fmt.Errorf("live_view click: value missing artifactId/sessionRef")
		}
		channelID := v.C
		if channelID == "" {
			channelID = cb.Container.ChannelID
		}

		minter := l.deps.ArtifactViewMinter
		if minter == nil {
			logger.Info("live_view click: webd not configured", "clicker", cb.User.ID)
			if err := PostEphemeral(ctx, sc, channelID, cb.User.ID,
				"Live view isn't configured for this workspace."); err != nil {
				logger.Info("live_view click: postEphemeral (not-configured) failed", "err", err.Error())
			}
			return true, nil
		}

		// Best-effort thread back-link. A getPermalink failure must NOT block
		// the view — log it and mint with an empty back-link.
		backLink := ""
		if v.T != "" {
			if pl, err := sc.GetPermalinkContext(ctx, &slackapi.PermalinkParameters{Channel: channelID, Ts: v.T}); err != nil {
				logger.Info("live_view click: getPermalink failed; back-link omitted",
					"channel", channelID, "thread", v.T, "err", err.Error())
			} else {
				backLink = pl
			}
		}

		// Canonical SpiceDB subject for the clicker. This is NOT merely audit
		// metadata: on an OIDC-off install (no WebAuthenticator for the kind),
		// webd/identityd's trust-link fallback mints the session cookie from
		// this Subject and gates CheckArtifactView on it. It MUST be a valid
		// canonical id (user:<base64>), the same form the view grant was
		// written against — never a raw email (SpiceDB object_ids reject @/.).
		ext := l.resolveIdentity(ctx, cb.User.ID)
		// The clicker is a channel participant (artifact viewer); a guest
		// without a verified email is keyed by the synthetic subject, matching
		// the form the view grant was written against. Opt in explicitly.
		subject := identity.FromExternal(
			identity.Kind(ext.Kind),
			identity.TeamScope(ext.TeamScope),
			identity.RawExternalID(ext.ExternalID),
			identity.Email(ext.Email),
		).AllowSynthetic()
		url, err := minter.MintArtifactViewLink(v.A, v.S, subject, backLink)
		if err != nil {
			logger.Info("live_view click: mint failed", "artifactID", v.A, "session", v.S, "err", err.Error())
			if perr := PostEphemeral(ctx, sc, channelID, cb.User.ID,
				"Couldn't generate a live-view link; please try again."); perr != nil {
				logger.Info("live_view click: postEphemeral (mint-error) failed", "err", perr.Error())
			}
			return true, nil
		}
		if url == "" {
			logger.Info("live_view click: webd URL not yet available", "artifactID", v.A, "session", v.S)
			if perr := PostEphemeral(ctx, sc, channelID, cb.User.ID,
				"Live view isn't available yet — try again in a moment."); perr != nil {
				logger.Info("live_view click: postEphemeral (not-ready) failed", "err", perr.Error())
			}
			return true, nil
		}

		opts := []slackapi.MsgOption{
			slackapi.MsgOptionBlocks(buildLiveViewOpenBlocks(url)...),
			slackapi.MsgOptionText("Open the live view", false),
		}
		if v.T != "" {
			opts = append(opts, slackapi.MsgOptionTS(v.T))
		}
		if _, err := sc.PostEphemeralContext(ctx, channelID, cb.User.ID, opts...); err != nil {
			logger.Info("live_view click: postEphemeral (open button) failed",
				"channel", channelID, "clicker", cb.User.ID, "err", err.Error())
			return true, fmt.Errorf("live_view click: postEphemeral: %w", err)
		}
		logger.Info("live_view click: posted fresh open link",
			"artifactID", v.A, "session", v.S, "clicker", cb.User.ID, "withBackLink", backLink != "")
		return true, nil
	}
	return false, nil
}

// handleEventsAPI routes an Events API callback to the appropriate path.
func (l *slackListener) handleEventsAPI(ctx context.Context, api slackevents.EventsAPIEvent) {
	if api.Type != slackevents.CallbackEvent {
		return
	}

	// Diagnostic: log the inner event type so operators can see which
	// events Slack IS delivering. If app_home_opened never shows up
	// here, the Slack app config is missing the event subscription —
	// not a code bug. Log at INFO so it's visible without bumping the
	// channelsd verbosity.
	log.FromContext(ctx).Info("slack: events_api inner",
		"type", api.InnerEvent.Type)

	// Delivery-lag signal: how long this event sat between Slack stamping it
	// and channelsd receiving it. This is what distinguishes a slow ack caused
	// by Slack-side event delivery from one caused by our own processing — the
	// two are indistinguishable from the outside without it. Best-effort: only
	// message-bearing inner events carry a ts worth measuring against.
	if ts := inboundEventTS(api); ts != "" {
		now := l.now // tests may leave the clock unset; default to wall time.
		if now == nil {
			now = time.Now
		}
		if lag, ok := deliveryLag(ts, now()); ok {
			log.FromContext(ctx).Info("slack: inbound delivery lag",
				"eventType", api.InnerEvent.Type, "eventTS", ts, "lagMs", lag.Milliseconds())
		}
	}

	switch ev := api.InnerEvent.Data.(type) {
	case *slackevents.AppHomeOpenedEvent:
		// User opened the App Home tab — re-publish the per-user view
		// with current state (which agents exist, what they need, what
		// the user has linked). See pkg/channels/channelkinds/slack/app_home.go
		// for the rendering pipeline + the "always re-publish, no
		// cache" rationale.
		l.handleAppHomeOpened(ctx, ev)
		return

	case *slackevents.AssistantThreadStartedEvent:
		// User opened the agent container or DM'd us. Slack assigns a
		// canonical thread_ts to the assistant thread; remember it so
		// subsequent message.im events in this conversation can call
		// assistant.threads.setStatus with a thread_ts Slack accepts.
		k := ev.AssistantThread.ChannelID + ":" + ev.AssistantThread.UserID
		l.assistantThreadsMu.Lock()
		l.assistantThreads[k] = ev.AssistantThread.ThreadTimeStamp
		l.assistantThreadsMu.Unlock()
		log.FromContext(ctx).Info("slack: assistant_thread_started",
			"channelID", ev.AssistantThread.ChannelID,
			"userID", ev.AssistantThread.UserID,
			"threadTS", ev.AssistantThread.ThreadTimeStamp)
		return

	case *slackevents.AssistantThreadContextChangedEvent:
		// User navigated to a different channel within the agent container.
		// We don't currently use the context payload (no channel-aware
		// context retrieval), but we re-stamp the thread_ts in case it has
		// shifted (it shouldn't, but be defensive).
		k := ev.AssistantThread.ChannelID + ":" + ev.AssistantThread.UserID
		l.assistantThreadsMu.Lock()
		l.assistantThreads[k] = ev.AssistantThread.ThreadTimeStamp
		l.assistantThreadsMu.Unlock()
		return

	case *slackevents.AppMentionEvent:
		// Any app_mention arriving — even a bot's — proves the app's
		// app_mention subscription is active. Record it so the message.channels
		// drop path can tell a missing subscription from a benign duplicate.
		l.sawAppMention.Store(true)
		// Bots can trigger app_mention (e.g., another bot @-mentions us) — and
		// that is exactly the cross-agent INTENT signal: one agent naming
		// another is mention-gating, arriving as its own event type.
		//
		// Same classifier as the message arm, so the two cannot disagree about
		// what counts as our own post. Self stays refused here too: an
		// app_mention we triggered ourselves is still a self-loop.
		//
		// The origin is classified ONCE and carried to the handler, rather than
		// re-derived there. Two derivations could disagree, and the way they
		// would disagree is an agent message travelling as a human one — which
		// refills the wake budget instead of spending it, turning the bound
		// into its opposite.
		mentionOrigin := classifyOrigin(ev.User, ev.BotID, l.botUserID)
		if crossAgentOn, _ := l.crossAgentConfig(); !admitsInbound(mentionOrigin, crossAgentOn) {
			return
		}
		// Dedup key: (channel, ts), against Slack re-delivering this same
		// app_mention. The message.channels twin of an @-mention is handled by
		// the mention guards in that arm below, not by this key — the two arms
		// hold separate key spaces.
		dedupKey := "app_mention:" + ev.Channel + ":" + ev.TimeStamp
		if !l.seenEvts.Add(dedupKey) {
			return
		}
		// Metaagent routing: if the mention text contains
		// <@METAAGENT_BOT_USER_ID>, route to authzd instead of the
		// session's runner. We resolve the session to get its ns/name
		// for the subject; if no active session exists in this thread,
		// post an ephemeral notice and drop.
		if l.metaagentBotUserID != "" && strings.Contains(ev.Text, "<@"+l.metaagentBotUserID+">") {
			l.handleMetaagentMention(ctx, ev)
			return
		}
		l.handleChannelMessage(ctx,
			ev.User, ev.Channel,
			ev.ThreadTimeStamp, ev.TimeStamp,
			ev.Text,
			slackInboundAttachments(ev.Files),
			true, // new thread allowed on @mention
			mentionOrigin,
		)
		// ADDITIONALLY classify, when the session runs ambient. Never instead
		// of the routing above — the turn is the user's message to the agent
		// first, and a classification second.
		l.maybeTriggerAmbient(ctx, ev.User, ev.Channel, ev.ThreadTimeStamp, ev.TimeStamp, ev.Text)

	case *slackevents.MessageEvent:
		// Reject subtypes that aren't a plain user message — edits,
		// deletions, channel/group membership churn, bot posts, etc.
		// file_share is the one admitted exception: Slack tags ANY message
		// that carries an attached file with this subtype, with or without
		// accompanying text, so rejecting it here would silently drop every
		// DM/thread-reply attachment before an InboundEvent is ever built
		// (see slackInboundAttachments below).
		if ev.SubType != "" && ev.SubType != slackapi.MsgSubTypeFileShare {
			return
		}
		// Who sent this? Our own posts and unattributed messages are refused in
		// every configuration; another agent's are admitted only when
		// cross-agent participation is on. Classified rather than folded into
		// one `||` chain so the self arm cannot be relaxed by someone meaning
		// to relax the other-agent arm.
		//
		// Admitted is not awake: an admitted cross-agent message still only
		// APPENDS unless mention-gating and wake credit both allow a wake.
		msgOrigin := classifyOrigin(ev.User, ev.BotID, l.botUserID)
		if crossAgentOn, _ := l.crossAgentConfig(); !admitsInbound(msgOrigin, crossAgentOn) {
			return
		}

		switch ev.ChannelType {
		case "im":
			dedupKey := "message.im:" + ev.Channel + ":" + ev.TimeStamp
			if !l.seenEvts.Add(dedupKey) {
				return
			}
			// Resolve the assistant thread_ts: prefer the canonical value
			// captured from assistant_thread_started; fall back to the
			// message's own ThreadTimeStamp (for messages in an established
			// thread) and finally to the message ts itself (legacy DM that
			// pre-dates the assistant container).
			threadTS := l.lookupAssistantThread(ev.Channel, ev.User)
			if threadTS == "" {
				threadTS = ev.ThreadTimeStamp
			}
			if threadTS == "" {
				threadTS = ev.TimeStamp
			}
			l.handleDM(ctx, ev.User, ev.Channel, threadTS, ev.Text, slackInboundAttachments(slackMessageEventFiles(ev)))

		case "channel", "group":
			// Only route thread replies; top-level messages in a channel
			// are only handled via the app_mention event.
			if ev.ThreadTimeStamp == "" {
				if appMentionMissingSuspected(ev.Text, l.botUserID, l.sawAppMention.Load()) {
					// A top-level @-mention reached the message.channels handler
					// while we've never seen an app_mention. That is EITHER a
					// missing app_mention subscription OR simply this message's
					// own app_mention not having been processed yet — Slack emits
					// both events with no ordering guarantee, so on the first
					// mention after start the message.channels event can win the
					// race. Don't decide from this single snapshot; defer the
					// verdict by a grace window.
					l.scheduleMissingAppMentionHint(ctx, ev.Channel, ev.User)
				}
				return
			}
			// A thread reply that @-mentions the bot ALSO arrives on the
			// app_mention subscription, and app_mention is the arm that owns it:
			// it is where metaagent routing lives, so letting whichever event
			// Slack happens to deliver first decide would make that routing a
			// coin flip. Drop the message.channels copy here, exactly as the
			// top-level branch above already does — same policy, and the same
			// missing-subscription hint covers the case where app_mention never
			// arrives.
			if mentionsUser(ev.Text, l.botUserID) {
				if appMentionMissingSuspected(ev.Text, l.botUserID, l.sawAppMention.Load()) {
					l.scheduleMissingAppMentionHint(ctx, ev.Channel, ev.User)
				}
				return
			}
			known, mode := l.threadIsOwned(ctx, ev.Channel, ev.ThreadTimeStamp)
			if !known {
				// Genuinely not an agent thread: no index entry on this
				// listener AND no session labelled with its output key. (A
				// bare index miss is NOT sufficient — see threadIsOwned.)
				log.FromContext(ctx).V(1).Info("slack: dropping channel message in unowned thread",
					"channel", ev.Channel, "thread", ev.ThreadTimeStamp,
					"listener", listenerChannelName(l))
				return
			}
			if mode == "mention_only" {
				// Adopted thread: only app_mention events route. This plain
				// reply is not lost — the next catch-up backfill picks it up.
				log.FromContext(ctx).V(1).Info("slack: dropping plain reply in mention-only thread (backfill will pick it up)",
					"channel", ev.Channel, "thread", ev.ThreadTimeStamp)
				return
			}
			dedupKey := "message:" + ev.Channel + ":" + ev.TimeStamp
			if !l.seenEvts.Add(dedupKey) {
				return
			}
			l.handleChannelMessage(ctx,
				ev.User, ev.Channel,
				ev.ThreadTimeStamp, ev.TimeStamp,
				ev.Text,
				slackInboundAttachments(slackMessageEventFiles(ev)),
				false, // must be existing thread
				msgOrigin,
			)
			// The ambient trigger's main path: an ordinary in-thread reply,
			// addressed to nobody in particular. The prefilter rejects most of
			// these before anything costs.
			l.maybeTriggerAmbient(ctx, ev.User, ev.Channel, ev.ThreadTimeStamp, ev.TimeStamp, ev.Text)
		}
	}
}

// appMentionMissingSuspected reports whether a dropped top-level channel
// message is a sign the Slack app lacks the app_mention event subscription:
// it @-mentions this bot, yet no app_mention has ever reached this listener.
// When app_mention works, the same @-mention is handled via app_mention and
// only its deduped message.channels duplicate reaches the drop path — so the
// sawAppMention gate is what distinguishes a real misconfiguration from that
// benign duplicate.
func appMentionMissingSuspected(text, botUserID string, sawAppMention bool) bool {
	if sawAppMention || botUserID == "" {
		return false
	}
	return mentionsUser(text, botUserID)
}

// mentionsUser reports whether text carries Slack's mention markup for userID.
// An empty userID never matches: without an id to compare against, "does this
// mention us?" has no answer, and answering "yes" would route every message.
func mentionsUser(text, userID string) bool {
	if userID == "" {
		return false
	}
	return strings.Contains(text, "<@"+userID+">")
}

// defaultAppMentionHintGrace is how long the listener waits for a racing
// app_mention before concluding the subscription is missing. The matching
// app_mention is normally processed within milliseconds (its handler flips
// sawAppMention as its very first step); this window comfortably absorbs
// event-queue bursts. The cost of being generous is only a few seconds of
// latency on a one-time-per-channel configuration hint, so we err long.
const defaultAppMentionHintGrace = 5 * time.Second

func (l *slackListener) appMentionHintGraceDur() time.Duration {
	if l.appMentionHintGrace > 0 {
		return l.appMentionHintGrace
	}
	return defaultAppMentionHintGrace
}

// scheduleMissingAppMentionHint defers the "missing app_mention subscription"
// verdict instead of deciding it from a single message.
//
// A channel @-mention makes Slack emit TWO events — app_mention and
// message.channels — with NO guaranteed delivery order. On the first @-mention
// after this listener starts, the message.channels event can be processed
// before the matching app_mention flips sawAppMention, so warning here directly
// is a false positive even though the subscription is healthy (the symptom:
// "I'm not receiving app_mention events" on a mention that WAS an app mention).
// Whether the app lacks the subscription is a configuration fact that is stable
// over time, so we re-check after a grace window: if any app_mention arrives
// within it, the subscription demonstrably works and we stay quiet; only a
// sustained absence is a real misconfiguration worth surfacing.
//
// At most one in-flight check (and one warning) per channel: the dedup key is
// claimed at schedule time so a genuinely-missing subscription — where every
// mention reaches this path — does not spawn a goroutine per message.
func (l *slackListener) scheduleMissingAppMentionHint(ctx context.Context, channelID, userID string) {
	if !l.seenEvts.Add("appmention-hint:" + channelID) {
		return
	}
	go func() {
		t := time.NewTimer(l.appMentionHintGraceDur())
		defer t.Stop()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if l.sawAppMention.Load() {
			// An app_mention arrived within the grace window — the subscription
			// works; this was just app_mention/message.channels delivery-order
			// skew, not a misconfiguration. Stay quiet.
			log.FromContext(ctx).V(1).Info("slack: a top-level @-mention reached message.channels before its app_mention; subscription is healthy, suppressing missing-subscription hint",
				"channel", channelID)
			return
		}
		// No app_mention has EVER arrived within the grace window → the Slack app
		// almost certainly lacks the app_mention event subscription (or was not
		// reinstalled after adding it). Surface it loudly instead of dropping
		// silently, once per channel.
		log.FromContext(ctx).Info("slack: a top-level @-mention reached message.channels and no app_mention arrived within the grace window; the Slack app is likely missing the app_mention event subscription (or needs reinstalling)",
			"channel", channelID)
		if err := PostEphemeral(ctx, l.ephemeralSurface(), channelID, userID,
			"I saw your mention, but I'm not receiving `app_mention` events, so I can't respond to @-mentions in this channel. Ask an admin to add the *app_mention* event subscription to this Slack app and reinstall it."); err != nil {
			log.FromContext(ctx).Info("slack: app_mention-missing hint ephemeral failed", "channel", channelID, "err", err.Error())
		}
	}()
}

// maxAttachmentFilenameRunes bounds a sanitized attachment filename. Applied
// at the listener boundary — the earliest point untrusted Slack input enters
// the system — before the name reaches any struct a later task will put
// into a turn, a log line, or a storage key.
const maxAttachmentFilenameRunes = 128

// sanitizeAttachmentFilename strips control characters and truncates to
// maxAttachmentFilenameRunes. Truncation counts only surviving (non-control)
// runes, and is rune-aware rather than byte-aware so a multi-byte UTF-8
// filename is never split mid-codepoint.
func sanitizeAttachmentFilename(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	kept := 0
	for _, r := range name {
		if kept >= maxAttachmentFilenameRunes {
			break
		}
		if unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
		kept++
	}
	return b.String()
}

// slackMessageEventFiles returns the files attached to a MessageEvent.
// Unlike AppMentionEvent (which carries Files directly), MessageEvent's
// custom UnmarshalJSON always re-parses the raw event into Message
// (*slack.Msg) — even for a plain new message with no subtype — so Files
// lives there, not on the event itself. Message is nil only when the event
// was built by hand (tests) rather than unmarshaled off the wire.
func slackMessageEventFiles(ev *slackevents.MessageEvent) []slackapi.File {
	if ev.Message == nil {
		return nil
	}
	return ev.Message.Files
}

// slackInboundAttachments maps Slack's raw file objects to
// channelkinds.InboundAttachment refs. Deliberately independent of any
// capability or Channel-spec check — see InboundEvent.Attachments: noticing
// that a file exists is always allowed, regardless of whether anything is
// configured to ever fetch its bytes.
func slackInboundAttachments(files []slackapi.File) []channelkinds.InboundAttachment {
	if len(files) == 0 {
		return nil
	}
	out := make([]channelkinds.InboundAttachment, 0, len(files))
	for _, f := range files {
		out = append(out, channelkinds.InboundAttachment{
			ExternalID: f.ID,
			Filename:   sanitizeAttachmentFilename(f.Name),
			MIME:       f.Mimetype,
			SizeBytes:  int64(f.Size),
		})
	}
	return out
}

// classifyThreadEntry maps the raw Slack thread_ts/message_ts pair to an
// InboundEvent.ThreadEntry. A mention with no upstream thread_ts (or one
// equal to its own ts) starts a thread (root); anything else is a reply
// inside a pre-existing thread. It decides nothing about adoption — the
// pipeline does, by combining this with session correlation.
func classifyThreadEntry(threadTS, msgTS string) string {
	if threadTS == "" || threadTS == msgTS {
		return channelkinds.ThreadEntryRoot
	}
	return channelkinds.ThreadEntryReply
}

// threadBootstrapPlan decides what the listener posts into a thread for a
// routed inbound. An informational starter is posted ONLY for a brand-new
// bot-rooted thread, to bootstrap a real thread_ts that
// assistant.threads.setStatus accepts. The starter is never edited — the
// sink lazily posts a streaming bubble for the first reply. An adopted
// thread (the bot summoned into a pre-existing thread) already has a thread
// root, so it gets the transparency join notice instead and NO starter —
// the agent's updates are then delivered as plain threaded replies. A
// non-new session (a reply on an established thread) needs neither.
func threadBootstrapPlan(newSession, adopted bool) (postStarter, postJoinNotice bool) {
	if !newSession {
		return false, false
	}
	if adopted {
		return false, true
	}
	return true, false
}

// formatCollectiveOwnershipNotice states, in plain words, that this session is
// owned by a POPULATION rather than by the person who started it.
//
// Said unprompted because the people it affects cannot discover it: ownership
// lives in a SpiceDB tuple derived from a Channel manifest they have never
// seen, and it carries real authority — an owner can approve what the agent is
// allowed to do, fork the session, and widen its scope. Someone joining the
// channel next week acquires all of that silently. A room that is collectively
// in charge of an agent should be told so.
func formatCollectiveOwnershipNotice() string {
	return " Everyone in this channel shares ownership of me: any member can talk to me, " +
		"and can approve or deny what I'm allowed to do — including people who join later."
}

// formatJoinNotice is the transparency message posted into a thread when
// the bot is summoned into it. Thread participants did not opt in, so
// the notice names who can talk to the bot and states the @-mention
// requirement.
//
// Each participant is made inert individually, before it is joined into this
// kind's own copy. Escaping the composed sentence instead happens to be
// equivalent TODAY — this notice interpolates nothing else and its separators
// are inert characters — but it would stop being equivalent the moment the
// sentence gained any markup this kind composes (a `<@…>` mention, a link to
// the session), and that is the shape publicNoteText and
// buildInteractionAppliedBlocks already had to be corrected into. Escaping the
// untrusted values, not the finished string, is the form that stays right.
//
// The names are not ours. They are AuthorDisplayName off users.info
// (history.go), i.e. RealName or Name as their OWNER set it, carried through
// channelsd's adoption grant into InboundDecision.GrantedParticipants. Any
// workspace member — a single-channel guest included — can set their profile
// name to `<!channel>` or `<https://…|Connect your Google account>`, post once
// in a thread, and have the BOT render it when it is later invited: a
// channel-wide ping or a genuine-looking hyperlink, in the very thread where
// this kind posts real credential-linking prompts. postJoinNotice posts with
// MsgOptionText(_, false), so slack-go escapes nothing on our behalf.
//
// withheld names participants adoption granted no standing to — see the
// withheld-naming rationale in the function body below.
func formatJoinNotice(participants, withheld []string) string {
	var b strings.Builder
	b.WriteString("👋 I've been brought into this thread and can see the recent conversation above.")
	if len(participants) > 0 {
		b.WriteString(" ")
		b.WriteString(strings.Join(escapeSlackTexts(participants), ", "))
		b.WriteString(" can chat with me here.")
	}
	// Naming the withheld authors is the whole point of tracking them. From
	// inside the thread someone who was refused looks exactly like someone who
	// was granted, right up until they @-mention the bot and nothing happens.
	// Saying so here explains the silence before it occurs.
	if len(withheld) > 0 {
		b.WriteString(" ")
		// Escaped for the same reason participants are, and with more cause: a
		// withheld author is by construction OUTSIDE the class's trust policy,
		// so their display name is the least trustworthy string in this
		// notice. Unescaped it rendered as the bot's own mrkdwn — a
		// <!channel> ping, a forged <@mention>, or a labelled link — in the
		// thread where this kind also posts real credential prompts.
		b.WriteString(strings.Join(escapeSlackTexts(withheld), ", "))
		if len(withheld) == 1 {
			b.WriteString(" is not on this agent's access list, so I won't respond to them.")
		} else {
			b.WriteString(" are not on this agent's access list, so I won't respond to them.")
		}
	}
	b.WriteString(" @-mention me to reply.")
	return b.String()
}

// postJoinNotice posts the join notice as its own thread message. It is
// separate from the working-indicator placeholder so it is never edited
// away. Best-effort: a failure is logged and ignored.
func (l *slackListener) postJoinNotice(ctx context.Context, channelID, threadTS string, participants, withheld []string) {
	if l.api == nil {
		return
	}
	_, _, err := l.api.PostMessageContext(ctx, channelID,
		slackapi.MsgOptionText(formatJoinNotice(participants, withheld), false),
		slackapi.MsgOptionTS(threadTS),
	)
	if err != nil {
		log.FromContext(ctx).Info("slack: postJoinNotice failed (best-effort)",
			"channelID", channelID, "threadTS", threadTS, "err", err.Error())
	}
}

// handleChannelMessage delivers a #channel or private-group message (either
// an @mention that may start a new thread, or a thread reply) into the
// inbound pipeline.
func (l *slackListener) handleChannelMessage(
	ctx context.Context,
	userID, channelID, threadTS, msgTS, text string,
	attachments []channelkinds.InboundAttachment,
	newThreadAllowed bool,
	origin botOrigin,
) {
	// anchor is the stable thread identifier: prefer thread_ts; fall back to
	// the message's own ts for top-level @mentions (which become the root of
	// the new thread).
	anchor := threadTS
	if anchor == "" {
		if !newThreadAllowed {
			return
		}
		anchor = msgTS
	}

	// Register the thread so future message.channels events route here.
	// Routing mode is unknown until the pipeline assigns it — pass "";
	// the post-Deliver upgrade below records the authoritative mode.
	l.threads.put(channelID+":"+anchor, "")

	channelKey := "thread:" + channelID + ":" + anchor
	identity := l.resolveIdentity(ctx, userID)
	// Read alongside the origin so the budget charged to this message is the
	// one the class that admitted it declared. Read again downstream, it could
	// be a different class's.
	_, wakeBudget := l.crossAgentConfig()

	dec, err := l.deps.Inbound.Deliver(ctx, channelkinds.InboundEvent{
		Channel:     l.deps.Channel,
		ExternalIDs: identity,
		ChannelKey:  channelKey,
		MessageText: text,
		Attachments: attachments,
		ThreadEntry: classifyThreadEntry(threadTS, msgTS),
		// Derived from the origin the admit site classified, not re-derived
		// from userID here. originOtherAgent is the ONLY value that spends
		// wake credit: self and unattributed never reach this call, and a
		// human refills.
		FromAgent:  origin == originOtherAgent,
		WakeBudget: wakeBudget,
		// The anchor the outbound reply must thread under, handed over so the
		// pipeline can write it BEFORE it wakes the runner. Stamping it after
		// Deliver returned is what let a reply post top-level under load.
		PreTurnAnnotations:             map[string]string{LastInboundTSAnnotationKey: anchor},
		RequesterCanonicalIDAnnotation: LastInboundCanonicalIDAnnotationKey,
		External: map[string]string{
			"channel_id":  channelID,
			"thread_ts":   anchor,
			"message_ts":  msgTS,
			"team_id":     identity.TeamScope.String(),
			"bot_user_id": l.botUserID,
		},
		Reply: channelkinds.InboundReplyHooks{
			Ephemeral: func(ctx context.Context, msg string) error {
				return PostEphemeral(ctx, slackClientFromAPI(l.concreteAPIClient()), channelID, userID, msg)
			},
		},
	})
	if err != nil {
		log.FromContext(ctx).Error(err, "slack: handleThreaded pipeline.Deliver error",
			"channelID", channelID, "anchor", anchor, "outcome", dec.Outcome)
		besteffort.Log(log.FromContext(ctx).Info, "slack internal-error notice",
			// One registered category, not a fifth copy of the sentence.
			// No SessionRef: these paths fire before a session exists (or
			// after resolving one failed), and the renderer omits the
			// provenance footer rather than naming a session that is not there.
			l.postNoticeEphemeral(ctx, channelID, userID, channelevents.SessionRef{}, internalErrorNotice()),
			"channelID", channelID, "userID", userID)
		return
	}
	if dec.Outcome == channelkinds.OutcomeDeniedByPermission {
		log.FromContext(ctx).Info("slack: handleThreaded denied",
			"channelID", channelID, "anchor", anchor, "notice", dec.Notice.Category())
		// Post publicly in the thread so the rejected user, the original
		// requester (tagged in the message), and channel observers all see
		// the denial in context. Ephemeral was misleading — coworkers
		// wouldn't know why the bot stayed silent.
		//
		// A deliberately suppressed notice (a duplicate request already
		// pending) posts nothing and logs why; the poster handles that, so
		// there is no empty-string sentinel to check here any more.
		l.postNoticeInThread(ctx, channelID, anchor, sessionRefOf(dec), dec.Notice)
		return
	}
	if dec.Outcome == channelkinds.OutcomeInternalError {
		log.FromContext(ctx).Info("slack: handleThreaded internal-error outcome (no err)",
			"channelID", channelID, "anchor", anchor)
		besteffort.Log(log.FromContext(ctx).Info, "slack internal-error notice",
			// One registered category, not a fifth copy of the sentence.
			// No SessionRef: these paths fire before a session exists (or
			// after resolving one failed), and the renderer omits the
			// provenance footer rather than naming a session that is not there.
			l.postNoticeEphemeral(ctx, channelID, userID, channelevents.SessionRef{}, internalErrorNotice()),
			"channelID", channelID, "userID", userID)
		return
	}
	if dec.Outcome == channelkinds.OutcomeThreadOwnedByAnotherAgent {
		// Withdraw the eager pre-Deliver registration above: this thread is
		// bound to a different agent. Leaving the positive entry in place
		// would be worse than never checking — it is the fast path, so every
		// later message in the thread would short-circuit threadIsOwned and
		// skip the ownership check entirely. Recording the refusal instead
		// also keeps the (uncached) apiserver lookup once per thread.
		l.threads.putRefused(channelID + ":" + anchor)
		log.FromContext(ctx).V(1).Info("slack: thread belongs to another agent; withdrawing from it",
			"listener", listenerChannelName(l), "channelID", channelID, "anchor", anchor)
		return
	}
	if dec.Outcome == channelkinds.OutcomeRefused || dec.Outcome == channelkinds.OutcomeForkPending ||
		dec.Outcome == channelkinds.OutcomeHandledNoAgent {
		// Terminal-session continuation. There is no runner to route to, so
		// post the visible notice and DO NOT set a "starting…" status (that
		// would strand the thread on a status with nothing behind it). For
		// NewInheriting the operator forks a fresh session in a new thread; the
		// sender's forward-link ties the two threads together once the agent
		// replies.
		log.FromContext(ctx).Info("slack: handleThreaded terminal-continuation outcome",
			"outcome", dec.Outcome, "channelID", channelID, "anchor", anchor)
		l.postNoticeInThread(ctx, channelID, anchor, sessionRefOf(dec), dec.Notice)
		return
	}
	log.FromContext(ctx).Info("slack: handleThreaded outcome",
		"outcome", dec.Outcome, "channelID", channelID, "anchor", anchor,
		"sessionName", dec.Session.Name, "newSession", dec.NewSession)
	if dec.Outcome == channelkinds.OutcomeRouted {
		// Upgrade the threadIndex entry with the session's authoritative
		// routing mode. The pre-Deliver put() registered the thread with
		// "" because the mode is the pipeline's to assign; an adopted
		// thread comes back mention_only, so non-mention replies must
		// stop routing.
		if dec.Session.Channel != nil {
			l.threads.put(channelID+":"+anchor, dec.Session.Channel.RoutingMode)
		}
		// Bootstrap actions for a routed inbound. A brand-new bot-rooted
		// thread gets an informational starter so assistant.threads.setStatus
		// has a real thread_ts. The starter is never edited; the first reply
		// always appends below it. An adopted thread (the bot summoned into a
		// pre-existing thread) already has a root, so it gets the
		// transparency join notice and NO starter — the agent's updates
		// then land as plain threaded replies. See threadBootstrapPlan.
		postStart, postJoin := threadBootstrapPlan(dec.NewSession, dec.Adopted)
		if postStart {
			l.postStarter(ctx, dec.Session.Namespace, dec.Session.Name, channelID, anchor, dec.Ownership.Collective)
		}
		if postJoin {
			l.postJoinNotice(ctx, channelID, anchor, dec.GrantedParticipants, dec.WithheldParticipants)
		}
		// Fallback only. On the append path the pipeline already wrote these
		// BEFORE waking the runner, which is the ordering the outbound reply
		// depends on; repeating it here would be a second patch of the same
		// object with the same values, on every message.
		if !dec.PreTurnAnnotationsStamped {
			l.stampLastInbound(ctx, dec.Session, anchor, dec.RequesterCanonicalID)
		}
		// A queued inbound did not start a turn — the one already in flight
		// owns the status line, and the pipeline has separately acked the
		// queue to this sender. Announcing "…is starting…" here would both
		// contradict that ack and clobber the live turn's progress caption,
		// so leave the thread's surface (and the watchdog clock the running
		// turn is already touching) alone.
		if !dec.Queued {
			l.postStartingStatus(ctx, channelID, anchor)
			// Inform the watchdog that a setStatus just landed — starts the
			// 30/60s silence-detection clock for this session.
			if l.deps.TouchSetStatus != nil {
				l.deps.TouchSetStatus(dec.Session.Namespace, dec.Session.Name)
			}
		}
	}
}

// handleDM delivers a DM message into the inbound pipeline.
// channelKey is "dm:<user_id>" — a single ongoing session per user per Channel CR.
func (l *slackListener) handleDM(ctx context.Context, userID, channelID, msgTS, text string, attachments []channelkinds.InboundAttachment) {
	identity := l.resolveIdentity(ctx, userID)
	channelKey := "dm:" + userID

	dec, err := l.deps.Inbound.Deliver(ctx, channelkinds.InboundEvent{
		Channel:     l.deps.Channel,
		ExternalIDs: identity,
		ChannelKey:  channelKey,
		MessageText: text,
		Attachments: attachments,
		// The DM's own message ts IS the anchor the reply threads under, and
		// it is handed over so the pipeline writes it before waking. This is
		// the exact path that posted orphan top-level replies under load.
		PreTurnAnnotations:             map[string]string{LastInboundTSAnnotationKey: msgTS},
		RequesterCanonicalIDAnnotation: LastInboundCanonicalIDAnnotationKey,
		External: map[string]string{
			"channel_id":  channelID,
			"message_ts":  msgTS,
			"team_id":     identity.TeamScope.String(),
			"bot_user_id": l.botUserID,
		},
		Reply: channelkinds.InboundReplyHooks{
			Ephemeral: func(ctx context.Context, msg string) error {
				return PostEphemeral(ctx, slackClientFromAPI(l.concreteAPIClient()), channelID, userID, msg)
			},
		},
	})
	if err != nil {
		log.FromContext(ctx).Error(err, "slack: handleDM pipeline.Deliver error",
			"channelID", channelID, "msgTS", msgTS, "outcome", dec.Outcome)
		besteffort.Log(log.FromContext(ctx).Info, "slack internal-error notice",
			// One registered category, not a fifth copy of the sentence.
			// No SessionRef: these paths fire before a session exists (or
			// after resolving one failed), and the renderer omits the
			// provenance footer rather than naming a session that is not there.
			l.postNoticeEphemeral(ctx, channelID, userID, channelevents.SessionRef{}, internalErrorNotice()),
			"channelID", channelID, "userID", userID)
		return
	}
	if dec.Outcome == channelkinds.OutcomeDeniedByPermission {
		log.FromContext(ctx).Info("slack: handleDM denied",
			"channelID", channelID, "msgTS", msgTS, "notice", dec.Notice.Category())
		// Ephemeral in a DM: there is no audience beyond the one person, so
		// the in-thread reasoning the channel path needs would just be noise.
		besteffort.Log(log.FromContext(ctx).Info, "slack deny notice",
			l.postNoticeEphemeral(ctx, channelID, userID, sessionRefOf(dec), dec.Notice),
			"channelID", channelID, "userID", userID)
		return
	}
	if dec.Outcome == channelkinds.OutcomeInternalError {
		log.FromContext(ctx).Info("slack: handleDM internal-error outcome (no err)",
			"channelID", channelID, "msgTS", msgTS)
		besteffort.Log(log.FromContext(ctx).Info, "slack internal-error notice",
			// One registered category, not a fifth copy of the sentence.
			// No SessionRef: these paths fire before a session exists (or
			// after resolving one failed), and the renderer omits the
			// provenance footer rather than naming a session that is not there.
			l.postNoticeEphemeral(ctx, channelID, userID, channelevents.SessionRef{}, internalErrorNotice()),
			"channelID", channelID, "userID", userID)
		return
	}
	if dec.Outcome == channelkinds.OutcomeRefused || dec.Outcome == channelkinds.OutcomeForkPending ||
		dec.Outcome == channelkinds.OutcomeHandledNoAgent {
		// Terminal-session continuation in a DM. Post the visible notice and
		// DO NOT set a "starting…" status — there is no runner behind the
		// terminal session. NewInheriting's fresh session continues under the
		// same DM thread anchor once the operator forks it.
		log.FromContext(ctx).Info("slack: handleDM terminal-continuation outcome",
			"outcome", dec.Outcome, "channelID", channelID, "msgTS", msgTS)
		l.postNoticeInThread(ctx, channelID, msgTS, sessionRefOf(dec), dec.Notice)
		return
	}
	log.FromContext(ctx).Info("slack: handleDM outcome",
		"outcome", dec.Outcome, "channelID", channelID, "msgTS", msgTS,
		"sessionName", dec.Session.Name)
	if dec.Outcome == channelkinds.OutcomeRouted {
		// Under agent_view there is no Slack-minted assistant thread: msgTS is
		// the user's own message ts (handleEventsAPI fell through
		// lookupAssistantThread()="" → ThreadTimeStamp → TimeStamp). We stamp it
		// as LastInboundTS so the outbound reply threads under it, and setStatus
		// on it auto-opens the thread in the Messages tab. A not-yet-migrated
		// assistant_view app still works: there msgTS is the captured assistant
		// root and the same stamping applies.
		// Fallback only — see the threaded path. The pipeline writes these
		// before it wakes the runner, so on the append path the reply's anchor
		// is already on the session by the time the turn can produce one.
		if !dec.PreTurnAnnotationsStamped {
			l.stampLastInbound(ctx, dec.Session, msgTS, dec.RequesterCanonicalID)
		}
		// Queued mid-turn DM: same reasoning as the threaded path — the turn
		// in flight owns the status line, and the queue ack is the user's
		// signal. See handleChannelMessage.
		if !dec.Queued {
			l.postStartingStatus(ctx, channelID, msgTS)
			if l.deps.TouchSetStatus != nil {
				l.deps.TouchSetStatus(dec.Session.Namespace, dec.Session.Name)
			}
		}
	}
}

// lookupAssistantThread returns the canonical Slack-assigned thread_ts for an
// assistant conversation with this user in this channel, or "" if the
// assistant_thread_started event hasn't been seen for that pair. Used by the
// DM dispatch path to pass a thread_ts that assistant.threads.setStatus
// accepts (any other ts surfaces as invalid_thread_ts).
func (l *slackListener) lookupAssistantThread(channelID, userID string) string {
	l.assistantThreadsMu.Lock()
	defer l.assistantThreadsMu.Unlock()
	return l.assistantThreads[channelID+":"+userID]
}

// stampLastInbound records the effective thread_ts and canonicalID of the
// current inbound on the AgentSession. Both are stamped in a single patch.
// The Slack sender reads LastInboundTS to resolve thread_ts for
// assistant.threads.setStatus calls. The runner reads LastInboundCanonicalID
// for tool-dispatch authz Checks. Best-effort: failures are logged so the
// downstream symptom is diagnosable.
func (l *slackListener) stampLastInbound(ctx context.Context, sess channelkinds.SessionInfo, threadTS, canonicalID string) {
	if l.deps.K8sClient == nil || threadTS == "" {
		return
	}
	patch := []byte(fmt.Sprintf(
		`{"metadata":{"annotations":{%q:%q,%q:%q}}}`,
		LastInboundTSAnnotationKey, threadTS,
		LastInboundCanonicalIDAnnotationKey, canonicalID,
	))
	err := l.deps.K8sClient.Patch(ctx,
		&spiceboxv1alpha1.AgentSession{
			ObjectMeta: metav1.ObjectMeta{Name: sess.Name, Namespace: sess.Namespace},
		},
		client.RawPatch(types.MergePatchType, patch),
	)
	besteffort.Log(log.FromContext(ctx).Info, "patch LastInbound annotations", err,
		"session", sess.Namespace+"/"+sess.Name, "threadTS", threadTS, "canonicalID", canonicalID)
}

// postInThread posts a non-ephemeral message into the thread. Used for
// permission-denial messages so the channel sees the denial (with @-tagged
// users) in context. Best-effort: any error is logged and swallowed.
func (l *slackListener) postInThread(ctx context.Context, channelID, threadTS, text string) {
	if l.api == nil {
		return
	}
	opts := []slackapi.MsgOption{slackapi.MsgOptionText(text, false)}
	if threadTS != "" {
		opts = append(opts, slackapi.MsgOptionTS(threadTS))
	}
	if _, _, err := l.api.PostMessageContext(ctx, channelID, opts...); err != nil {
		log.FromContext(ctx).Info("slack: postInThread failed (best-effort, continuing)",
			"error", err, "channelID", channelID, "threadTS", threadTS)
	}
}

// postStarter posts the one-time informational starter message into the thread
// to bootstrap the thread root in Slack's database. The starter is never
// edited by the listener itself — the sink lazily posts a new streaming
// bubble for the first reply — but a SUCCESSFUL post's coordinates are
// recorded into the shared starterCache (keyed by sessNS+"/"+sessName) so the
// thread_title sender can later edit this exact message in place to emulate a
// title. Best-effort: logs and returns on error (setStatus still proceeds;
// the agent's first reply will land as a fresh chat.postMessage from the
// sender).
func (l *slackListener) postStarter(ctx context.Context, sessNS, sessName, channelID, threadTS string, collectiveOwnership bool) {
	if l.api == nil {
		return
	}
	text, startedUnix := l.startingMessage()
	if collectiveOwnership {
		// The first message a new session posts is the only one everyone in the
		// channel reliably sees, so the disclosure belongs here rather than in a
		// follow-up nobody scrolls back to.
		text += formatCollectiveOwnershipNotice()
	}
	opts := []slackapi.MsgOption{slackapi.MsgOptionText(text, false)}
	if threadTS != "" {
		opts = append(opts, slackapi.MsgOptionTS(threadTS))
	}
	_, ts, err := l.api.PostMessageContext(ctx, channelID, opts...)
	if err != nil || ts == "" {
		log.FromContext(ctx).Info("slack: postStarter failed (best-effort, continuing)",
			"error", err, "channelID", channelID, "threadTS", threadTS)
		return
	}
	if l.starters != nil {
		l.starters.put(sessNS+"/"+sessName, starterCoords{
			ChannelID: channelID, MessageTS: ts, AgentName: l.agentDisplayName(), StartedUnix: startedUnix,
		})
	}
	log.FromContext(ctx).Info("slack: posted starter",
		"channelID", channelID, "threadTS", threadTS, "starterTS", ts)
}

// postStartingStatus calls assistant.threads.setStatus with the
// "<agentClass> is starting…" text so the AI-native indicator under the bot's
// name shows the agent name immediately. Sets loading_messages to the same
// text fanned out across spinner frames (animatedLoadingMessages) so the
// thread-top indicator and Slack's bottom-of-channel typing indicator both
// display the same text — animated by Slack's own rotation — instead of
// Slack's built-in default phrases. Best-effort: any error is logged and
// swallowed.
func (l *slackListener) postStartingStatus(ctx context.Context, channelID, threadTS string) {
	if l.api == nil {
		return
	}
	status := l.startingStatus()
	log.FromContext(ctx).Info("slack: setStatus (starting)",
		"channelID", channelID, "threadTS", threadTS, "status", status)
	if err := l.api.SetAssistantThreadsStatusContext(ctx, slackapi.AssistantThreadsSetStatusParameters{
		ChannelID:       channelID,
		ThreadTS:        threadTS,
		Status:          status,
		LoadingMessages: animatedLoadingMessages(status),
	}); err != nil {
		log.FromContext(ctx).Info("slack: postStartingStatus setStatus failed (best-effort, continuing)",
			"error", err, "channelID", channelID, "threadTS", threadTS)
		return
	}
	log.FromContext(ctx).Info("slack: setStatus succeeded",
		"channelID", channelID, "threadTS", threadTS)
}

// agentDisplayName returns a user-visible label for the agent. Uses the
// AgentClass name from the bound Channel CR (e.g., "summarizer"); falls back
// to "agent" if unavailable.
func (l *slackListener) agentDisplayName() string {
	if l.deps.Channel != nil && l.deps.Channel.Spec.AgentClass != "" {
		return l.deps.Channel.Spec.AgentClass
	}
	return "agent"
}

// startingMessageAt renders the un-titled starter text for a given agent name
// and start time. Extracted so postStarter can reuse the exact unix timestamp
// baked into the <!date…> token when caching starter coordinates.
func startingMessageAt(agentName string, unix int64) string {
	return fmt.Sprintf("🤖 *%s* started <!date^%d^{date_short_pretty} at {time}|just now>",
		agentName, unix)
}

// startingMessage returns the one-time informational starter posted into the
// thread to bootstrap a thread root that assistant.threads.setStatus accepts.
// It uses Slack's <!date^TS^...|fallback> token so Slack re-renders the
// timestamp client-side as a live relative time ("just now", "5 minutes ago",
// "yesterday"), so the message stays current without us ever editing it. The
// returned unix is the same timestamp baked into the <!date…> token, so a
// caller (postStarter) can cache it alongside the posted message's coordinates.
func (l *slackListener) startingMessage() (text string, unix int64) {
	now := l.now
	if now == nil {
		now = time.Now
	}
	unix = now().Unix()
	return startingMessageAt(l.agentDisplayName(), unix), unix
}

// startingStatus is the assistant.threads.setStatus indicator text shown
// under the bot's name as soon as the thread is created. It includes the
// agent name so the user sees which agent is responding.
func (l *slackListener) startingStatus() string {
	return l.agentDisplayName() + " is starting…"
}

// emailTrusted reports whether a Slack user's profile email may be used as the
// canonical authorization identity. It is trusted ONLY for a full member of the
// bot's own installed workspace — never for a foreign-workspace user, a guest,
// a bot, or a deleted account — because the canonical SpiceDB subject is
// derived from this email and a foreign/guest account is low-trust.
func emailTrusted(u *slackapi.User, installedTeamID string) bool {
	return orgMembershipOf(u, installedTeamID) == channelkinds.OrgMembershipMember
}

// orgMembershipOf classifies a Slack user's standing in the bot's installed
// workspace for the session-start gate. Only a full, non-deleted, human
// member of the installed workspace is a member; every other shape — guest
// (restricted/ultra-restricted), shared-channel stranger, foreign workspace,
// bot, deleted, nil user, unknown installed team — is guest. It never returns
// the empty OrgMembership: an undetermined standing must gate, not pass.
//
// This predicate exists to be branched on for authorization; accountTypeOf
// (userprofile.go) remains descriptive-only.
func orgMembershipOf(u *slackapi.User, installedTeamID string) channelkinds.OrgMembership {
	if u == nil || installedTeamID == "" {
		return channelkinds.OrgMembershipGuest
	}
	if u.TeamID != installedTeamID {
		return channelkinds.OrgMembershipGuest
	}
	if limitedAccount(u) {
		return channelkinds.OrgMembershipGuest
	}
	return channelkinds.OrgMembershipMember
}

// limitedAccount reports whether the Slack account is anything other than a
// full, active, human account: a guest (restricted/ultra-restricted), a
// shared-channel stranger, a bot, or a deleted account.
func limitedAccount(u *slackapi.User) bool {
	return u.IsRestricted || u.IsUltraRestricted || u.IsStranger || u.IsBot || u.Deleted
}

// resolveIdentity returns ExternalIdentity for a Slack user_id, using the LRU
// cache to avoid hitting users.info on every event from a returning user.
//
// On any API error the identity is returned with an empty email and the
// INSTALLED team as its scope, so it falls back to the slack:<team>:<user>
// canonical_id path in identity.go. The team matters: Principal.Canonical has
// no empty-teamScope guard, so leaving the scope empty here would mint
// base64("slack::U…") — a subject that matches neither the user's email
// canonical nor their team-scoped one. The user would then be durably
// mis-attributed (denied interact on their own session, and left with an
// orphan cluster-scoped UserIdentity carrying an empty domain) because of one
// transient Slack blip. The failure is also logged: a mis-attribution with no
// trace is the hardest kind to diagnose.
func (l *slackListener) resolveIdentity(ctx context.Context, userID string) channelkinds.ExternalIdentity {
	id := channelkinds.ExternalIdentity{
		Kind:       KindName,
		ExternalID: identity.RawExternalID(userID),
		TeamScope:  identity.TeamScope(l.installedTeamID),
		// Fail-closed default: every path that cannot positively establish
		// full installed-workspace membership (API error, no client, a
		// cache entry without a stamp) leaves the user gated as a guest.
		OrgMembership: channelkinds.OrgMembershipGuest,
	}
	// Nil-safe for unit tests that build a slackListener directly
	// (without the idents cache / api client wired). Production
	// always has both set via newSlackListener.
	if l.idents != nil {
		if cached, ok := l.idents.Get(userID); ok {
			id.Email = identity.Email(cached.Email)
			id.TeamScope = identity.TeamScope(cached.TeamID)
			id.DisplayName = cached.DisplayName
			if cached.Membership == channelkinds.OrgMembershipMember {
				id.OrgMembership = channelkinds.OrgMembershipMember
			}
			return id
		}
	}
	if l.api == nil {
		return id
	}
	user, err := l.api.GetUserInfoContext(ctx, userID)
	if err != nil || user == nil {
		log.FromContext(ctx).Info("slack: users.info lookup failed; attributing to the team-scoped synthetic subject",
			"slackUserID", userID, "installedTeamID", l.installedTeamID,
			"listener", listenerChannelName(l), "err", errString(err))
		return id
	}
	// Only trust the profile email for full members of the bot's own
	// installed workspace. Foreign-workspace users, guests, bots, and
	// deleted accounts fall back to the unforgeable slack:<team>:<user>
	// synthetic so an attacker cannot escalate via a shared Slack
	// workspace they don't fully belong to.
	membership := orgMembershipOf(user, l.installedTeamID)
	var trustedEmail string
	if emailTrusted(user, l.installedTeamID) {
		trustedEmail = user.Profile.Email
	}
	id.Email = identity.Email(trustedEmail)
	id.TeamScope = identity.TeamScope(user.TeamID)
	id.DisplayName = preferredDisplayName(user)
	id.OrgMembership = membership
	if l.idents != nil {
		l.idents.Put(userInfo{UserID: userID, Email: trustedEmail, TeamID: user.TeamID, DisplayName: id.DisplayName, Membership: membership})
	}
	return id
}

// errString renders err for a structured log field, tolerating nil so a
// "lookup produced nothing" path can share one log line with a real failure.
func errString(err error) string {
	if err == nil {
		return "users.info returned no user"
	}
	return err.Error()
}

// slackClientFromAPI adapts *slackapi.Client to the slackClient interface
// declared in sender.go so PostEphemeral/OpenIM can be reused without
// duplicating the method surface.
func slackClientFromAPI(c *slackapi.Client) slackClient {
	if c == nil {
		return nil
	}
	return &concreteClientAdapter{c: c}
}

// concreteClientAdapter wraps *slackapi.Client as slackClient.
type concreteClientAdapter struct{ c *slackapi.Client }

func (a *concreteClientAdapter) PostMessageContext(ctx context.Context, channelID string, opts ...slackapi.MsgOption) (string, string, error) {
	return a.c.PostMessageContext(ctx, channelID, opts...)
}

func (a *concreteClientAdapter) PostEphemeralContext(ctx context.Context, channelID, userID string, opts ...slackapi.MsgOption) (string, error) {
	return a.c.PostEphemeralContext(ctx, channelID, userID, opts...)
}

func (a *concreteClientAdapter) UpdateMessageContext(ctx context.Context, channelID, ts string, opts ...slackapi.MsgOption) (string, string, string, error) {
	return a.c.UpdateMessageContext(ctx, channelID, ts, opts...)
}

func (a *concreteClientAdapter) OpenConversationContext(ctx context.Context, params *slackapi.OpenConversationParameters) (*slackapi.Channel, bool, bool, error) {
	return a.c.OpenConversationContext(ctx, params)
}

func (a *concreteClientAdapter) SetAssistantThreadsStatusContext(ctx context.Context, params slackapi.AssistantThreadsSetStatusParameters) error {
	return a.c.SetAssistantThreadsStatusContext(ctx, params)
}

func (a *concreteClientAdapter) SetAssistantThreadsTitleContext(ctx context.Context, params slackapi.AssistantThreadsSetTitleParameters) error {
	return a.c.SetAssistantThreadsTitleContext(ctx, params)
}

func (a *concreteClientAdapter) UploadFileContext(ctx context.Context, params slackapi.UploadFileParameters) (*slackapi.FileSummary, error) {
	return a.c.UploadFileContext(ctx, params)
}

func (a *concreteClientAdapter) GetUserByEmailContext(ctx context.Context, email string) (*slackapi.User, error) {
	return a.c.GetUserByEmailContext(ctx, email)
}

func (a *concreteClientAdapter) GetUserInfoContext(ctx context.Context, user string) (*slackapi.User, error) {
	return a.c.GetUserInfoContext(ctx, user)
}

func (a *concreteClientAdapter) OpenViewContext(ctx context.Context, triggerID string, view slackapi.ModalViewRequest) (*slackapi.ViewResponse, error) {
	return a.c.OpenViewContext(ctx, triggerID, view)
}

func (a *concreteClientAdapter) UpdateViewContext(ctx context.Context, view slackapi.ModalViewRequest, externalID, hash, viewID string) (*slackapi.ViewResponse, error) {
	return a.c.UpdateViewContext(ctx, view, externalID, hash, viewID)
}

func (a *concreteClientAdapter) GetUsersContext(ctx context.Context, options ...slackapi.GetUsersOption) ([]slackapi.User, error) {
	return a.c.GetUsersContext(ctx, options...)
}

func (a *concreteClientAdapter) GetPermalinkContext(ctx context.Context, params *slackapi.PermalinkParameters) (string, error) {
	return a.c.GetPermalinkContext(ctx, params)
}

// ---------------------------------------------------------------------------
// eventIDCache: thread-safe LRU set for dedup keys.
// Add returns true (inserted) when the key is new; false when already cached.
// ---------------------------------------------------------------------------

type eventIDCache struct {
	mu    sync.Mutex
	max   int
	set   map[string]*list.Element
	order *list.List
}

func newEventIDCache(max int) *eventIDCache {
	return &eventIDCache{
		max:   max,
		set:   make(map[string]*list.Element),
		order: list.New(),
	}
}

// Add inserts key and returns true if it was not previously present.
// Returns false (and refreshes LRU position) if the key already exists.
func (c *eventIDCache) Add(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if el, ok := c.set[key]; ok {
		c.order.MoveToFront(el)
		return false
	}

	el := c.order.PushFront(key)
	c.set[key] = el

	for c.order.Len() > c.max {
		old := c.order.Back()
		if old == nil {
			break
		}
		c.order.Remove(old)
		delete(c.set, old.Value.(string))
	}
	return true
}

// ---------------------------------------------------------------------------
// threadIndex: bounded LRU set of "<channel_id>:<thread_ts>" strings.
// Tracks threads the listener has dispatched so message.channels/groups
// replies can be routed to the correct session.
// ---------------------------------------------------------------------------

const threadIndexMax = 4096

// threadEntry is the value stored per known thread. routingMode is ""
// (every thread message routes) or "mention_only" (only @-mentions
// route — an adopted thread).
type threadEntry struct {
	key         string
	routingMode string
	// refused marks a thread that resolved to a Channel this listener may not
	// serve (another agent's). It is cached exactly like a hit because the
	// lookup behind it goes to an UNCACHED client: several agents' Slack apps
	// can share one conversation, and without remembering the refusal every
	// message in every one of their threads would re-issue a List and a Get
	// against the apiserver, forever.
	refused bool
}

type threadIndex struct {
	mu    sync.Mutex
	set   map[string]*list.Element
	order *list.List
}

func newThreadIndex() *threadIndex {
	return &threadIndex{
		set:   make(map[string]*list.Element),
		order: list.New(),
	}
}

// put registers key as a known thread with the given routing mode,
// refreshing its LRU position if already present and evicting the oldest
// entry if the index is at capacity. A non-empty routingMode never
// downgrades to "": once a thread is known to be mention_only it stays
// that way for the life of the index entry (callers that don't know the
// mode pass "").
func (t *threadIndex) put(key, routingMode string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if el, ok := t.set[key]; ok {
		t.order.MoveToFront(el)
		entry := el.Value.(*threadEntry)
		if routingMode != "" {
			entry.routingMode = routingMode
		}
		// A positive claim supersedes an earlier refusal: the listener was
		// later summoned into the thread and now has its own session on it.
		entry.refused = false
		return
	}

	el := t.order.PushFront(&threadEntry{key: key, routingMode: routingMode})
	t.set[key] = el

	for t.order.Len() > threadIndexMax {
		old := t.order.Back()
		if old == nil {
			break
		}
		t.order.Remove(old)
		delete(t.set, old.Value.(*threadEntry).key)
	}
}

// putRefused remembers that this listener may not serve key, so the decision
// costs one apiserver round-trip per thread rather than one per message. See
// threadEntry.refused.
func (t *threadIndex) putRefused(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// An existing entry must be FLIPPED, not left alone: handleChannelMessage
	// registers a thread eagerly, before the pipeline has ruled on it, so the
	// entry this withdraws is usually one that already says "owned".
	if el, ok := t.set[key]; ok {
		t.order.MoveToFront(el)
		el.Value.(*threadEntry).refused = true
		return
	}
	t.set[key] = t.order.PushFront(&threadEntry{key: key, refused: true})

	for t.order.Len() > threadIndexMax {
		old := t.order.Back()
		if old == nil {
			break
		}
		t.order.Remove(old)
		delete(t.set, old.Value.(*threadEntry).key)
	}
}

// lookup returns the thread's routing mode, whether this listener was refused
// it, and whether the thread is known at all, refreshing its LRU position.
func (t *threadIndex) lookup(key string) (routingMode string, refused, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	el, found := t.set[key]
	if !found {
		return "", false, false
	}
	t.order.MoveToFront(el)
	entry := el.Value.(*threadEntry)
	return entry.routingMode, entry.refused, true
}

// has reports whether key is a known thread — including one known to be
// refused. Callers deciding whether to ROUTE must use lookup and honour
// refused; has answers only "have we resolved this thread before?".
func (t *threadIndex) has(key string) bool {
	_, _, ok := t.lookup(key)
	return ok
}

// threadIsOwned reports whether (channelID, threadTS) belongs to an agent
// session this listener should route for, and that thread's routing mode.
//
// The in-memory threadIndex is the fast path. A MISS is not authoritative: the
// index is per-listener state, and a cron thread is written to exactly ONE
// listener — the one whose Channel is the session's OutputChannel (both
// SessionUpdated and repopulateThreadIndex walk 2 filter on
// out.Name == deps.Channel.Name). When several slack Channels share one Slack
// app (an output-only Channel plus the interactive one — the standard cron
// layout), the inbound is handled by whichever connection Slack delivers to.
// If that is not the listener holding the entry, a legitimate reply was
// dropped as an "unowned thread".
//
// That made replies to a cron thread intermittent: the same thread routed once
// and was dropped minutes later with no restart between. Routing correctness
// must not depend on which of several equivalent listeners receives an event,
// so on a miss we consult the authoritative record — a session labelled with
// this thread's output key, which any listener can read. Found sessions are
// folded back into the index so the lookup is once per thread, not per message.
func (l *slackListener) threadIsOwned(ctx context.Context, channelID, threadTS string) (bool, string) {
	if mode, refused, ok := l.threads.lookup(channelID + ":" + threadTS); ok {
		return !refused, mode
	}
	if l.deps.K8sClient == nil || l.deps.Channel == nil {
		return false, ""
	}
	key := "thread:" + channelID + ":" + threadTS
	var sessions spiceboxv1alpha1.AgentSessionList
	if err := l.deps.K8sClient.List(ctx, &sessions,
		client.InNamespace(l.deps.Channel.Namespace),
		client.MatchingLabels{spiceboxv1alpha1.LabelOutputChannelKey: channelkey.LabelValue(key)},
	); err != nil {
		// Never silently swallow: a lookup failure here reads to the user as a
		// dropped message, so it must be greppable.
		log.FromContext(ctx).Info("slack: thread-ownership lookup failed; treating thread as unowned",
			"channel", channelID, "thread", threadTS, "err", err.Error())
		return false, ""
	}
	if len(sessions.Items) == 0 {
		return false, ""
	}
	// A label match proves some session anchors this thread. It does NOT prove
	// THIS listener may serve it: the query is namespace-wide, so every Slack
	// app that is a member of the same Slack conversation matches here. Slack
	// hands each of those apps its own copy of the event, so claiming without
	// this check delivers one human message once per app and lets whichever
	// app writes last own the thread's status line. Only the bound Channel or
	// a sibling of it (same agent, same Slack app) may claim — see
	// mayClaimThread.
	for i := range sessions.Items {
		sess := &sessions.Items[i]
		if !mayClaimThread(l.deps.Channel, l.boundOutputChannel(ctx, sess)) {
			continue
		}
		// Cron-spawned (OutputChannel) threads are bot-originated — routing
		// mode default (""). Fold into the index so subsequent messages hit
		// the fast path. Only an ACCEPTED claim is indexed: indexing a refused
		// one would make every later reply in that thread short-circuit on the
		// fast path and skip this check entirely.
		l.threads.put(channelID+":"+threadTS, "")
		log.FromContext(ctx).V(1).Info("slack: thread recovered via session lookup (index miss on this listener)",
			"listener", listenerChannelName(l), "channel", channelID, "thread", threadTS,
			"session", sess.Name)
		return true, ""
	}
	l.threads.putRefused(channelID + ":" + threadTS)
	log.FromContext(ctx).V(1).Info("slack: thread is bound to another agent's Channel; not claiming",
		"listener", listenerChannelName(l), "channel", channelID, "thread", threadTS,
		"boundSession", sessions.Items[0].Name)
	return false, ""
}

// mayClaimThread reports whether a listener serving Channel `own` may route
// messages for a thread whose authoritative binding is the Channel `bound`.
//
// A thread belongs 1:1 to the Channel its session is bound to. The single
// exception is a SIBLING Channel — the same agent behind the same Slack app —
// because that is the standard cron layout: an output-only Channel carrying
// the thread anchor, plus the interactive Channel that fields the human's
// replies. Slack delivers each event to exactly one socket connection per app,
// so a sibling can never receive a duplicate of what the bound listener
// already handled. Two *different* Slack apps that are both members of one
// conversation each get their own copy of every event — claiming those is what
// turns one human message into N deliveries and lets one agent overwrite
// another's thread status.
//
// Sameness is declarative: identical credentialsRef.secretName (the same
// tokens, therefore the same Slack app) and identical agentClass. Two Channels
// holding duplicate copies of one app's tokens in separate Secrets read as
// different apps and are refused — a false negative that costs one dropped
// reply and is logged, rather than a false positive that mis-routes another
// agent's thread. An empty secretName or agentClass never matches: absence is
// not sameness, or every incompletely-specified Channel would claim every
// other's threads.
func mayClaimThread(own, bound *spiceboxv1alpha1.Channel) bool {
	if own == nil || bound == nil {
		return false
	}
	if own.Namespace == bound.Namespace && own.Name == bound.Name {
		return true
	}
	secret, class := own.Spec.CredentialsRef.SecretName, own.Spec.AgentClass
	if secret == "" || class == "" {
		return false
	}
	return own.Namespace == bound.Namespace &&
		secret == bound.Spec.CredentialsRef.SecretName &&
		class == bound.Spec.AgentClass
}

// boundOutputChannel resolves the slack Channel CR named by a session's OUTPUT
// binding — the authoritative owner of that session's thread on the fallback
// path, which is keyed by LabelOutputChannelKey. Returns nil when the binding
// is absent, is not a slack binding, or the Channel CR cannot be read; every
// such case is refused by mayClaimThread, so an unreadable Channel fails
// closed rather than granting the claim.
func (l *slackListener) boundOutputChannel(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) *spiceboxv1alpha1.Channel {
	out := sess.Spec.OutputChannel
	if out == nil || out.Kind != KindName || out.Name == "" {
		return nil
	}
	if out.Name == l.deps.Channel.Name && sess.Namespace == l.deps.Channel.Namespace {
		return l.deps.Channel
	}
	var ch spiceboxv1alpha1.Channel
	if err := l.deps.K8sClient.Get(ctx, types.NamespacedName{
		Namespace: sess.Namespace, Name: out.Name,
	}, &ch); err != nil {
		log.FromContext(ctx).Info("slack: cannot resolve the Channel a thread is bound to; refusing the claim",
			"listener", listenerChannelName(l), "boundChannel", out.Name,
			"session", sess.Namespace+"/"+sess.Name, "err", err.Error())
		return nil
	}
	return &ch
}

// listenerChannelName returns the Channel CR name this listener serves, or
// "<unbound>" when Deps carries none. Used only for diagnostics — two slack
// Channels sharing one Slack app are otherwise indistinguishable in logs.
func listenerChannelName(l *slackListener) string {
	if l == nil || l.deps.Channel == nil {
		return "<unbound>"
	}
	return l.deps.Channel.Name
}

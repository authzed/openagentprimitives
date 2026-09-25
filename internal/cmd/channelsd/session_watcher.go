// Watches AgentSessions with channel bindings and posts a threaded reply
// to the originating channel when one transitions to phase=Failed. Without
// this, channel users see silence on agent failures and assume the bot
// is broken.
//
// Implementation: 5s poll mirroring listeners.go's pattern. Tracks reported
// sessions in an in-memory LRU so we don't repeat-post during the polling
// loop.
//
// The in-memory half is a hot-path cache, never the whole dedup: a pod restart
// starts with a fresh map, and any signal that is BOTH durable and sticky (a
// terminal phase, a condition no reconcile path ever clears) is still sitting
// there to be re-observed — so the post fires again on every redeploy, forever.
// Every user-visible post below whose trigger outlives the process must
// therefore persist its dedup on the AgentSession: failureReportedAnnotation
// for phase=Failed, restartDeniedReportedAnnotation for RestartDenied. The
// transient notices (scheduling, startup wait) are exempt because their trigger
// clears itself — re-posting after a restart is correct there, since the
// condition is still actively true and the user is still waiting on it.
package main

import (
	"container/list"
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/outbound"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/pinnededit"
	"github.com/authzed/openagentprimitives/pkg/channels/notice"
	"github.com/authzed/openagentprimitives/pkg/channels/sessionnotice"
)

const (
	sessionWatcherInterval    = 5 * time.Second
	sessionWatcherReportedMax = 1024
	sessionFailureMessageMax  = 500
)

// schedulingWaitingText is the transient in-thread notice posted while the
// operator's durable SandboxScheduling condition is False (a bundle/detector
// pod stuck Pending on a scheduler failure past its grace period). It is
// cleared (empty-Text sentinel) once SandboxScheduling flips back to True.
// Shared with webd via sessionnotice so both surfaces name this state the same
// way; the glyph is channelsd's own presentation for a chat transport.
const schedulingWaitingText = "⏳ " + sessionnotice.SchedulingLead

// sessionWatcher polls AgentSessions and posts failure messages to the
// originating channel when a session transitions to phase=Failed.
type sessionWatcher struct {
	cli client.Client
	// senders is the outbound.SenderResolver interface (not the concrete
	// *senderResolver) so tests can inject a lightweight capturing fake
	// without standing up a real Channel CR + registered channelkinds.Kind.
	// Production wires the concrete *senderResolver from main.go, which
	// satisfies the interface structurally.
	senders outbound.SenderResolver
	wd      *statusWatchdog

	mu       sync.Mutex
	reported map[string]*list.Element // sessionUID → LRU element
	order    *list.List
	// idleHandled tracks (uid, LastIdleAt) tuples we've already cleaned up
	// so we don't re-Clear on every 5s tick. Keyed by UID, value is the
	// LastIdleAt timestamp we observed when we ran the cleanup. A new
	// LastIdleAt means the session woke and idled again; a fresh cleanup
	// is appropriate.
	idleHandled map[string]string
	// retryReported tracks (uid, RetryAttempts) tuples we've already
	// published the Retry button for. A new RetryAttempts value means
	// the runner re-entered AwaitingRetry; a fresh post is appropriate.
	retryReported map[string]int32
	// startupNoticeReported tracks session UIDs for which we've already posted a
	// startup notice (e.g. "starting the security scanner…"), so the 5s poll
	// posts it at most once per session while the runner is held coming up
	// (RunnerReady=False with a startupWaitReasons reason).
	startupNoticeReported map[string]bool
	// restartDeniedReported tracks (uid, RestartDenied LastTransitionTime)
	// tuples we've already reported. Keyed by UID; a new LastTransitionTime
	// means the user attempted another continuation and was denied again, which
	// is new information and gets its own post. Independent of
	// failureReportedAnnotation — a denied continuation is a separate event from
	// the session's own failure, and is usually observed on a session whose
	// failure was already reported.
	restartDeniedReported map[string]string
	// schedNoticePosted tracks session UIDs for which we've posted the
	// transient "waiting for capacity" notice for the CURRENT SandboxScheduling
	// stuck episode. Separate from failureReportedAnnotation/reported (which
	// dedup the terminal Failed message) — this dedups the transient in-thread
	// notice so a 5s-poll while the condition stays False doesn't repost, and
	// deletion-on-clear (SandboxScheduling flips back to True) resets it so a
	// later, unrelated stuck episode posts again.
	schedNoticePosted map[string]bool
	// pinnedApplied tracks (uid -> last-rendered hash) for the pinned opening
	// message edit, so an unchanged status doesn't re-issue chat.update every
	// 5s tick. The hash covers the full desired tuple (badge, body, link);
	// any change to any of the three is a new render worth pushing. The
	// terminal-final edit ALSO persists a durable annotation
	// (pinnedFinalAnnotation) — this in-memory map alone would repost the
	// final edit once per channelsd restart, same hazard as every other
	// terminal-trigger marker in this file.
	pinnedApplied map[string]string

	// publish is the NATS publish closure. Wired by main.go to nc.Publish;
	// tests override with a recording stub.
	publish func(subject string, data []byte) error
}

func newSessionWatcher(cli client.Client, senders outbound.SenderResolver, wd *statusWatchdog) *sessionWatcher {
	return &sessionWatcher{
		cli:                   cli,
		senders:               senders,
		wd:                    wd,
		reported:              map[string]*list.Element{},
		order:                 list.New(),
		idleHandled:           map[string]string{},
		retryReported:         map[string]int32{},
		startupNoticeReported: map[string]bool{},
		schedNoticePosted:     map[string]bool{},
		restartDeniedReported: map[string]string{},
		pinnedApplied:         map[string]string{},
	}
}

// Run polls until ctx is canceled. Call in a dedicated goroutine.
func (w *sessionWatcher) Run(ctx context.Context) {
	logger := log.FromContext(ctx).WithName("sessionwatcher")
	ticker := time.NewTicker(sessionWatcherInterval)
	defer ticker.Stop()

	w.reconcile(ctx, logger)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.reconcile(ctx, logger)
		}
	}
}

func (w *sessionWatcher) reconcile(ctx context.Context, logger logr.Logger) {
	var sessions spiceboxv1alpha1.AgentSessionList
	// Only fetch channel-attached sessions via the label selector.
	if err := w.cli.List(ctx, &sessions, client.HasLabels{spiceboxv1alpha1.LabelChannelName}); err != nil {
		logger.Error(err, "list agentsessions")
		return
	}
	w.pruneMarkers(sessions.Items)
	for i := range sessions.Items {
		sess := &sessions.Items[i]
		if sess.Spec.InputChannel == nil {
			continue
		}
		// channelsd surfaces status, failures, and the pinned opening-message
		// flip to the channel it POSTS to — the OUTPUT channel (the outbound
		// relay addresses OutputChannel, falling back to the input binding for a
		// same-channel session that leaves output unset). Decide the skip on
		// that same channel, NOT unconditionally on the input: a triggered
		// session is routinely cross-transport (a reviewbot review is github-in
		// / slack-out), and gating on the input (github, webd-hosted) skipped
		// the whole session — so its Slack thread never received the terminal
		// failure notice or the opening-flip, a silent hang. A client/host-side
		// output kind (webd for "browser", the CLI for "local") is surfaced by
		// that host process; channelsd has no transport for it, so resolving a
		// sender fails with `unknown kind` on every 5s tick. See clientHostedHere.
		surface := sess.Spec.OutputChannel
		if surface == nil {
			surface = sess.Spec.InputChannel
		}
		if surface == nil || clientHostedHere(surface.Kind) {
			continue
		}
		// Denied continuation: the user replied in this session's thread, was
		// acked with "…I'm picking it up in a new session…", and the operator's
		// SessionFork gate then refused it. Runs BEFORE every phase gate below —
		// a restart can be denied from any phase, and the common case is a
		// terminal session whose Failed message was already reported (so the
		// failure dedup further down would otherwise skip it entirely).
		w.maybeReportRestartDenied(ctx, logger, sess)
		// Terminal cleanup: drop watchdog tracking for a settled session.
		// The runner's final turn_activity pause folds into the machine as a
		// yield (Yielded set, not Forgotten — so a brief out-of-order caption
		// stays gated), but a Succeeded/Failed session never passes through the
		// Idle path below, so without this its Yielded State would linger in
		// the watchdog map for the process lifetime. Forget is idempotent
		// (a delete on an absent key is a no-op), so running it every tick for
		// terminal sessions is cheap.
		if w.wd != nil {
			switch sess.Status.Phase {
			case spiceboxv1alpha1.AgentSessionPhaseSucceeded,
				spiceboxv1alpha1.AgentSessionPhaseFailed:
				w.wd.Forget(sess.Namespace, sess.Name)
			}
		}
		// Live pinned opening message: re-render a triggered session's
		// thread-root status in place as it progresses (and once more, final,
		// on conclusion) — independent of every phase-gated branch below, since
		// a candidate session may be in ANY phase (Running, Idle, terminal).
		w.maybeEditOpeningMessage(ctx, logger, sess)
		// Idle cleanup: dismiss a stale "⚠️ Taking longer" indicator and
		// drop watchdog tracking when the session goes Idle without a
		// respond_to_user. Dedup'd by (UID, LastIdleAt) — a wake-and-idle
		// cycle gets a fresh cleanup.
		if sess.Status.Phase == spiceboxv1alpha1.AgentSessionPhaseIdle && w.wd != nil {
			w.maybeClearIdle(ctx, logger, sess)
		}
		// AwaitingCredentials: the listener's postStartingStatus Touched
		// the watchdog when the user's request arrived, but the agent
		// hasn't actually started running — it's parked waiting for the
		// user to click through identityd's link flow. The 30s "Taking
		// longer than expected" warn would fire spuriously here. Drop
		// tracking; the runner's first setStatus once creds land (or the
		// outbound relay's KindToolActivity Touch) re-tracks with fresh
		// state.
		if sess.Status.Phase == spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials && w.wd != nil {
			// Clear (not just Forget): a warn that fired before the session
			// parked for credentials must be torn down, not left stranded.
			if err := w.wd.Clear(ctx, sess.Namespace, sess.Name); err != nil {
				logger.Info("awaiting-credentials cleanup: watchdog Clear failed",
					"session", sess.Namespace+"/"+sess.Name, "err", err.Error())
			}
		}
		// Startup wait: the runner has not started yet because a startup
		// dependency is still coming up (today: a content-guard detector
		// loading its model; in future, other sidecars/preconditions).
		// waitFromStatus already classifies this as a visible wait so the
		// silence watchdog stays quiet; post a one-time notice so the user
		// knows why there's a brief pause. Generic over the reason.
		if notice, ok := startupWait(sess); ok {
			w.maybePostStartupNotice(ctx, logger, sess, notice)
		}
		// Sandbox scheduling: the operator's durable SandboxScheduling condition
		// is the channelsd-visible twin of a bundle/detector pod stuck Pending on
		// a scheduler failure. It is only meaningful while the session is still
		// bootstrapping/working a live turn (Pending or Running) — a parked or
		// terminal session has nothing in-flight for the user to wait on. Absent
		// condition means no scheduling problem was ever observed; do nothing.
		if sess.Status.Phase == spiceboxv1alpha1.AgentSessionPhasePending ||
			sess.Status.Phase == spiceboxv1alpha1.AgentSessionPhaseRunning {
			if cond := meta.FindStatusCondition(sess.Status.Conditions,
				spiceboxv1alpha1.AgentSessionConditionSandboxScheduling); cond != nil {
				w.handleSandboxScheduling(ctx, logger, sess, cond)
			}
		}
		if sess.Status.Phase == spiceboxv1alpha1.AgentSessionPhaseAwaitingRetry {
			if w.publish == nil {
				// Tests that don't wire publish are explicit-opt-in; production
				// always sets it via main.go. Skip silently in that case.
				continue
			}
			uid := string(sess.UID)
			attempt := strconv.Itoa(int(sess.Status.RetryAttempts))
			w.mu.Lock()
			prev, seen := w.retryReported[uid]
			alreadyAtSameAttempt := seen && prev == sess.Status.RetryAttempts
			w.mu.Unlock()
			if alreadyAtSameAttempt {
				continue
			}
			// Durable half: AwaitingRetry persists until the user clicks Retry
			// or the 30-minute TTL expires the session, so a channelsd restart
			// inside that window would otherwise post a second Retry card for
			// the same attempt. Unlike the runner's own re-publish path, this
			// one goes out via PublishOut straight to the outbound relay, so
			// HandleInteractionRequest's PendingInteractions dedup — which
			// exists for exactly this hazard — never sees it.
			if sess.Annotations[retryPromptReportedAnnotation] == attempt {
				w.mu.Lock()
				w.retryReported[uid] = sess.Status.RetryAttempts
				w.mu.Unlock()
				continue
			}
			if err := w.publishRetryPrompt(ctx, sess); err != nil {
				logger.Error(err, "publish provider_error_retry envelope",
					"session", sess.Namespace+"/"+sess.Name,
					"attempt", sess.Status.RetryAttempts)
				continue
			}
			w.mu.Lock()
			w.retryReported[uid] = sess.Status.RetryAttempts
			w.mu.Unlock()
			if err := w.stampAnnotation(ctx, sess, retryPromptReportedAnnotation, attempt); err != nil {
				logger.Error(err, "persist retry-prompt-reported annotation",
					"session", sess.Namespace+"/"+sess.Name, "attempt", attempt)
			}
			continue
		}
		if sess.Status.Phase != spiceboxv1alpha1.AgentSessionPhaseFailed {
			continue
		}
		uid := string(sess.UID)
		// Annotation-persisted dedup survives pod restarts — without this,
		// every channelsd restart re-posts every Failed session's message
		// to the channel.
		if sess.Annotations[failureReportedAnnotation] != "" {
			w.markReported(uid) // populate in-memory cache for future ticks
			continue
		}
		if w.alreadyReported(uid) {
			continue
		}
		if err := w.reportFailure(sess); err != nil {
			logger.Error(err, "report session failure",
				"session", sess.Namespace+"/"+sess.Name)
			continue
		}
		w.markReported(uid)
		if err := w.persistReported(ctx, sess); err != nil {
			// Non-fatal — in-memory cache prevents re-post within this
			// pod lifetime; persist failure means a restart will re-post
			// once. Log but don't retry.
			logger.Error(err, "persist failure-reported annotation",
				"session", sess.Namespace+"/"+sess.Name)
		}
		logger.Info("reported session failure to channel",
			"session", sess.Namespace+"/"+sess.Name,
			"reason", sess.Status.FailureReason)
	}
}

// persistReported stamps an annotation on the AgentSession so a restart
// of channelsd doesn't re-post the failure message. Treat any other
// processes' updates as authoritative — we use TS-of-now as the value
// purely for human readability; presence-of-key is the dedup signal.
func (w *sessionWatcher) persistReported(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) error {
	return w.stampAnnotation(ctx, sess, failureReportedAnnotation,
		time.Now().UTC().Format(time.RFC3339))
}

// stampAnnotation merge-patches a single annotation onto sess. One key per
// patch, so it neither races nor clobbers concurrent writers of other keys.
//
// This is how every post whose trigger outlives the channelsd process records
// that it already fired — see the package header. Callers pass a value derived
// from the trigger (a condition's LastTransitionTime, an attempt counter) so
// re-stamping an unchanged trigger is a no-op; failureReportedAnnotation is the
// exception that predates the rule, and gets away with a wall-clock value
// because presence alone is its dedup signal.
func (w *sessionWatcher) stampAnnotation(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, key, value string) error {
	patch := []byte(fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, key, value))
	return w.cli.Patch(ctx,
		&spiceboxv1alpha1.AgentSession{
			ObjectMeta: metav1.ObjectMeta{
				Name: sess.Name, Namespace: sess.Namespace,
			},
		},
		client.RawPatch(types.MergePatchType, patch),
	)
}

// retryPromptReportedAnnotation records the RetryAttempts value whose Retry
// prompt channelsd has already published, so a restart inside the AwaitingRetry
// window does not put a second live Retry button in the thread. Valued by the
// attempt rather than presence: the next attempt is a new prompt.
const retryPromptReportedAnnotation = "agentprimitives.authzed.com/retry-prompt-reported"

// failureReportedAnnotation marks a session whose failure has already
// been reported to its originating channel. Persisted on the AgentSession
// so it survives channelsd restarts.
const failureReportedAnnotation = "slack.agentprimitives.authzed.com/failure-reported"

// pinnedFinalAnnotation records the OpeningBadge value of the terminal-final
// pinned-opening-message edit channelsd has already applied. Valued by the
// badge rather than presence: unlike failureReportedAnnotation's one-shot
// event, a session can be re-derived from a different terminal badge across
// restarts (e.g. a later capture picks up the same phase with a different
// recorded outcome), and re-editing to a genuinely different final badge is
// still correct — only a re-observation of the SAME badge must be suppressed.
const pinnedFinalAnnotation = "agentprimitives.authzed.com/pinned-badge-final"

// maybeClearIdle issues a one-time watchdog Clear when sess is observed
// in Phase=Idle for the first time (or after a wake-and-idle cycle). The
// session.status.lastIdleAt timestamp serves as the dedup key — a new
// value means the session re-idled and is eligible for a fresh cleanup.
func (w *sessionWatcher) maybeClearIdle(ctx context.Context, logger logr.Logger, sess *spiceboxv1alpha1.AgentSession) {
	uid := string(sess.UID)
	lastIdleAt := ""
	if sess.Status.LastIdleAt != nil {
		lastIdleAt = sess.Status.LastIdleAt.UTC().Format(time.RFC3339Nano)
	}
	w.mu.Lock()
	prev, ok := w.idleHandled[uid]
	w.mu.Unlock()
	if ok && prev == lastIdleAt {
		return
	}
	// Mark AFTER the Clear succeeds, like every sibling marker in this file.
	// Marking first stranded a failed Clear forever: LastIdleAt does not change
	// while the session sits Idle, so the dedup key never moves and no later
	// tick ever retries — the "⚠️ Taking longer" indicator stays up on a
	// session that has stopped working, which is the one thing this cleanup
	// exists to prevent.
	if err := w.wd.Clear(ctx, sess.Namespace, sess.Name); err != nil {
		logger.Info("idle cleanup: watchdog Clear failed; retrying next tick",
			"session", sess.Namespace+"/"+sess.Name, "err", err.Error())
		return
	}
	w.mu.Lock()
	w.idleHandled[uid] = lastIdleAt
	w.mu.Unlock()
}

// maybeEditOpeningMessage re-renders sess's live pinned opening message —
// the triggered session's thread-root status line — in place, when the
// desired rendering (pinnededit.Desired) has changed since the last edit
// this process applied.
//
// Two dedup layers, for two different failure modes. The in-memory
// pinnedApplied hash is the hot path: it skips the chat.update round-trip on
// every 5s tick while the session's status is unchanged, which is the common
// case for a long-running scan. It alone is not enough for the terminal-final
// edit, though — a channelsd restart starts with an empty map, and a
// terminal phase is durable and sticky (nothing ever clears Failed/Succeeded),
// so without a persisted marker every redeploy would re-issue the final edit
// once per terminal session it still lists. pinnedFinalAnnotation is that
// persisted marker, mirroring failureReportedAnnotation's role for the
// failure notice elsewhere in this file.
//
// An edit failure is logged and NOT memoized (neither layer), so the next
// tick retries — the same "log, don't strand a real failure behind a marker"
// discipline maybeClearIdle uses above.
func (w *sessionWatcher) maybeEditOpeningMessage(ctx context.Context, logger logr.Logger, sess *spiceboxv1alpha1.AgentSession) {
	content, ok := pinnededit.Desired(sess)
	if !ok {
		return // no anchored opening message on this session; nothing to edit
	}
	terminal := sess.Status.Phase == spiceboxv1alpha1.AgentSessionPhaseFailed ||
		sess.Status.Phase == spiceboxv1alpha1.AgentSessionPhaseSucceeded

	if terminal && sess.Annotations[pinnedFinalAnnotation] == string(content.Badge) {
		return // durable dedup: this exact terminal badge was already applied
	}

	uid := string(sess.UID)
	hash := fmt.Sprintf("%s\x00%s\x00%s", content.Badge, content.Body, content.Link)
	w.mu.Lock()
	prev, seen := w.pinnedApplied[uid]
	w.mu.Unlock()
	if seen && prev == hash {
		return // hot-path dedup: identical to the last render this process applied
	}

	sender, err := w.senders.SenderFor(ctx, sess)
	if err != nil {
		logger.Info("pinned opening edit: resolve sender failed",
			"session", sess.Namespace+"/"+sess.Name, "err", err.Error())
		return
	}
	if sender == nil {
		return // client-hosted kind; SenderFor's documented (nil, nil) drop sentinel
	}
	editor, ok := sender.(channelkinds.OpeningMessageEditor)
	if !ok {
		return // output kind cannot edit an already-posted message; no-op
	}

	if err := editor.EditOpeningMessage(ctx, channelkinds.SessionInfo{
		Namespace:   sess.Namespace,
		Name:        sess.Name,
		Channel:     sess.Spec.OutputChannel,
		Annotations: sess.Annotations,
	}, content); err != nil {
		logger.Info("pinned opening edit failed; retrying next tick",
			"session", sess.Namespace+"/"+sess.Name, "err", err.Error())
		return // do not memo a failed edit
	}

	w.mu.Lock()
	w.pinnedApplied[uid] = hash
	w.mu.Unlock()
	if terminal {
		if err := w.stampAnnotation(ctx, sess, pinnedFinalAnnotation, string(content.Badge)); err != nil {
			logger.Info("pinned opening edit: stamping final annotation failed",
				"session", sess.Namespace+"/"+sess.Name, "err", err.Error())
		}
	}
}

// handleSandboxScheduling reacts to sess's SandboxScheduling condition,
// posting/clearing the transient "waiting for capacity" in-thread notice.
// Dedup is by UID and is INDEPENDENT of the operator's condition
// LastTransitionTime — it tracks whether WE have already posted for the
// current stuck episode, not how many times the condition itself flapped, so
// a 5s poll while SandboxScheduling stays False does not repost every tick.
// False → post once; True after a prior post → clear once and forget the UID
// so a later, unrelated stuck episode is eligible to post again; True with no
// prior post is a no-op (nothing to clear).
func (w *sessionWatcher) handleSandboxScheduling(ctx context.Context, logger logr.Logger, sess *spiceboxv1alpha1.AgentSession, cond *metav1.Condition) {
	uid := string(sess.UID)
	switch cond.Status {
	case metav1.ConditionFalse:
		w.mu.Lock()
		already := w.schedNoticePosted[uid]
		w.mu.Unlock()
		if already {
			return
		}
		if err := w.postSchedulingNotice(ctx, sess, schedulingWaitingText); err != nil {
			logger.Error(err, "sandbox-scheduling notice: post", "session", sess.Namespace+"/"+sess.Name)
			return
		}
		w.mu.Lock()
		w.schedNoticePosted[uid] = true
		w.mu.Unlock()
	case metav1.ConditionTrue:
		w.mu.Lock()
		wasPosted := w.schedNoticePosted[uid]
		delete(w.schedNoticePosted, uid)
		w.mu.Unlock()
		if !wasPosted {
			return // never posted for this episode; nothing to clear
		}
		if err := w.postSchedulingNotice(ctx, sess, ""); err != nil {
			logger.Error(err, "sandbox-scheduling notice: clear", "session", sess.Namespace+"/"+sess.Name)
		}
	}
}

// postSchedulingNotice sends a KindNotification with the given text to sess's
// originating channel. An empty text is the clear sentinel (mirrors
// status_watchdog.go's clearIndicator). A sender-resolution failure (no
// listener registered for the channel kind, etc.) is logged and returned —
// never dropped silently — so the caller decides whether to retry the dedup
// state.
//
// This one keeps the DEFAULT sender, unlike the notice paths: a
// KindNotification IS in every default sender's switch — it is the transient
// status-caption surface, which is exactly what a "waiting for capacity" wait
// belongs on.
func (w *sessionWatcher) postSchedulingNotice(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, text string) error {
	sender, err := w.senders.SenderFor(ctx, sess)
	if err != nil {
		return fmt.Errorf("resolve sender: %w", err)
	}
	if sender == nil {
		// Client-hosted (browser-view / builtin) kind: SenderFor's documented
		// (nil, nil) drop sentinel. reconcile's clientHostedHere guard skips
		// most of these, but it reads the BINDING's kind while the resolver
		// reads the Channel CR's, so the two can disagree. Never dereference
		// the nil interface (that Send is what crash-looped channelsd).
		return nil
	}
	env, err := channelevents.BuildEnvelope(sess.Namespace, sess.Name,
		channelevents.KindNotification,
		channelevents.NotificationPayload{Text: text})
	if err != nil {
		return fmt.Errorf("build envelope: %w", err)
	}
	_, err = sender.Send(ctx, channelkinds.SessionInfo{
		Namespace: sess.Namespace, Name: sess.Name,
		Channel: spiceboxv1alpha1.OutboundBinding(sess),
	}, env)
	return err
}

// maybePostStartupNotice posts a startup notice to sess's channel at most once
// per session (dedup by UID). It is sent as a KindNotification, which renders
// on the channel's ephemeral status surface (the same surface the watchdog
// warns use), so the runner's first setStatus replaces it instead of leaving a
// lingering chat message. Best-effort: failures are logged, never fatal.
func (w *sessionWatcher) maybePostStartupNotice(ctx context.Context, logger logr.Logger, sess *spiceboxv1alpha1.AgentSession, notice string) {
	if sess.Spec.InputChannel == nil {
		return // kubectl-driven; nothing to post
	}
	uid := string(sess.UID)
	w.mu.Lock()
	already := w.startupNoticeReported[uid]
	w.mu.Unlock()
	if already {
		return
	}
	sender, err := w.senders.SenderFor(ctx, sess)
	if err != nil {
		logger.Error(err, "startup notice: resolve sender", "session", sess.Namespace+"/"+sess.Name)
		return
	}
	if sender == nil {
		return // client-hosted kind; SenderFor's (nil, nil) drop sentinel
	}
	env, err := channelevents.BuildEnvelope(sess.Namespace, sess.Name,
		channelevents.KindNotification,
		channelevents.NotificationPayload{Text: notice})
	if err != nil {
		logger.Error(err, "startup notice: build envelope", "session", sess.Namespace+"/"+sess.Name)
		return
	}
	if _, err := sender.Send(ctx, channelkinds.SessionInfo{
		Namespace: sess.Namespace, Name: sess.Name,
		Channel: spiceboxv1alpha1.OutboundBinding(sess),
	}, env); err != nil {
		logger.Error(err, "startup notice: send", "session", sess.Namespace+"/"+sess.Name)
		return
	}
	w.mu.Lock()
	w.startupNoticeReported[uid] = true
	w.mu.Unlock()
}

// maybeReportRestartDenied posts a one-time in-thread message when the
// operator's durable RestartDenied condition is True.
//
// This is the user-visible half of a refused thread continuation. channelsd
// already told the user "That conversation had already finished, so I'm picking
// it up in a new session — same thread, full history."; if the fork gate then
// says no, the user is left waiting on a session that will never appear. The
// operator does publish a requester notice on out.metaagent_notice, but that
// path is best-effort raw JSON on a subject the wildcard outbound relay also
// consumes and rejects ("unsupported envelope version 0"), so it cannot be the
// thing the user depends on. The condition is durable; this relays it.
//
// Sent as a durable notice, NOT a transient status caption: the answer to
// "why did nothing happen?" must still be there when the user scrolls back,
// and must not be overwritten by whatever the session says next.
//
// Dedup is by (UID, LastTransitionTime): the 5s poll must not repost, but a
// second denial — the user tried again — is new information and posts again.
// The stamp is persisted to restartDeniedReportedAnnotation so the dedup
// survives a channelsd restart; see that constant for why in-memory alone is
// not enough here.
func (w *sessionWatcher) maybeReportRestartDenied(ctx context.Context, logger logr.Logger, sess *spiceboxv1alpha1.AgentSession) {
	cond := meta.FindStatusCondition(sess.Status.Conditions,
		spiceboxv1alpha1.AgentSessionConditionRestartDenied)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		return
	}
	uid := string(sess.UID)
	stamp := cond.LastTransitionTime.UTC().Format(time.RFC3339Nano)
	w.mu.Lock()
	prev, seen := w.restartDeniedReported[uid]
	w.mu.Unlock()
	if seen && prev == stamp {
		return
	}
	// Durable half of the dedup. Checked after the in-memory map purely as an
	// ordering preference (the map is the hot path); either one hitting means
	// this exact denial already reached the thread.
	if sess.Annotations[restartDeniedReportedAnnotation] == stamp {
		w.mu.Lock()
		w.restartDeniedReported[uid] = stamp // populate in-memory cache for future ticks
		w.mu.Unlock()
		return
	}

	// Keyed by the denial's own stamp: a second denial is a second notice, and
	// the two must not share one interaction identity.
	if err := w.publishNotice(sess, "notice-"+categories.ContinuationDenied+"-"+string(sess.UID)+"-"+stamp,
		buildRestartDeniedNotice(cond.Reason, cond.Message)); err != nil {
		logger.Error(err, "restart-denied report: publish",
			"session", sess.Namespace+"/"+sess.Name)
		return
	}
	w.mu.Lock()
	w.restartDeniedReported[uid] = stamp
	w.mu.Unlock()
	if err := w.persistRestartDeniedReported(ctx, sess, stamp); err != nil {
		// Non-fatal, mirroring persistReported: the in-memory map still
		// suppresses reposts for this pod's lifetime, so a persist failure
		// costs one duplicate post after the next restart — strictly better
		// than failing the report the user is waiting on.
		logger.Error(err, "persist restart-denied-reported annotation",
			"session", sess.Namespace+"/"+sess.Name)
	}
	logger.Info("reported denied continuation to channel",
		"session", sess.Namespace+"/"+sess.Name, "reason", cond.Reason)
}

// restartDeniedReportedAnnotation records the RestartDenied LastTransitionTime
// channelsd has already relayed to the thread.
//
// In-memory dedup alone is not sufficient for this signal, unlike the transient
// notices above. RestartDenied is written True in exactly one place
// (pkg/controllers/agentsession/restart.go, the fork gate) and is NEVER cleared
// — no reconcile path sets it False or removes it. So the trigger outlives the
// process that reported it: every channelsd redeploy started with a fresh map,
// re-observed the same sticky condition on every session that was ever denied,
// and re-posted "I couldn't continue this conversation" to those threads. Once
// per redeploy, forever.
//
// The value is the condition's LastTransitionTime, NOT a presence marker (the
// difference from failureReportedAnnotation, whose event can only happen once
// per session). A genuinely new denial carries a new stamp and must still
// reach the user across a restart.
//
// Deriving the value from the condition rather than time.Now() also keeps the
// patch idempotent: re-applying it for an unchanged denial is a no-op.
const restartDeniedReportedAnnotation = "agentprimitives.authzed.com/restart-denied-reported"

// persistRestartDeniedReported stamps the reported denial's LastTransitionTime
// onto the AgentSession. A merge patch of one annotation, so it neither races
// nor clobbers concurrent writers of other keys.
func (w *sessionWatcher) persistRestartDeniedReported(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, stamp string) error {
	return w.stampAnnotation(ctx, sess, restartDeniedReportedAnnotation, stamp)
}

// buildRestartDeniedMessage formats the user-facing text for a refused
// continuation. The condition Message carries the gate's requester-facing
// wording; the Reason is the fallback so the message is never a bare prefix
// with nothing after it.
func buildRestartDeniedNotice(reason, message string) *notice.Notice {
	// The condition Message carries the gate's own requester-facing wording;
	// Reason is the fallback so the notice is never a bare header with nothing
	// behind it.
	detail := strings.TrimSpace(message)
	if detail == "" {
		detail = reason
	}
	if detail == "" {
		detail = "the request was not authorized"
	}
	return notice.New(categories.ContinuationDenied, notice.Args{
		Lead:     "Couldn't continue this conversation",
		NextStep: "Start a new thread if you still need this.",
		Excerpt:  &channelevents.InteractionExcerpt{Label: "Reason", Content: detail},
		Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceParticipants},
	})
}

// reportFailure publishes the terminal "the agent stopped" notice for sess.
//
// An error here means the user did NOT get the message, so the caller must not
// mark the session reported — the next tick retries. With the notice on the
// publish path that is a sound policy: a failure is a transient bus problem,
// not the permanent every-5s-forever re-failure that routing it to a default
// sender (which can never render an interaction_request) produced.
func (w *sessionWatcher) reportFailure(sess *spiceboxv1alpha1.AgentSession) error {
	reason := sess.Status.FailureReason
	if reason == "" {
		reason = "unknown"
	}
	// Pick the most recent Failed condition's message for additional context.
	var condMsg string
	for _, c := range sess.Status.Conditions {
		if c.Type == spiceboxv1alpha1.AgentSessionConditionFailed && c.Status == "True" {
			condMsg = c.Message
			break
		}
	}
	// A session fails once, so its UID alone identifies the notice.
	return w.publishNotice(sess, "notice-"+categories.AgentFailed+"-"+string(sess.UID),
		buildFailureNotice(reason, condMsg))
}

// buildFailureNotice composes the user-facing message for a session that died.
//
// The failure reason and the condition message are both machine-generated —
// controller text, sometimes a whole stack trace — so they travel in the
// Excerpt, which every surface renders inert. The lead and next step are
// written for a human who wants to know whether to start over.
func buildFailureNotice(reason, msg string) *notice.Notice {
	args := notice.Args{
		Lead:     "The agent stopped and can't continue",
		NextStep: "Start a new thread to try again.",
		Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceParticipants},
	}
	detail := strings.TrimSpace(msg)
	if detail == "" {
		detail = reason
	} else if reason != "" {
		detail = reason + ": " + detail
	}
	if detail != "" {
		// Truncated because a failure message is sometimes an entire stack
		// trace, and a notice that buries its own next step is no help.
		if len(detail) > sessionFailureMessageMax {
			detail = detail[:sessionFailureMessageMax] + "…"
		}
		args.Excerpt = &channelevents.InteractionExcerpt{Label: "Reason", Content: detail}
	}
	return notice.New(categories.AgentFailed, args)
}

// publishNotice puts a notice on the session's OUT subject, where the outbound
// relay picks it up and renders it through the channel kind's "interaction"
// sub-channel sender.
//
// A notice CANNOT be handed to the default Sender. It rides
// KindInteractionRequest, and a kind's default Sender implements only the
// status/notification kinds — slack's answers an interaction_request with
// "unsupported envelope kind" and drops it, which is how "The agent stopped and
// can't continue" and "Couldn't continue this conversation" reached no one. Only
// the "interaction" sub-channel sender renders one, and only the relay resolves
// that sender, so publishing is the ONLY delivery path. Every other notice
// publisher in channelsd (the pipeline's agent-unavailable and attachment
// notices, this file's retry prompt) already goes out this way.
//
// Publishing also hands the relay three things a direct Send here cannot: it
// addresses the session's OutboundBinding (so a split-channel cron/bento
// session reaches its OUTPUT channel), it stamps SessionInitiator, and it
// carries the session annotations the sub-channel senders read.
//
// requestRef must identify this specific notice — the slack interaction sender
// keys its delivery handles by it, so two different notices on one session must
// not share one. Callers derive it from the notice's own trigger (see
// stampAnnotation for the same discipline), which keeps a re-publish of an
// unchanged trigger addressable as the same interaction.
//
// A nil publish func is a wiring bug, and notice.Publish reports it as an
// error rather than dropping the message — never a silent no-op.
func (w *sessionWatcher) publishNotice(sess *spiceboxv1alpha1.AgentSession, requestRef string, n *notice.Notice) error {
	return n.Publish(w.publish,
		channelevents.SessionRef{Namespace: sess.Namespace, Name: sess.Name},
		requestRef)
}

// publishRetryPrompt emits a generic interaction_request(provider_error_retry)
// envelope on the OUT subject for sess. The outbound relay renders it through
// the channel kind's unified "interaction" sub-channel — Slice B migrated
// provider_error_retry off its bespoke KindProviderErrorRetry sub-channel onto
// the Interaction model (DecideParticipant + AudienceParticipants broadcast).
//
// The prompt is an AudienceParticipants broadcast: the Retry button is a generic
// ActionKindDecision visible to every session participant, and decision authz
// (who may click it) is re-checked server-side by the category's
// DecideParticipant policy — this publisher addresses no single requester.
//
// The Body reproduces the legacy Slack buildRetryPromptText EXACTLY minus its
// reason line, which becomes the Lead (the generic renderer bolds the Lead — the
// one accepted cosmetic delta of this migration). The AwaitingRetry condition's
// Message is the truncated err.Error() the runner captured.
func (w *sessionWatcher) publishRetryPrompt(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) error {
	cond := meta.FindStatusCondition(sess.Status.Conditions,
		spiceboxv1alpha1.AgentSessionConditionAwaitingRetry)
	message := ""
	if cond != nil {
		message = cond.Message
	}
	// Body = fenced, ≤500-char-truncated message (if any) + the "Click Retry"
	// trailer. sessionFailureMessageMax (500) matches the legacy
	// retryPromptMaxMsgLen the Slack sender truncated at.
	body := ""
	if trimmed := strings.TrimSpace(message); trimmed != "" {
		if len(trimmed) > sessionFailureMessageMax {
			trimmed = trimmed[:sessionFailureMessageMax] + "…"
		}
		body = "```\n" + trimmed + "\n```\n"
	}
	body += "Click Retry to try again."

	req := channelevents.InteractionRequestPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: sess.Namespace, Name: sess.Name},
		Category:        categories.ProviderErrorRetry,
		// Stable, unique per (session, attempt): a new AwaitingRetry attempt
		// (RetryAttempts bump) re-posts a fresh prompt, matching the watcher's
		// own once-per-attempt dedup.
		RequestRef: fmt.Sprintf("provider-error-retry-%s-%s-%d", sess.Namespace, sess.Name, sess.Status.RetryAttempts),
		Lead:       "⚠️ Agent failed: " + sess.Status.FailureReason,
		Body:       body,
		Actions: []channelevents.InteractionAction{{
			ID:    "retry",
			Label: "Retry",
			Style: channelevents.ActionStylePrimary,
			Kind:  channelevents.ActionKindDecision,
		}},
		Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceParticipants},
	}
	if err := req.Validate(); err != nil {
		return fmt.Errorf("provider_error_retry: built an invalid interaction_request payload (session %s/%s): %w",
			sess.Namespace, sess.Name, err)
	}
	return channelevents.PublishOut(w.publish, sess.Namespace, sess.Name,
		channelevents.KindInteractionRequest, req)
}

// pruneMarkers drops every per-session marker whose AgentSession is no longer
// in the watcher's List — i.e. was deleted or GC'd.
//
// These maps are keyed by UID and only ever grown at their post sites, so
// without this they accumulate one entry per session for the life of the
// process; a long-lived channelsd on a busy cluster leaks them all. `reported`
// is exempt because its LRU (markReported) already caps it at
// sessionWatcherReportedMax and its map is coupled to `order`.
//
// Safe to drop a marker for an absent session: a UID is never reused, so a
// pruned entry can never suppress a future post. The two posts whose trigger
// outlives the process are backed by durable annotations regardless.
//
// Called from reconcile, which is single-goroutine (Run's ticker), so the List
// it prunes against is always the one about to be processed.
func (w *sessionWatcher) pruneMarkers(items []spiceboxv1alpha1.AgentSession) {
	live := make(map[string]struct{}, len(items))
	for i := range items {
		live[string(items[i].UID)] = struct{}{}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for uid := range w.idleHandled {
		if _, ok := live[uid]; !ok {
			delete(w.idleHandled, uid)
		}
	}
	for uid := range w.retryReported {
		if _, ok := live[uid]; !ok {
			delete(w.retryReported, uid)
		}
	}
	for uid := range w.startupNoticeReported {
		if _, ok := live[uid]; !ok {
			delete(w.startupNoticeReported, uid)
		}
	}
	for uid := range w.restartDeniedReported {
		if _, ok := live[uid]; !ok {
			delete(w.restartDeniedReported, uid)
		}
	}
	// schedNoticePosted is normally cleared when the episode ends
	// (SandboxScheduling flips True), but a session deleted mid-episode never
	// reaches that branch.
	for uid := range w.schedNoticePosted {
		if _, ok := live[uid]; !ok {
			delete(w.schedNoticePosted, uid)
		}
	}
	for uid := range w.pinnedApplied {
		if _, ok := live[uid]; !ok {
			delete(w.pinnedApplied, uid)
		}
	}
}

// alreadyReported reports whether uid has been posted and updates LRU order.
func (w *sessionWatcher) alreadyReported(uid string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if el, ok := w.reported[uid]; ok {
		w.order.MoveToFront(el)
		return true
	}
	return false
}

// markReported records uid as posted, evicting the oldest entry when the LRU
// exceeds sessionWatcherReportedMax.
func (w *sessionWatcher) markReported(uid string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if el, ok := w.reported[uid]; ok {
		w.order.MoveToFront(el)
		return
	}
	el := w.order.PushFront(uid)
	w.reported[uid] = el
	for w.order.Len() > sessionWatcherReportedMax {
		old := w.order.Back()
		if old == nil {
			break
		}
		w.order.Remove(old)
		delete(w.reported, old.Value.(string))
	}
}

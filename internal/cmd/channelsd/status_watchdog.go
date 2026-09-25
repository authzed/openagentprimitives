// Per-session "is the agent silently stuck?" watchdog. The DECISION logic lives
// in pkg/channels/channelsd/watchdog (a pure, table-tested state machine); this file is
// the I/O shell: it owns the per-session map, the tick loop, and the Slack/k8s
// calls, and feeds the machine two kinds of signal —
//
//   - Activity / Extend: the agent emitted progress (setStatus, tool_activity,
//     plan_update) or declared an expected_duration. Pushes the deadline out.
//   - waitFromStatus: at decision time (only for a session already silent past
//     the warn threshold), the session's status classifies the wait — active,
//     visibly-waiting (idle / tool approval / permission / retry — the channel
//     already shows why, so stay silent), or an info-leakage approval (whose
//     prompt is a private ephemeral the requester can't see, so announce it).
//
// Defaults: warn at 60s, stall at 120s (see watchdog.Default). The watchdog is
// in-memory only; channelsd restarts wipe state and the next inbound re-arms it.
package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/dustin/go-humanize"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/watchdog"
	"github.com/authzed/openagentprimitives/pkg/channels/notice"
	"github.com/authzed/openagentprimitives/pkg/channels/sessionnotice"
)

const statusWatchdogTickInterval = 5 * time.Second

// Watchdog message text. Plain — no leading emoji (the warning glyph read as
// alarmist for what is usually a benign slow operation).
const (
	watchdogWarnText = "Taking longer than expected — still working…"

	// watchdogLeakageText is shown when the agent is paused on an info-leakage
	// approval. That approval prompt is delivered only as a private ephemeral
	// to the approver(s), so the requester and the rest of the thread would
	// otherwise see unexplained silence.
	watchdogLeakageText = "Paused — waiting for approval to share data with a channel member who lacks access to the underlying data…"

	// genericStartupText is the fallback startup notice for a benign
	// RunnerReady=False wait whose reason has no specific text in
	// startupWaitReasons.
	genericStartupText = sessionnotice.GenericStartupLead
)

// startupWaitReasons enumerates the RunnerReady=False reasons that represent a
// benign, bounded "still bringing up the session" wait: the agent has not
// started yet because a startup dependency is still coming up. The operator
// owns each reason's own deadline / fast-fail, so the silence watchdog must NOT
// fire its generic "taking longer" warn over these — instead channelsd posts a
// one-time startup notice. This map is the single extension point: a new
// startup dependency (another sidecar, some other precondition) is wired up by
// adding its reason + user-facing text here. RunnerReady=False reasons that are
// NOT benign startup (e.g. RunnerCrashed) are deliberately absent so they keep
// their normal watchdog/failure handling.
// Delegated to pkg/channels/sessionnotice so channelsd and webd cannot drift
// into describing one state two ways: channelsd publishes this text to a
// channel, webd renders the same derivation inline for client-hosted sessions
// its watcher deliberately skips.
var startupWaitReasons = sessionnotice.StartupWaitReasons

// statusWatchdog tracks per-session timing and fires warn / timeout actions
// when the agent goes silent while actively working.
// senderProvider is the subset of *senderResolver the status watchdog needs.
// SenderFor returns (nil, nil) for a CLIENT-HOSTED (browser-view / builtin)
// channel kind — every caller MUST nil-check the returned sender before calling
// Send, or it dereferences a nil interface (the crash this guard exists to
// prevent). Declared as an interface so tests can inject a resolver that yields
// a nil sender without standing up the whole channel-kind machinery.
type senderProvider interface {
	SenderFor(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) (channelkinds.Sender, error)
}

type statusWatchdog struct {
	cli     client.Client
	senders senderProvider
	cfg     watchdog.Config

	// publish is the NATS publish closure, wired by main.go to nc.Publish.
	// It carries the watchdog's NOTICES (not its captions): a notice is a
	// KindInteractionRequest, which only a channel kind's "interaction"
	// sub-channel sender renders, and only the outbound relay resolves that
	// sender — so a notice reaches a user by being published, never by being
	// handed to the default Sender. See sendNotice.
	publish func(subject string, data []byte) error

	// clock is the single time source for every deadline reference the shell
	// stamps (Touch / Extend / ApplyEvent / tick). Injected so timing tests can
	// drive the warn/timeout windows with a fake clock instead of wall-time
	// sleeps. Defaults to clock.RealClock{}.
	clock clock.Clock

	mu    sync.Mutex
	state map[string]*watchdog.State // key = "ns/name" of AgentSession
	// stalls counts CONSECUTIVE timeout notices per session. It deliberately
	// outlives `state` (which the timeout branch Forgets) so a re-armed session
	// that stalls again is reported as a re-check rather than a fresh discovery.
	// Reset the moment the runner shows real progress.
	stalls map[string]int
}

func newStatusWatchdog(cli client.Client, senders senderProvider) *statusWatchdog {
	return &statusWatchdog{
		cli:     cli,
		senders: senders,
		cfg:     watchdog.Default(),
		clock:   clock.RealClock{},
		state:   map[string]*watchdog.State{},
		stalls:  map[string]int{},
	}
}

// bumpStall increments and returns the session's consecutive-stall count.
func (w *statusWatchdog) bumpStall(key string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stalls[key]++
	return w.stalls[key]
}

// resetStall clears the consecutive-stall streak. Called wherever the runner
// demonstrably came back (a live turn) or the session left the running state
// (Idle, terminal, awaiting credentials, deleted).
func (w *statusWatchdog) resetStall(key string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.stalls, key)
}

func wdKey(ns, name string) string { return ns + "/" + name }

// Touch records that a setStatus / tool_activity / plan_update just landed for
// the session — forward progress that re-arms the silence window.
func (w *statusWatchdog) Touch(ns, name string) {
	if w == nil {
		return
	}
	now := w.clock.Now()
	w.mu.Lock()
	defer w.mu.Unlock()
	st := w.state[wdKey(ns, name)]
	if st == nil {
		st = &watchdog.State{}
		w.state[wdKey(ns, name)] = st
	}
	st.Activity(now)
}

// Extend applies the agent's expected_duration hint, pushing the silence
// deadline forward (with the machine's 2× fudge + cap). A non-positive dur is a
// no-op. Creates tracking state if absent so a hint that precedes any setStatus
// still arms the window.
func (w *statusWatchdog) Extend(ns, name string, dur time.Duration) {
	if w == nil || dur <= 0 {
		return
	}
	now := w.clock.Now()
	w.mu.Lock()
	defer w.mu.Unlock()
	st := w.state[wdKey(ns, name)]
	if st == nil {
		st = &watchdog.State{LastActivity: now}
		w.state[wdKey(ns, name)] = st
	}
	st.ExpectedDuration(now, dur, w.cfg)
}

// Forget drops a session's tracking state. Called on terminal phases, on the
// user's reply landing, and on idle cleanup.
func (w *statusWatchdog) Forget(ns, name string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.state, wdKey(ns, name))
}

// Clear tears down the user-visible indicator AND drops tracking. Used by the
// session_watcher when a session goes Idle so a previously-fired "taking
// longer" indicator doesn't linger, and at terminal/idle GC. Clear =
// Forget + clearIndicator: it both removes the per-session State and sends the
// clear sentinel. For the in-flight yield path (ApplyEvent's ClearCaption) use
// clearIndicator instead — that must KEEP the Yielded State so later
// out-of-order captions stay gated.
func (w *statusWatchdog) Clear(ctx context.Context, ns, name string) error {
	if w == nil {
		return nil
	}
	w.Forget(ns, name)
	// The session left the running state (Idle / terminal / awaiting creds), so
	// any stall streak is over. Forget alone must NOT do this: the timeout
	// branch Forgets immediately after posting, and that is exactly the streak
	// we need to remember.
	w.resetStall(wdKey(ns, name))
	return w.clearIndicator(ctx, ns, name)
}

// clearIndicator dismisses the user-visible status indicator WITHOUT dropping
// tracking. It sends an empty Notification, which each channel kind reads as
// "clear my indicator". Split out from Clear so the unified machine's yield
// path can dismiss the visible indicator while leaving the per-session State
// (now Yielded) in place — that flag is what gates a later out-of-order
// (lower-Seq) caption from re-arming a fresh state and flickering the
// indicator back on. A nil sender/client (kubectl-driven, tests) is a no-op.
func (w *statusWatchdog) clearIndicator(ctx context.Context, ns, name string) error {
	if w == nil || w.senders == nil || w.cli == nil {
		return nil
	}
	sess, err := w.getSession(ctx, ns, name)
	if err != nil {
		return err
	}
	if sess.Spec.InputChannel == nil {
		return nil // kubectl-driven; no indicator to clear
	}
	sender, err := w.senders.SenderFor(ctx, sess)
	if err != nil {
		return err
	}
	if sender == nil {
		// Client-hosted (browser-view / builtin) kind: SenderFor's documented
		// "relay drops the envelope" sentinel. There is no channel sender to
		// clear an indicator on — drop, never dereference the nil (that nil
		// interface Send is what crash-looped channelsd).
		return nil
	}
	env, err := channelevents.BuildEnvelope(ns, name, channelevents.KindNotification,
		channelevents.NotificationPayload{}) // empty Text = clear sentinel
	if err != nil {
		return err
	}
	_, err = sender.Send(ctx, channelkinds.SessionInfo{Namespace: ns, Name: name, Channel: spiceboxv1alpha1.OutboundBinding(sess)}, env)
	return err
}

// ApplyEvent folds one ordered status event into the per-session machine and
// returns what the relay should do with the envelope that produced it (forward
// to the Sender / clear the indicator). Creates per-session State on first
// sight. Every status signal MUST route through this one ordered entrypoint, so
// that caption gating, yield suppression, stale-drop and the silence timer all
// share one consistent view of the session.
func (w *statusWatchdog) ApplyEvent(ctx context.Context, ns, name string, ev watchdog.Event) watchdog.ApplyResult {
	if w == nil {
		return watchdog.ApplyResult{}
	}
	now := w.clock.Now()
	w.mu.Lock()
	st := w.state[wdKey(ns, name)]
	if st == nil {
		st = &watchdog.State{}
		w.state[wdKey(ns, name)] = st
	}
	res := st.Apply(ev, now)
	w.mu.Unlock()
	if res.ClearCaption {
		// Tear down only the VISIBLE indicator — NOT the tracking State. The
		// machine has just marked this session Yielded; that flag must persist
		// so a later out-of-order (lower-Seq) caption is still gated. The
		// forgetting Clear() is reserved for session GC (idle/terminal); calling
		// it here would wipe Yielded and let a stale caption re-arm a fresh
		// state and flicker the indicator back on. Best-effort: a clear failure
		// is logged, never panics the relay's NATS callback.
		if err := w.clearIndicator(ctx, ns, name); err != nil {
			log.FromContext(ctx).Info("watchdog: ApplyEvent clear indicator failed (best-effort, continuing)",
				"session", wdKey(ns, name), "err", err.Error())
		}
	}
	return res
}

// OnTurnActivity applies a runner-authoritative turn-activity signal by folding
// it into the unified machine via ApplyEvent: active re-arms (and re-baselines
// the ordering on the new episode's Seq); a non-leakage pause yields (clears
// the indicator + marks Yielded so further captions are gated). The single
// exception is a leakage-approval pause: that approval prompt is a private
// ephemeral the requester cannot see, so folding it into the machine as a yield
// would clear the indicator and hide an invisible wait. Instead we leave the
// State untouched so Decide's WaitLeakage path keeps ANNOUNCING the wait. seq /
// uid carry the envelope's logical order + session instance from the runner.
func (w *statusWatchdog) OnTurnActivity(ctx context.Context, ns, name string, active bool, cause string, seq uint64, uid string) {
	if w == nil {
		return
	}
	if !active && cause == channelevents.PauseCauseLeakageApproval {
		// Leakage carve-out: do NOT Apply — leave tracking so the watchdog
		// still announces the requester-invisible approval wait.
		return
	}
	if active {
		// The runner started a turn: it is demonstrably alive, so a previous
		// stall streak ended. The next stall (if any) is a fresh discovery.
		w.resetStall(wdKey(ns, name))
	}
	w.ApplyEvent(ctx, ns, name, watchdog.Event{
		Kind:   watchdog.EvTurnActivity,
		Active: active,
		Cause:  cause,
		Seq:    seq,
		UID:    uid,
	})
}

// Run polls until ctx is canceled. Call in a dedicated goroutine.
func (w *statusWatchdog) Run(ctx context.Context) {
	logger := log.FromContext(ctx).WithName("statuswatchdog")
	ticker := time.NewTicker(statusWatchdogTickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.tick(ctx, logger)
		}
	}
}

func (w *statusWatchdog) tick(ctx context.Context, logger logger) {
	now := w.clock.Now()

	// Cheap pure pre-filter under lock: only sessions whose TIMING alone (i.e.
	// ignoring the wait classification) would warn/time out are candidates.
	// This keeps the per-candidate getSession out of the common path.
	w.mu.Lock()
	var candidates []string
	for key, st := range w.state {
		if st.Decide(watchdog.WaitActive, now, w.cfg) != watchdog.ActionNone {
			candidates = append(candidates, key)
		}
	}
	w.mu.Unlock()

	for _, key := range candidates {
		ns, name := splitNSName(key)
		sess, err := w.getSession(ctx, ns, name)
		if err != nil {
			if apierrors.IsNotFound(err) {
				w.Forget(ns, name) // session gone; stop tracking
				w.resetStall(key)  // …and drop its stall streak
			} else {
				logger.Error(err, "watchdog: get session for fire decision", "session", key)
			}
			continue
		}
		wait := waitFromStatus(sess)

		// Re-decide under lock against the LIVE state — Activity may have reset
		// it during the getSession — then record the outcome so we don't
		// re-evaluate (and re-fetch) every tick.
		w.mu.Lock()
		st, ok := w.state[key]
		if !ok {
			w.mu.Unlock()
			continue
		}
		action := st.Decide(wait, now, w.cfg)
		switch action {
		case watchdog.ActionWarn, watchdog.ActionWarnLeakage:
			st.Warned = true
		case watchdog.ActionTimeout:
			st.TimedOut = true
		case watchdog.ActionNone:
			// Not fireable now. Either Activity reset the timer (still active —
			// keep tracking) or a visible/leakage-already-announced wait is
			// suppressing it (drop tracking; the next Activity re-arms, so we
			// don't re-fetch status every tick).
			if st.Decide(watchdog.WaitActive, now, w.cfg) != watchdog.ActionNone {
				delete(w.state, key)
			}
		}
		w.mu.Unlock()

		switch action {
		case watchdog.ActionWarn:
			w.sendCaption(ctx, logger, sess, watchdogWarnText)
		case watchdog.ActionWarnLeakage:
			w.sendCaption(ctx, logger, sess, watchdogLeakageText)
		case watchdog.ActionTimeout:
			consecutive := w.bumpStall(key)
			w.sendNotice(ctx, logger, sess,
				fmt.Sprintf("notice-%s-%s-%d", categories.AgentStalled, sess.UID, consecutive),
				watchdogTimeoutNotice(name, consecutive))
			w.Forget(ns, name)
		}
	}
}

// watchdogTimeoutNotice is the stall message: a durable notice rather than a
// transient status caption, because the watchdog has stopped waiting and the
// user needs the message to persist past the indicator.
//
// consecutive is the session's consecutive-stall count (1 on first discovery).
// A repeat re-words the lead so the reader can tell we looked again rather
// than re-reading one sentence: an unchanged message on every reply reads as a
// stuck bot, not as something that keeps checking.
//
// Degraded rather than critical, and not terminal — the watchdog is reporting
// an ABSENCE of progress, which is a guess about a session that may still be
// alive. Dressing that as a death would be a stronger claim than the evidence
// supports.
func watchdogTimeoutNotice(name string, consecutive int) *notice.Notice {
	lead := "The agent seems to have stalled"
	if consecutive > 1 {
		lead = fmt.Sprintf("Still no progress (%s check)", humanize.Ordinal(consecutive))
	}
	return notice.New(categories.AgentStalled, notice.Args{
		Lead: lead,
		Body: "There's been no progress for 120s. The session may have failed to start, " +
			"the model may have hung, or a tool may be unreachable.",
		NextStep: "Send your request again.",
		Fields: []channelevents.InteractionField{{
			// The session id and nothing more. This lands in a chat thread in
			// front of whoever was talking to the agent, and most of them have
			// no cluster access — a shell command they cannot run is noise, and
			// it leaks operator vocabulary onto a product surface. Whoever CAN
			// investigate looks it up from the id.
			Label: "Session",
			Value: name,
		}},
		Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceParticipants},
	})
}

// sendNotice PUBLISHES a notice for the session's channel. Separate from
// sendCaption, which carries transient status captions: a caption is
// replaceable state, a notice is a durable event, and they ride different wire
// kinds precisely so a surface can treat them differently.
//
// The two also take different delivery paths, and that is the point. A caption
// is a KindNotification, which every kind's DEFAULT Sender renders. A notice is
// a KindInteractionRequest, which the default Sender answers with "unsupported
// envelope kind" — only the kind's "interaction" sub-channel sender renders
// one, and only the outbound relay resolves that sender. Handing a notice to
// the default Sender is why "The agent seems to have stalled" — the message
// that explains an otherwise silent session — reached no user.
//
// requestRef identifies this notice; the caller passes the consecutive-stall
// count so a re-check is its own interaction rather than overwriting the
// previous report's delivery handle.
func (w *statusWatchdog) sendNotice(_ context.Context, logger logger, sess *spiceboxv1alpha1.AgentSession, requestRef string, n *notice.Notice) {
	if sess.Spec.InputChannel == nil {
		return // kubectl-driven; nothing to post
	}
	// notice.Publish reports a nil publish func as an error rather than
	// dropping the message, so an unwired watchdog is loud in the logs instead
	// of silently swallowing every stall report. Client-hosted kinds need no
	// guard here: the relay owns that nil-sender drop.
	if err := n.Publish(w.publish,
		channelevents.SessionRef{Namespace: sess.Namespace, Name: sess.Name},
		requestRef); err != nil {
		logger.Error(err, "watchdog: publish notice", "session", wdKey(sess.Namespace, sess.Name))
	}
}

// startupWait reports whether the session is in a benign startup wait — the
// runner has not started because a startup dependency is still coming up
// (RunnerReady=False with a reason in startupWaitReasons) — and, if so, the
// user-facing notice to post. The runner does not exist yet in this window, so
// this "starting up" UX is driven from channelsd off the RunnerReady condition
// the operator already sets, not from the runner. Generic over the reason: the
// security-scanner case is just today's first member of startupWaitReasons.
func startupWait(sess *spiceboxv1alpha1.AgentSession) (notice string, ok bool) {
	c := meta.FindStatusCondition(sess.Status.Conditions,
		spiceboxv1alpha1.AgentSessionConditionRunnerReady)
	if c == nil || c.Status != metav1.ConditionFalse {
		return "", false
	}
	notice, ok = startupWaitReasons[c.Reason]
	if ok && notice == "" {
		notice = genericStartupText
	}
	return notice, ok
}

// waitFromStatus classifies why a session is not producing output, from its
// status. This is the SINGLE place that maps session state → watchdog policy.
//   - a PendingInteractions entry with Category info_leakage → WaitLeakage
//     (invisible to the requester → announce)
//   - any other PendingInteractions entry / PendingRequesters → WaitVisible
//     (prompt is in-thread or a DM the requester's surface shows)
//   - Idle / AwaitingApproval / AwaitingRetry / AwaitingCredentials / terminal
//     → WaitVisible (the channel already shows it, or there's nothing to watch)
//   - otherwise (Running / Pending) → WaitActive
func waitFromStatus(sess *spiceboxv1alpha1.AgentSession) watchdog.Wait {
	s := sess.Status
	// Startup: the agent has not begun running because a startup dependency is
	// still coming up (today: the content-guard detector loading its model).
	// This is a visible, bounded wait — the session_watcher posts the startup
	// notice, and the operator owns each reason's own deadline / fast-fail — so
	// the silence watchdog must not fire the generic "taking longer" warn over
	// it. Generic over the reason (see startupWaitReasons).
	if _, ok := startupWait(sess); ok {
		return watchdog.WaitVisible
	}
	// Since Slice C2 every approval family (tool_approval, info_leakage,
	// content_inspection) parks on the single generic PendingInteractions list.
	// info_leakage wins over every other wait class: its approval prompt is a
	// private ephemeral the requester can't see, so the watchdog must announce it
	// even when the same session also holds a visible-in-thread wait (a
	// tool_approval / content_inspection prompt) — most-invisible-surface wins.
	// So scan for a leakage entry FIRST, before folding the rest of the list (and
	// PendingRequesters) into the visible-in-thread class.
	for _, e := range s.PendingInteractions {
		if e.Category == string(categories.InfoLeakage) {
			return watchdog.WaitLeakage
		}
	}
	if len(s.PendingInteractions) > 0 || len(s.PendingRequesters) > 0 {
		return watchdog.WaitVisible
	}
	// Yielded to the user via await_user_message: a visible, indefinite wait —
	// the durable floor under the at-most-once turn_activity yield event. Suppress
	// the silence warn.
	if sess.Status.AwaitingUserInputSince != nil {
		return watchdog.WaitVisible
	}
	switch s.Phase {
	case spiceboxv1alpha1.AgentSessionPhaseIdle,
		spiceboxv1alpha1.AgentSessionPhaseAwaitingApproval,
		spiceboxv1alpha1.AgentSessionPhaseAwaitingDecision,
		spiceboxv1alpha1.AgentSessionPhaseAwaitingRetry,
		spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials,
		spiceboxv1alpha1.AgentSessionPhaseSucceeded,
		spiceboxv1alpha1.AgentSessionPhaseFailed:
		return watchdog.WaitVisible
	}
	return watchdog.WaitActive
}

// send posts a watchdog message (best-effort, logged on failure — never
// silently dropped). channelID resolution and the actual Slack call live in the
// resolved channel kind's sender.
func (w *statusWatchdog) sendCaption(ctx context.Context, logger logger, sess *spiceboxv1alpha1.AgentSession, text string) {
	if sess.Spec.InputChannel == nil {
		return // kubectl-driven; nothing to post
	}
	sender, err := w.senders.SenderFor(ctx, sess)
	if err != nil {
		logger.Error(err, "watchdog: resolve sender", "session", wdKey(sess.Namespace, sess.Name))
		return
	}
	if sender == nil {
		// Client-hosted (browser-view) kind: nothing to post (SenderFor's
		// drop sentinel). Never dereference the nil interface.
		return
	}
	env, err := channelevents.BuildEnvelope(sess.Namespace, sess.Name,
		channelevents.KindNotification, channelevents.NotificationPayload{Text: text})
	if err != nil {
		logger.Error(err, "watchdog: build envelope", "session", wdKey(sess.Namespace, sess.Name))
		return
	}
	if _, err := sender.Send(ctx, channelkinds.SessionInfo{
		Namespace: sess.Namespace, Name: sess.Name, Channel: spiceboxv1alpha1.OutboundBinding(sess),
	}, env); err != nil {
		logger.Error(err, "watchdog: send caption", "session", wdKey(sess.Namespace, sess.Name))
	}
}

func (w *statusWatchdog) getSession(ctx context.Context, ns, name string) (*spiceboxv1alpha1.AgentSession, error) {
	var sess spiceboxv1alpha1.AgentSession
	if err := w.cli.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &sess); err != nil {
		return nil, err // raw so apierrors.IsNotFound works in the tick
	}
	return &sess, nil
}

func splitNSName(key string) (string, string) {
	for i := 0; i < len(key); i++ {
		if key[i] == '/' {
			return key[:i], key[i+1:]
		}
	}
	return "", key
}

// logger is the minimal subset we use; satisfied by logr.Logger.
type logger interface {
	Info(msg string, keysAndValues ...interface{})
	Error(err error, msg string, keysAndValues ...interface{})
}

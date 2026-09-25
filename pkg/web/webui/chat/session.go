package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/outbound"
	"github.com/authzed/openagentprimitives/pkg/channels/sessionnotice"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentstatus"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/web/viewurn"
)

// newChatSessionNamespace is the namespace the built-in chat's conversations
// conventionally live in — "default", the same fallback `oap`'s CLI uses (see
// cmd/oap/internal/kube.Bundle.Namespace).
//
// It constrains NOTHING this package serves: every session-addressing route
// takes its namespace from the URL and serves whatever namespace the viewer
// holds agentsession#interact in (see sessionKey). Since this package creates
// no sessions, the constant survives only as the shared default of the test
// fixtures and the desktop bundle.
const newChatSessionNamespace = "default"

// sessionListener abstracts browser.Host's Listener() for testability: the
// registry only needs SubmitUserMessage/SubmitInterrupt/SubmitDecision/
// SubmitResurface, so tests can substitute a fake without building a real
// browser.Host (which needs a real Channel + inbound pipeline).
//
// SubmitUserMessage/SubmitInterrupt/SubmitDecision each take an EXPLICIT ext
// — the submitting subject's identity, derived fresh per call by
// Registry.Submit* — never a value captured when the listener was built. A
// live entry is shared across every subject holding agentsession#interact (a
// participant may be first to rehydrate a session someone else started), so a
// construction-time identity misattributes every later sender's message or
// decision to the builder. SubmitResurface carries no identity and needs none.
type sessionListener interface {
	SubmitUserMessage(ctx context.Context, ext channelkinds.ExternalIdentity, text, requestID string) (channelkinds.InboundDecision, error)
	SubmitInterrupt(ctx context.Context, ext channelkinds.ExternalIdentity, requestID string) error
	SubmitDecision(ctx context.Context, ext channelkinds.ExternalIdentity, category, requestRef, actionID string) error
	SubmitResurface(ctx context.Context) error
}

// channelListener is the subset of a channelkind Listener (the
// *browserListener from browser.Host.Listener()) that boundListener wraps. Its
// SubmitInterrupt/SubmitInteractionDecision take ns/name as parameters even
// though the concrete listener knows its own bound session, and boundListener
// is the seam that supplies them. ext is explicit for the reason in
// sessionListener's doc.
type channelListener interface {
	SubmitUserMessage(ctx context.Context, ext channelkinds.ExternalIdentity, text, requestID string) (channelkinds.InboundDecision, error)
	SubmitInterrupt(ctx context.Context, ext channelkinds.ExternalIdentity, ns, name, requestID string) error
	SubmitInteractionDecision(ctx context.Context, ext channelkinds.ExternalIdentity, ns, name, category, requestRef, actionID string) error
	SubmitResurface(ctx context.Context, ns, name string) error
}

// boundListener adapts a channelListener to sessionListener by closing over
// its conversation's (ns, sessionName), so the Registry's Submit* methods
// reach the ns/name-taking listener methods without threading those through
// the registry layer. It holds NO identity of its own — every identity-bearing
// call passes the caller's ext straight through, unmodified.
type boundListener struct {
	l           channelListener
	ns          string
	sessionName string
}

func (b *boundListener) SubmitUserMessage(ctx context.Context, ext channelkinds.ExternalIdentity, text, requestID string) (channelkinds.InboundDecision, error) {
	return b.l.SubmitUserMessage(ctx, ext, text, requestID)
}

func (b *boundListener) SubmitInterrupt(ctx context.Context, ext channelkinds.ExternalIdentity, requestID string) error {
	return b.l.SubmitInterrupt(ctx, ext, b.ns, b.sessionName, requestID)
}

func (b *boundListener) SubmitDecision(ctx context.Context, ext channelkinds.ExternalIdentity, category, requestRef, actionID string) error {
	return b.l.SubmitInteractionDecision(ctx, ext, b.ns, b.sessionName, category, requestRef, actionID)
}

func (b *boundListener) SubmitResurface(ctx context.Context) error {
	return b.l.SubmitResurface(ctx, b.ns, b.sessionName)
}

var _ sessionListener = (*boundListener)(nil)

// sessionEntry is the registry's live-session record: one per open chat
// conversation, for the conversation's whole lifetime (across zero or more
// attached browser tabs).
type sessionEntry struct {
	ns, name string
	// owner is the canonical webui subject ("user:<...>") that started this
	// conversation — a durable record (surfaced as the info panel's Owner
	// field), not an authorization gate: every session-scoped route is
	// gated on Registry.checkInteract (agentsession#interact), which admits
	// any subject the platform granted standing to, not only owner.
	owner string

	// agentClass is the class this conversation runs; title is a short human
	// label (the first user message, else a synthesized "<class> · <time>").
	// Both are set once by wireExistingSession from the AgentSession's spec,
	// before the entry is published, and never mutated. Write-only in
	// production today — a session-list surface is the intended reader.
	agentClass string
	title      string
	// createdAt is when this chat conversation was created in THIS webd
	// process. Set in newSessionEntry; immutable thereafter.
	createdAt time.Time

	listener sessionListener

	// stop cancels the phase-watcher goroutine and stops the outbound
	// relay's NATS subscription. teardown calls it before deleting the k8s
	// objects. nil-safe (test entries may leave it nil).
	stop func()

	// deleteK8s deletes the ephemeral AgentSession + Channel + creds Secret
	// this session owns. Separated from stop so tests can substitute a
	// no-op. nil-safe.
	deleteK8s func(ctx context.Context)

	// senders resolves this session's outbound Sender / sub-channel Sender /
	// StreamDeltaSink for the Registry's single process-wide relay (see
	// sender_resolver.go); production wires the session's browser.Host. Set
	// once during wireSessionEntry, before the entry is visible to the
	// registry, and never mutated — so reading it needs no lock.
	senders outbound.SenderResolver

	// logger is used only on Emit's drop paths (unrecognized event type,
	// marshal failure), so a broken broadcast is surfaced rather than
	// swallowed. Backfilled by wireSessionEntry; the zero Logger's no-op sink
	// is safe for test entries that never hit those paths.
	logger logr.Logger

	// mu guards sinks/lastActivity/torndown together so attach-vs-teardown
	// (and detach-vs-teardown) can never race: a session that has already
	// been marked torndown can never gain a new sink, and a sink can never
	// be silently orphaned by a teardown that ran between an attach's
	// liveness check and its map insert (both happen under the same lock).
	mu           sync.Mutex
	sinks        map[emitter]struct{}
	lastActivity time.Time
	torndown     bool
}

func newSessionEntry(ns, name, owner string) *sessionEntry {
	now := time.Now()
	return &sessionEntry{
		ns: ns, name: name, owner: owner,
		createdAt:    now,
		sinks:        map[emitter]struct{}{},
		lastActivity: now,
	}
}

// Emit implements browser.EventSink by broadcasting to every attached sink.
// Safe for concurrent calls. A session with zero attached sinks drops the
// event without even marshaling it; that, plus the per-sink non-blocking
// enqueue, is what stops a NATS-callback-driven Emit from ever blocking on a
// slow or absent tab. The tradeoff is no replay on reconnect — the turns are
// still durable in operator memory.
//
// The wire frame is marshaled ONCE for all sinks, not once per sink.
// Recording fakes that do not implement frameEmitter get the typed event.
func (e *sessionEntry) Emit(msg any) {
	e.mu.Lock()
	if len(e.sinks) == 0 {
		e.mu.Unlock()
		return // no attached tab: drop on the floor without marshaling
	}
	targets := make([]emitter, 0, len(e.sinks))
	for s := range e.sinks {
		targets = append(targets, s)
	}
	e.mu.Unlock()

	// Marshal the wire frame once for all sinks. A drop (unrecognized type or
	// a marshal failure) is logged, never silently swallowed; production
	// frameEmitter sinks then receive nothing for this event, while recording
	// fakes still get the typed value below.
	var raw []byte
	if frame, ok := toFrame(msg); !ok {
		e.logger.Info("chat: dropping unrecognized event type", "session", e.name)
	} else if b, err := json.Marshal(frame); err != nil {
		e.logger.Info("chat: marshal ws frame failed; dropping", "session", e.name, "type", frame.Type, "err", err.Error())
	} else {
		raw = b
	}

	for _, s := range targets {
		if fe, isFrame := s.(frameEmitter); isFrame {
			if raw != nil {
				fe.emitFrame(raw)
			}
			continue // frameEmitter with an undeliverable frame: already logged
		}
		s.Emit(msg) // recording fake / non-websocket sink: hand it the typed event
	}
}

func (e *sessionEntry) touch() {
	e.mu.Lock()
	e.lastActivity = time.Now()
	e.mu.Unlock()
}

// attach adds sink if the session is still live, atomically with the
// liveness check — so a teardown racing an attach can never result in a
// sink that is added after teardown already notified+closed every sink
// (which would leak that connection forever). Returns false when the
// session has already been torn down; the caller must close the connection
// itself in that case.
func (e *sessionEntry) attach(s emitter) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.torndown {
		return false
	}
	e.sinks[s] = struct{}{}
	e.lastActivity = time.Now()
	return true
}

func (e *sessionEntry) detach(s emitter) {
	e.mu.Lock()
	delete(e.sinks, s)
	e.lastActivity = time.Now()
	e.mu.Unlock()
}

// idleFor reports how long the entry has had zero attached sinks AND no
// activity, or false if a sink is currently attached (the idle clock is
// paused while any tab is open).
func (e *sessionEntry) idleFor() (time.Duration, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.sinks) > 0 {
		return 0, false
	}
	return time.Since(e.lastActivity), true
}

// markTorndown flips the session to torn-down and returns every currently
// attached sink for the caller to notify + close exactly once. Idempotent:
// a second call (e.g. a reaper and a phase-watcher racing) returns
// (nil, false) so teardown only runs once.
func (e *sessionEntry) markTorndown() ([]emitter, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.torndown {
		return nil, false
	}
	e.torndown = true
	out := make([]emitter, 0, len(e.sinks))
	for s := range e.sinks {
		out = append(out, s)
	}
	e.sinks = nil
	return out, true
}

var _ browser.EventSink = (*sessionEntry)(nil)

// terminalNotice describes why a session's phase-watcher fired. Registry
// teardown also mints these for "idle_timeout" and "server_shutdown".
type terminalNotice struct {
	// Reason is the terminal cause the browser renders.
	Reason string // "succeeded" | "failed"
	// FailureReason is the machine-readable cause; empty unless Reason=="failed".
	FailureReason string
	// FailureMessage is the human-readable cause; empty unless Reason=="failed".
	FailureMessage string
}

// adoptRealSession is the production sessionBuilderFunc: it builds the
// in-process half of an AgentSession that ALREADY exists — one an authorized
// start route just created — and creates nothing itself.
//
// The per-session memory-token Secret is a readiness signal only; its CONTENTS
// are deliberately never read, because webd is browser-facing and must not
// hold the Ed25519 audit-signing seed that would let it forge this session's
// audit chain (see pkg/memory/tokens). browsersession.Create does not wait for
// readiness, so the wait lives here, between the create and the wiring.
//
// onTerminal fires exactly once, from a background goroutine, on a terminal
// phase; Registry.Adopt wires it to that session's own teardown, so nothing
// here is coupled to "the" registry.
func adoptRealSession(
	ctx context.Context,
	d Deps,
	key sessionKey,
	subject string,
	onTerminal func(terminalNotice),
) (*sessionEntry, error) {
	if err := waitForSessionReady(ctx, d.K8s(), key.Namespace, key.Name); err != nil {
		return nil, err
	}
	var sess spiceboxv1alpha1.AgentSession
	if err := d.K8s().Get(ctx, client.ObjectKey{Namespace: key.Namespace, Name: key.Name}, &sess); err != nil {
		return nil, fmt.Errorf("load the session to adopt: %w", err)
	}
	return wireExistingSession(d, key, &sess, subject, onTerminal)
}

// wireExistingSession builds the in-process half of a chat conversation whose
// AgentSession already exists: the bound Channel, the browser Host, the
// listener, the health watcher, and the entry's sidebar metadata.
//
// The ONE body shared by both ways a live entry comes into being — Adopt (a
// session an authorized route just created) and rehydrate (one left behind by
// a restart or idle reap) — so the two can never wire a conversation
// differently. Neither creates a cluster object; every step depends only on
// objects that already exist, which is what makes both paths possible.
func wireExistingSession(
	d Deps,
	key sessionKey,
	sess *spiceboxv1alpha1.AgentSession,
	subject string,
	onTerminal func(terminalNotice),
) (*sessionEntry, error) {
	if sess.Spec.InputChannel == nil {
		return nil, fmt.Errorf("session %q has no bound input channel", key.String())
	}
	var ch spiceboxv1alpha1.Channel
	if err := d.K8s().Get(context.Background(), client.ObjectKey{Namespace: key.Namespace, Name: sess.Spec.InputChannel.Name}, &ch); err != nil {
		return nil, fmt.Errorf("load the bound Channel %q: %w", sess.Spec.InputChannel.Name, err)
	}

	// The ATTACHING subject, not the CR's starter: ext/principal seed
	// HostConfig.User/Principal, which the live_view_offer/session_view_offer
	// senders use to address a minted deep-link to "the session user". They do
	// NOT decide who authors a later message or decision — the Registry's
	// Submit* methods derive that fresh per call (see sessionListener), which
	// is what makes an entry built by one subject safe for another.
	ext, principal, err := deriveIdentity(subject)
	if err != nil {
		return nil, fmt.Errorf("derive the attaching subject's identity: %w", err)
	}

	// entry.owner comes from the CR's started-by, not subject: it is the
	// conversation's durable "started by" record (the info panel's Owner) and
	// must stay the starter regardless of who first opens it.
	entry, err := wireSessionEntry(d, key.Namespace, sess, &ch, nil /* creds: owner-ref'd to the Channel */, ext, principal,
		spiceboxv1alpha1.StartedBySubject(sess).String(), onTerminal)
	if err != nil {
		return nil, err
	}
	// Stamped while the entry is still private to this caller — before
	// Registry.publish makes it visible — so no reader observes a half-set
	// entry. Read from the CR, not from arguments: the session's own spec is
	// the single source, whichever path is wiring it.
	entry.agentClass = sess.Spec.Class
	entry.createdAt = sess.CreationTimestamp.Time
	entry.title = sessionTitle(sess.Spec.Prompt.Inline, sess.Spec.Class, entry.createdAt)
	return entry, nil
}

// wireSessionEntry builds the in-process half of a chat session — the entry,
// the browser Host, the listener, and the health watcher — for an AgentSession
// that already exists in the cluster.
//
// Its one caller is wireExistingSession, which supplies the Channel and the
// identities; it is kept separate so the Host/listener/watcher assembly can be
// read (and tested) without the Get-and-derive preamble in front of it.
//
// creds may be nil: it is only used to build deleteK8s, and an existing
// session's Secret is owner-ref'd to the Channel, so the cascade covers it.
//
// ns is the namespace sess actually lives in — supplied by the caller rather
// than assumed, because the sessions this wires span whatever namespaces the
// viewer holds interact in, never a single hardcoded default.
func wireSessionEntry(
	d Deps,
	ns string,
	sess *spiceboxv1alpha1.AgentSession,
	ch *spiceboxv1alpha1.Channel,
	creds *corev1.Secret,
	ext channelkinds.ExternalIdentity,
	principal identity.Principal,
	owner string,
	onTerminal func(terminalNotice),
) (*sessionEntry, error) {
	sessionName := sess.Name

	// chatVia is the web chat surface's view URN. The inputs are the
	// constant type + empty id/sub, so Format cannot fail in practice — but
	// per AGENTS.md ("never silently drop errors") the error is checked
	// rather than discarded with `_`.
	chatVia, err := viewurn.Format(viewurn.TypeChat, "", "")
	if err != nil {
		return nil, fmt.Errorf("mint chat view URN: %w", err)
	}

	// Constructed before the Host so it can BE the Host's Sink: Emit is
	// already meaningful on an entry with zero attached sinks (it drops render
	// events until a tab attaches). listener/stop/deleteK8s are backfilled
	// below, once the pieces producing them exist.
	entry := newSessionEntry(ns, sessionName, owner)
	entry.logger = d.Logger()

	host, err := newBrowserHost(d, ns, ch, entry, ext, principal, sessionName, chatVia)
	if err != nil {
		return nil, fmt.Errorf("build browser host: %w", err)
	}

	// Makes this session's Host reachable from the Registry's single
	// process-wide relay, rather than running a per-session one here. See
	// sender_resolver.go for why a per-session relay leaks every conversation
	// into every other.
	entry.senders = host

	sessCtx, cancel := context.WithCancel(context.Background())
	entry.listener = &boundListener{l: host.Listener(), ns: ns, sessionName: sessionName}
	entry.stop = cancel
	entry.deleteK8s = func(ctx context.Context) {
		deleteChatK8sObjects(ctx, d, sess, ch, creds)
	}

	// The health watcher surfaces failures to the browser instead of letting a
	// wedged session hang the chat's throbber forever: it emits a visible
	// send_error to this session's sinks (via entry.Emit) for an invalid bound
	// Channel, and drives teardown (which emits the terminal session_ended) on a
	// Failed phase, an out-from-under-us deletion, or a startup that never
	// progresses past Pending within startupGrace. See watchSessionHealth.
	go watchSessionHealth(sessCtx, d, ns, sessionName, ch.Name, entry.Emit, onTerminal)

	return entry, nil
}

// newBrowserHost assembles this session's browser.Host from the chat Deps.
// Separate from wireSessionEntry so the sub-channel minter wiring — the
// ArtifactViewMinter behind the live_view_offer sender's "View live" link, and
// the SessionViewMinter behind the session_view_offer escalation anchor — is
// unit-testable on its own. A missing minter here silently swallows a
// session's live-view offer.
func newBrowserHost(
	d Deps,
	ns string,
	ch *spiceboxv1alpha1.Channel,
	sink browser.EventSink,
	ext channelkinds.ExternalIdentity,
	principal identity.Principal,
	sessionName, via string,
) (*browser.Host, error) {
	nc := d.NATS()
	return browser.NewHost(browser.HostConfig{
		Deps: channelkinds.Deps{
			Channel:     ch,
			NATSPublish: func(subj string, p []byte) error { return nc.Publish(subj, p) },
			NATSRequest: func(subj string, p []byte, timeout time.Duration) ([]byte, error) {
				msg, err := nc.Request(subj, p, timeout)
				if err != nil {
					return nil, err
				}
				return msg.Data, nil
			},
			K8sClient:          d.K8s(),
			ArtifactViewMinter: d.ArtifactViewMinter(),
			SessionViewMinter:  d.SessionViewMinter(),
		},
		Sink:        sink,
		User:        ext,
		Principal:   principal,
		Namespace:   ns,
		SessionName: sessionName,
		Via:         via,
	})
}

// deleteChatK8sObjects best-effort deletes the AgentSession, Channel, and
// creds Secret a chat conversation owns. Both the session and the Secret are
// also owner-ref'd to the Channel, so a hard webd crash mid-delete still
// cleans up via Kubernetes GC — this is belt-and-suspenders. Every failure is
// logged, never dropped.
//
// The ONE deletion primitive for a chat session's CRs, reserved for an
// EXPLICIT user "delete conversation" action (not yet wired). No lifecycle,
// error, or shutdown path may call it: a build error, a shutdown landing
// mid-build, and a terminal/idle teardown all leave the CR for the operator to
// own. Deleting out from under those paths is how a session the user started
// silently vanishes.
func deleteChatK8sObjects(ctx context.Context, d Deps, sess *spiceboxv1alpha1.AgentSession, ch *spiceboxv1alpha1.Channel, creds *corev1.Secret) {
	k8s := d.K8s()
	if err := k8s.Delete(ctx, sess); err != nil && !apierrors.IsNotFound(err) {
		d.Logger().Info("chat: delete AgentSession failed during teardown", "session", sess.Namespace+"/"+sess.Name, "err", err.Error())
	}
	if err := k8s.Delete(ctx, ch); err != nil && !apierrors.IsNotFound(err) {
		d.Logger().Info("chat: delete Channel failed during teardown", "channel", ch.Namespace+"/"+ch.Name, "err", err.Error())
	}
	if err := k8s.Delete(ctx, creds); err != nil && !apierrors.IsNotFound(err) {
		d.Logger().Info("chat: delete creds Secret failed during teardown", "secret", creds.Namespace+"/"+creds.Name, "err", err.Error())
	}
}

// waitForSessionReady blocks until the chat session is ready for the browser
// to attach — meaning EITHER of two things is true:
//
//   - The session has STARTED (phaseStarted). This is the signal that matters
//     for a session parked awaiting the user — AwaitingCredentials /
//     AwaitingIdentityChoice — which has no runner and so never gets a
//     memory-token Secret, yet the browser MUST attach for the prompt to
//     render. Checked FIRST, before the context, so an already-ready session
//     can never be turned into a spurious "context canceled".
//
//   - The operator minted the `<name>-memory-token` Secret: the precise
//     "runner up, channelsd accepting inbounds" signal for a normal session.
//     Presence is the signal; the CONTENTS are never read, because the
//     Ed25519 audit-signing seed inside would let a browser-facing process
//     forge this session's audit chain.
//
// A session still in "" / Pending with no token is waited out, up to
// sessionReadyTimeout.
func waitForSessionReady(ctx context.Context, k8s client.Client, ns, name string) error {
	deadline := time.Now().Add(sessionReadyTimeout)
	ticker := time.NewTicker(sessionReadyPollInterval)
	defer ticker.Stop()
	for {
		var sess spiceboxv1alpha1.AgentSession
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &sess); err == nil {
			if phaseStarted(sess.Status.Phase) {
				return nil
			}
		}
		var sec corev1.Secret
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: name + "-memory-token"}, &sec); err == nil {
			if len(sec.Data["token"]) > 0 {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("waiting for session %q to become ready: timed out after %s", name, sessionReadyTimeout)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for session %q to become ready: %w", name, ctx.Err())
		case <-ticker.C:
		}
	}
}

// sessionReadyTimeout bounds how long waitForSessionReady polls for the
// per-session memory-token Secret before giving up; sessionReadyPollInterval
// is how often it re-checks. Consts rather than the test-tunable vars the
// watchdog uses below, because no test needs to shrink the readiness wait.
const (
	sessionReadyTimeout      = 60 * time.Second
	sessionReadyPollInterval = 1 * time.Second
)

// sessionPollInterval is how often watchSessionHealth re-reads the
// AgentSession + bound Channel. startupGrace bounds how long a chat session
// may sit not-yet-Running (owner unresolved, Channel invalid, runner never
// scheduled, ...) before the watcher declares it a terminal "couldn't start"
// so the browser stops spinning. Both are package vars so tests can shrink
// them (tests that mutate them must NOT run in parallel).
var (
	sessionPollInterval = 2 * time.Second
	startupGrace        = 45 * time.Second
	// runnerStartupGrace bounds the runner BRING-UP window. Once the wait is on
	// the runner (RunnerReady=False, any reason) startupGrace no longer
	// applies: image pull + scheduling + a refused-then-retried pod routinely
	// exceeds 45s on a cold node for a start that still succeeds. This ceiling
	// still bounds a runner that never comes up so the throbber never spins
	// forever.
	runnerStartupGrace = 5 * time.Minute
)

// watchSessionHealth is the built-in chat's no-silent-hang guardian: it polls
// the AgentSession (and, until it starts, its bound Channel) and makes every
// failure VISIBLE to the attached browser tabs instead of letting the chat's
// throbber spin forever.
//
//   - emit fans a browser.MsgSendError (rendered as a visible error line) to
//     this session's sinks for a bound Channel reporting Valid=False — deduped,
//     so a persistently-invalid Channel warns once, not every tick.
//   - emit also fans the startup line, every pre-start tick: what is in the
//     way (or the generic lead while nothing is blocking), and Started=true
//     once, on the tick the session first reaches a started phase.
//   - onTerminal is invoked exactly once — then the watcher returns — on a
//     Failed phase (surfacing the phase's FailureReason/message), a Succeeded
//     phase, the AgentSession being deleted out from under us (NotFound: e.g.
//     the operator GC'ing a session whose Channel never validated — our own
//     teardown cancels ctx first, so this only fires for an EXTERNAL delete),
//     or the session never progressing past Pending within startupGrace.
//
// onTerminal drives Registry.teardown, which emits the terminal session_ended
// frame carrying the reason. Same phase watch cmd/oap/internal/chatcmd runs,
// plus the channel-health and stuck-startup surfacing a browser needs and a
// terminal does not (the CLI prints operator errors directly).
func watchSessionHealth(ctx context.Context, d Deps, ns, name, channelName string, emit func(any), onTerminal func(terminalNotice)) {
	ticker := time.NewTicker(sessionPollInterval)
	defer ticker.Stop()
	start := time.Now()
	channelWarned := false
	// hasStarted latches once the session first reaches a started phase. The
	// startup grace measures from watcher creation and guards ONLY the initial
	// startup; a session that started, went Idle, and was woken back to Pending
	// (a new inbound, or an artifact-view annotation) legitimately re-enters
	// Pending long after `start` — without this latch that wake would be
	// mis-declared "couldn't start (still pending)".
	hasStarted := false
	// lastPhase drives the turn-complete backstop: when the session settles into
	// Idle (turn done, awaiting the next user message) we emit a turn-complete
	// so the browser clears its "working" throbber — the authoritative phase
	// wins even if the runner's real-time turn_activity(active:false) was missed
	// or a late render tick re-armed it. Emitted once per Idle-entry.
	lastPhase := ""
	ref := browser.SessionRef{Namespace: ns, Name: name}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			var s spiceboxv1alpha1.AgentSession
			if err := d.K8s().Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &s); err != nil {
				if apierrors.IsNotFound(err) {
					// Deleted out from under us — surface it rather than
					// spinning on NotFound forever (the exact silent hang this
					// guards against). Our own teardown cancels ctx before
					// deleting, so reaching here means something external
					// removed the session.
					onTerminal(terminalNotice{
						Reason:         "failed",
						FailureReason:  "SessionRemoved",
						FailureMessage: "the agent session was removed before it could finish (see the admin console / logs)",
					})
					return
				}
				continue // transient; retry next tick
			}
			switch s.Status.Phase {
			case spiceboxv1alpha1.AgentSessionPhaseFailed:
				onTerminal(terminalNotice{Reason: "failed", FailureReason: s.Status.FailureReason, FailureMessage: failureMessageOf(&s)})
				return
			case spiceboxv1alpha1.AgentSessionPhaseSucceeded:
				onTerminal(terminalNotice{Reason: "succeeded"})
				return
			}

			// Turn-complete backstop: the session has settled into Idle (turn
			// done, awaiting the next user message). Emit a turn-complete so the
			// browser clears "working" even if the runner's real-time
			// turn_activity(active:false) was missed or a late render tick
			// re-armed it. Once per Idle-entry — a wake (Idle→Pending→Running) and
			// the next completion re-arm and re-emit.
			if s.Status.Phase == spiceboxv1alpha1.AgentSessionPhaseIdle && lastPhase != spiceboxv1alpha1.AgentSessionPhaseIdle {
				emit(browser.MsgTurnActivity{Session: ref, Active: false, Cause: channelevents.PauseCauseIdle})
			}
			lastPhase = s.Status.Phase

			// Once the runner is actually up (any phase past Pending), the
			// startup failures below no longer apply — stop the grace clock and
			// stop polling the Channel; only a terminal phase (or an external
			// delete) matters from here. Latch it: a later return to Pending
			// (wake from idle) must NOT re-arm the startup terminal.
			if phaseStarted(s.Status.Phase) {
				if !hasStarted {
					// The one frame that takes the shell's startup line down.
					emit(sessionStartupMsg{Session: ref, Started: true})
				}
				hasStarted = true
				continue
			}
			// The session already started once and has now dropped back to
			// Pending (a wake). The startup grace is for the FIRST startup only;
			// from here only a terminal phase or an external delete matters.
			if hasStarted {
				continue
			}

			// Still pre-Running. Surface an invalid bound Channel once so the
			// user learns WHY nothing is happening (SecretMissing / SpecInvalid
			// / ...), rather than watching a silent spinner.
			chReason, chMessage, chBad := channelValidFalse(ctx, d.K8s(), ns, channelName)
			if chBad && !channelWarned {
				channelWarned = true
				emit(browser.MsgSendError{
					Session: ref,
					Kind:    channelevents.Kind("channel_health"),
					Err:     "channel invalid: " + reasonDetail(chReason, chMessage),
					At:      time.Now().UTC(),
				})
			}

			// The window depends on how far startup got — startupGrace for the
			// PRE-runner gates, the far longer runnerStartupGrace once the wait
			// is on the runner itself, since image pull, scheduling and a
			// refused-then-retried pod legitimately take minutes.
			bringingUp := runnerBringingUp(&s)
			grace := startupGrace
			if bringingUp {
				grace = runnerStartupGrace
			}

			// The shell's startup line: the same caption channelsd puts on the
			// channel thread (one fold, agentstatus.StartupCaption), or the
			// generic lead while nothing is blocking, plus "still trying" once a
			// runner-side wait has outlived the short grace.
			startup := sessionStartupMsg{Session: ref, Text: sessionnotice.GenericStartupLead}
			if text, short, ok := agentstatus.StartupCaption(&s); ok {
				startup.Text, startup.Short = text, short
			}
			startup.StillTrying = bringingUp && time.Since(start) > startupGrace
			emit(startup)

			// Hard bound: never left Pending within the grace window. Fail with
			// the most specific detail available, so the browser shows a real
			// reason instead of an infinite throbber.
			if time.Since(start) > grace {
				reason, message := stuckDetail(&s, chReason, chMessage)
				onTerminal(terminalNotice{Reason: "failed", FailureReason: reason, FailureMessage: message})
				return
			}
		}
	}
}

// phaseStarted reports whether the AgentSession has progressed past initial
// scheduling. The startup-grace clock and the Channel-health probe guard only
// that pre-Running window: an AwaitingCredentials session HAS started and is
// legitimately waiting on the user, so it must never be flagged stuck. The
// predicate lives in the apis package so webd and the CLI TUI share one
// definition.
func phaseStarted(phase string) bool {
	return spiceboxv1alpha1.AgentSessionPhaseStarted(phase)
}

// runnerReadyFalse returns the RunnerReady condition when it is present and
// False — the one fact both the grace ceiling and the terminal detail read —
// and nil otherwise, so the two sites cannot drift on what "runner-side wait"
// means.
func runnerReadyFalse(s *spiceboxv1alpha1.AgentSession) *metav1.Condition {
	if c := meta.FindStatusCondition(s.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionRunnerReady); c != nil && c.Status == metav1.ConditionFalse {
		return c
	}
	return nil
}

// runnerBringingUp reports whether the session is waiting on the runner
// itself — RunnerReady is False, whatever the reason — rather than stuck in a
// pre-runner gate. A pod pulling its image, one the scheduler has not placed,
// and one the cluster refused and the operator keeps retrying all look the
// same from the person's side: the agent is coming up, and the short
// startupGrace must not fail it. The long runnerStartupGrace still bounds it,
// so a runner that never comes up is surfaced. The signal is the condition,
// NOT Status.Phase, which stays Pending for the whole bring-up.
func runnerBringingUp(s *spiceboxv1alpha1.AgentSession) bool {
	return runnerReadyFalse(s) != nil
}

// channelValidFalse reports the reason/message of a bound Channel's Valid=False
// condition, or (_, _, false) when the Channel is Valid, has no Valid condition
// yet, or can't be read (a transient Get error must not be reported as a
// failure).
func channelValidFalse(ctx context.Context, k8s client.Client, ns, name string) (reason, message string, isFalse bool) {
	var ch spiceboxv1alpha1.Channel
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &ch); err != nil {
		return "", "", false
	}
	if c := meta.FindStatusCondition(ch.Status.Conditions, spiceboxv1alpha1.ChannelConditionValid); c != nil && c.Status == metav1.ConditionFalse {
		return c.Reason, c.Message, true
	}
	return "", "", false
}

// stuckDetail builds the FailureReason/FailureMessage for a session that never
// left Pending within startupGrace, preferring the most specific signal
// available: an invalid bound Channel, else the AgentSession's own not-ready
// condition, else a generic couldn't-start message. Every branch ends with a
// pointer to where the full detail lives, since the browser only ever sees this
// one line.
func stuckDetail(s *spiceboxv1alpha1.AgentSession, chReason, chMessage string) (reason, message string) {
	const seeMore = " — see the admin console / logs"
	if chReason != "" {
		return chReason, "the agent session couldn't start: channel invalid (" + reasonDetail(chReason, chMessage) + ")" + seeMore
	}
	// The wait was on the runner and it never came up within the long ceiling.
	// NOT "couldn't start" — the platform kept trying — but say what was in the
	// way: the cluster's own words on a refused pod are the actionable part.
	// RunnerCreating is progress, not a cause, so it alone is never named.
	if c := runnerReadyFalse(s); c != nil {
		msg := "the agent is taking unusually long to start"
		if c.Reason != spiceboxv1alpha1.ReasonRunnerCreating {
			msg += " (" + agentstatus.FriendlyGateDetail(c.Reason, c.Message) + ")"
		}
		return "RunnerStartupTimeout", msg + seeMore
	}
	if r, m := firstNotReadyCondition(s); r != "" {
		// Friendly phrasing, not the raw gate reason: the machine token still
		// travels in the returned reason for programmatic consumers.
		return r, "the agent session couldn't start (" + agentstatus.FriendlyGateDetail(r, m) + ")" + seeMore
	}
	return "StartupTimeout", "the agent session couldn't start (still pending)" + seeMore
}

// firstNotReadyCondition returns the reason/message of the most informative
// POSITIVE readiness gate that is False on the AgentSession — a condition whose
// False genuinely means "couldn't start". When no positive gate is False,
// returns "" so stuckDetail falls back to the generic StartupTimeout message.
//
// The gate list and its ordering live in pkg/controllers/agentstatus, shared
// with channelsd's startup caption: it renders the same ranking as a LIVE
// thread status where this renders a TERMINAL "couldn't start". Two copies
// would disagree about which blocker is the cause and which the consequence.
func firstNotReadyCondition(s *spiceboxv1alpha1.AgentSession) (reason, message string) {
	reason, message, _ = agentstatus.FirstNotReadyGate(s)
	return reason, message
}

// reasonDetail joins a condition reason with its human message ("SecretMissing:
// secret foo not found").
func reasonDetail(reason, message string) string {
	return agentstatus.ReasonDetail(reason, message)
}

func failureMessageOf(s *spiceboxv1alpha1.AgentSession) string {
	if c := meta.FindStatusCondition(s.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionFailed); c != nil && c.Message != "" {
		return c.Message
	}
	return ""
}

// titleMaxRunes bounds a synthesized session title so the sidebar label stays
// on one line; longer first messages are truncated with an ellipsis.
const titleMaxRunes = 60

// sessionTitle derives the short sidebar label for a conversation: the first
// line of the opening user message (trimmed + truncated), or — when that is
// empty — a synthesized "<agentClass> · <HH:MM>" fallback so a session always
// has a label.
func sessionTitle(text, agentClass string, createdAt time.Time) string {
	if line, _, _ := strings.Cut(strings.TrimSpace(text), "\n"); strings.TrimSpace(line) != "" {
		return truncateRunes(strings.TrimSpace(line), titleMaxRunes)
	}
	return fmt.Sprintf("%s · %s", agentClass, createdAt.Format("15:04"))
}

// truncateRunes shortens s to at most max runes, appending "…" when it cut.
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return strings.TrimRight(string(r[:max]), " ") + "…"
}

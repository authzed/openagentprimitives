package chat

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/outbound"
	"github.com/authzed/openagentprimitives/pkg/web/browsersession"
)

// defaultMaxLiveSessionsPerSubject bounds ONE subject's live entries. Per
// subject, not process-wide: a process-wide cap is shared-fate, letting one
// viewer's open tabs lock out every other user.
const defaultMaxLiveSessionsPerSubject = 16

// defaultMaxLiveSessions is the process capacity ceiling: every live entry
// holds a websocket, a listener and a health-watch goroutine, so the map cannot
// be unbounded whatever the per-subject limit allows. Reaching it is an
// operator signal, not a user's fault, and it is refused with different copy
// and a different log line from the per-subject limit.
//
// 256 ÷ 16 means sixteen fully-engaged viewers can saturate one webd, after
// which every other viewer is refused — hence SetLimits.
const defaultMaxLiveSessions = 256

// limits are the live-entry caps in force, settable once at startup.
//
// Package-level rather than per-Registry: the Registry is a process-wide
// singleton (chat.go's ensureRegistry) built during the WebUI mount loop,
// after internal/cmd/webd has parsed its flags, so there is no construction site a
// caller could pass them to. SetLimits is the seam instead.
var limits = struct {
	perSubject int
	total      int
}{perSubject: defaultMaxLiveSessionsPerSubject, total: defaultMaxLiveSessions}

// SetLimits overrides the two live-session caps. Call it BEFORE the registry
// is built (internal/cmd/webd does, from its flags, during startup); it is not safe to
// call concurrently with request serving and is not meant to be.
//
// A non-positive value leaves that limit at its default rather than disabling
// it: "0" from an unset flag must not silently mean "unlimited", which would
// remove the only bound on a browser-facing surface.
func SetLimits(perSubject, total int) {
	if perSubject > 0 {
		limits.perSubject = perSubject
	}
	if total > 0 {
		limits.total = total
	}
}

// MaxLiveSessionsPerSubject and MaxLiveSessions report the caps in force, for
// a startup banner or an operator-facing surface that wants to say what they
// are rather than have an operator guess from a refusal.
func MaxLiveSessionsPerSubject() int { return limits.perSubject }
func MaxLiveSessions() int           { return limits.total }

// sessionIdleTTL is how long a session may sit with zero attached browser
// tabs before the reaper tears down its IN-PROCESS state — it stops the
// relay/watcher and drops the registry entry via teardown, but does NOT delete
// the AgentSession/Channel CR (the operator owns CR lifecycle: its own idle
// sleep-and-reap keeps the session wakeable and re-hydratable). The idle clock
// is paused entirely while at least one tab is attached (see
// sessionEntry.idleFor). Abandoning a browser tab for this long simply detaches
// the conversation; the CR survives for re-opening — deliberately UNLIKE the
// CLI TUI ("oap agent chat"), whose ephemeral sessions are deleted on quit.
const sessionIdleTTL = 30 * time.Minute

// reapInterval is how often the reaper scans for idle sessions.
const reapInterval = time.Minute

// reservationTTL bounds how long a slot may sit RESERVED with nothing
// published into it. A reservation is normally seconds old (reserve, create
// the cluster objects, adopt); without this TTL the two ways it outlives that
// leak the slot for the process's lifetime:
//
//   - the create succeeded but the adopt failed. The caller deliberately keeps
//     the reservation — the session exists and is the viewer's, so it must
//     keep counting against them — until something re-attaches it or this TTL
//     reclaims it, or sixteen such failures lock the viewer out.
//   - a panic between Reserve and the return. net/http recovers per
//     connection, so the process survives with a slot nothing will ever
//     publish into.
//
// Longer than any adopt path (adoptRealSession waits up to 60s for the
// operator) so it can never reclaim a reservation still in flight.
const reservationTTL = 10 * time.Minute

// Sentinel errors the HTTP handlers map to specific status codes.
//
// ErrUnknownAgentClass aliases browsersession.ErrUnknownAgentClass rather than
// declaring a second errors.New with the same text, so handlers.go's
// errors.Is stays matched to the value browsersession.Create wraps.
//
// ErrAuthzUnavailable is distinct from ErrForbidden on purpose: a CheckInteract
// that itself failed (SpiceDB outage, network blip) is INDETERMINATE, never a
// denial. mapSubmitError maps it to 503, never 403 — folding it into
// ErrForbidden would turn an authz outage into every caller losing access.
//
// ErrTooManySessionsForSubject and ErrServerAtCapacity are two DIFFERENT
// refusals: "you have too many open" the viewer fixes by closing one, "this
// server is full" is the operator's problem and closing a tab will not help.
// Different copy, statuses, and log lines.
var (
	ErrTooManySessionsForSubject = errors.New("you already have the maximum number of sessions open; close one first")
	ErrServerAtCapacity          = errors.New("this server is at capacity; try again shortly")
	ErrUnknownAgentClass         = browsersession.ErrUnknownAgentClass
	ErrSessionNotFound           = errors.New("chat session not found")
	// ErrSessionUnavailable is "there was no answer", where ErrSessionNotFound
	// is "the answer is no". A Kubernetes read that FAILED — an RBAC refusal in
	// a namespace this webd cannot read, a throttled apiserver, a wiring step
	// that could not complete — is not evidence the conversation is gone, and a
	// 404 tells a viewer their session vanished when it is right there. Maps to
	// 503 (mapSubmitError): retryable, and true.
	ErrSessionUnavailable = errors.New("chat session is temporarily unavailable; try again")
	ErrForbidden          = errors.New("chat session belongs to a different user")
	ErrAuthzUnavailable   = errors.New("authorization check failed; try again")
	ErrRegistryClosed     = errors.New("chat registry is shutting down")
	// errSessionAlreadyLive reports that Reserve was asked for a key this
	// process already holds — live, or reserved by another caller. Unexported:
	// a caller reserving a freshly-minted, uuid-suffixed name can never hit it,
	// and it is a wiring bug rather than anything a viewer can act on.
	errSessionAlreadyLive = errors.New("chat session is already live in this process")
)

// sessionKey identifies one live entry. Sessions are addressed by
// (namespace, name) because this package serves whatever namespaces the
// viewer holds agentsession#interact in — a Slack channel's namespace,
// `oap`'s default, anything a Channel was created in — so a bare name would
// collide the moment two namespaces hold a session of the same name. That
// collision would read as the WRONG transcript rather than as an error.
type sessionKey struct{ Namespace, Name string }

func (k sessionKey) String() string { return k.Namespace + "/" + k.Name }

// sessionBuilderFunc builds the IN-PROCESS half of a session that already
// exists in the cluster — it creates no Kubernetes object and writes no
// relationship. Production wires adoptRealSession (session.go); tests
// substitute a fake that never touches k8s/NATS/SpiceDB.
//
// Deliberately no create-shaped builder: creating an AgentSession is an
// AUTHORIZED route's job (the start handlers in pkg/web/webui/sessions and
// pkg/web/webui/agentui), and a create path here is how an unauthorized one
// comes back.
type sessionBuilderFunc func(
	ctx context.Context,
	d Deps,
	key sessionKey,
	subject string,
	onTerminal func(terminalNotice),
) (*sessionEntry, error)

// liveSlot is one key's record in the registry table.
//
// subject is recorded at RESERVATION time — before any entry (or, for the
// start route, any cluster object) exists — so the per-subject cap can refuse
// a start before it creates anything. Deliberately NOT entry.owner: owner is
// the conversation's durable STARTER, and a slot held by a participant who
// re-attached someone else's conversation counts against that participant.
//
// reservedAt exists only so the reaper can reclaim a slot that never received
// an entry (see reservationTTL). Never updated: once an entry is published the
// entry's own idle clock takes over.
type liveSlot struct {
	subject    string
	entry      *sessionEntry // nil while the in-process wiring is in flight
	reservedAt time.Time
}

// Registry is the per-process (webd instance) live-session table for the
// built-in web chat. Exactly one is constructed per webd process (see
// ensureRegistry in chat.go); it outlives any single HTTP request.
type Registry struct {
	deps  Deps
	build sessionBuilderFunc
	// clock is the registry's time source, non-nil only in tests that need to
	// age a reservation past reservationTTL without sleeping for it. See now().
	clock func() time.Time

	mu       sync.Mutex
	sessions map[sessionKey]*liveSlot // a slot with a nil entry = reserved, wiring in flight
	// closed is set under mu by shutdown, before it tears down any session.
	// claimSlot consults it when reserving, and publish consults it again
	// right before inserting — otherwise a wiring call racing Shutdown inserts
	// a live entry into a table whose relay/reaper have stopped, leaking that
	// session's relay subscription + k8s objects forever.
	closed bool

	// relay is the Registry's single process-wide outbound.Relay: ONE
	// subscription to the cluster-wide "ap.session.*.*.out.>" wildcard for
	// every open chat conversation, dispatching through the Registry itself
	// (see sender_resolver.go). nil until startRelay succeeds; nil-safe in
	// shutdown for test registries that never call it.
	relay       *outbound.Relay
	relayCancel context.CancelFunc

	stopReaper chan struct{}
	reaperDone chan struct{}

	// pingInterval overrides the ws keepalive interval; 0 means the default.
	// A per-Registry field (not a package var) so a test can shrink it without
	// racing another test's keepalive goroutine on shared global state.
	pingInterval time.Duration
}

// wsPing is the keepalive interval for this Registry's chat websockets.
func (r *Registry) wsPing() time.Duration {
	if r.pingInterval > 0 {
		return r.pingInterval
	}
	return defaultWSPingInterval
}

func newRegistry(d Deps) *Registry {
	return newRegistryWithBuilder(d, adoptRealSession)
}

// newRegistryWithBuilder is the test seam: it swaps out adoptRealSession for
// a fake so registry lifecycle tests never touch k8s/NATS/SpiceDB.
func newRegistryWithBuilder(d Deps, build sessionBuilderFunc) *Registry {
	return &Registry{
		deps:       d,
		build:      build,
		sessions:   map[sessionKey]*liveSlot{},
		stopReaper: make(chan struct{}),
		reaperDone: make(chan struct{}),
	}
}

// startReaper launches the idle-session reaper. Call once per Registry.
func (r *Registry) startReaper() {
	go r.reapLoop()
}

// startRelay starts the Registry's single process-wide outbound relay: one
// subscription to "ap.session.*.*.out.>" for every open chat conversation,
// with the Registry itself as the outbound.SenderResolver (see
// sender_resolver.go). Call once per Registry, before it is published as the
// process-wide singleton (chat.go's ensureRegistry) — a session created before
// this call has nowhere for its outbound envelopes to go.
//
// ONE relay, not one per session: the subscription is a cluster-wide wildcard,
// so N per-session relays would each receive every session's envelopes and
// deliver them to every OTHER session's sink — a cross-conversation data leak
// plus N× duplicate delivery. Scoped by the Registry's own session table, a
// single relay can only reach the entry an envelope's session name resolves to.
func (r *Registry) startRelay() error {
	ctx, cancel := context.WithCancel(context.Background())
	relay := &outbound.Relay{
		NC:      r.deps.NATS(),
		K8s:     r.deps.K8s(),
		Senders: r,
		// The cluster-wide subscription also sees every Slack- and CLI-driven
		// session's envelopes, none of which this relay can deliver. Accept
		// rejects them from the live-session table BEFORE the AgentSession is
		// loaded, so foreign traffic costs no API round-trip; without it,
		// unrelated cluster traffic queues rate-limited Gets on the single
		// goroutine dispatching this subscription, ahead of the chat user's
		// own reply.
		Accept: r.acceptsSession,
		// KindTurnActivity is a control-plane watchdog signal the relay routes
		// to OnTurnActivity BEFORE any Sender, so this seam is the only way the
		// coarse active⇄paused transition reaches the chat UI: it clears the
		// browser's "working" throbber on agent_work_complete even for a turn
		// that produced no final user_message (a plan-only turn). Scoped by
		// session name so the fan-out stays leak-safe.
		OnTurnActivity: r.onTurnActivity,
	}
	if err := relay.Start(ctx); err != nil {
		cancel()
		return fmt.Errorf("start chat outbound relay: %w", err)
	}
	r.relay = relay
	r.relayCancel = cancel
	return nil
}

// acceptsSession is the relay's pre-Get scope (outbound.Relay.Accept): whether
// an outbound envelope for (ns, name) could possibly be delivered here. Same
// fail-closed question entryForSession answers for the resolver methods (see
// sender_resolver.go), asked one step earlier and off the same live-session
// table, so the two can never disagree about which conversations exist. A
// foreign namespace, a torn-down session, or one still mid-wiring (a nil
// placeholder, which r.lookup reports absent) is rejected before any API call.
func (r *Registry) acceptsSession(ns, name string) bool {
	_, ok := r.lookup(sessionKey{Namespace: ns, Name: name})
	return ok
}

// onTurnActivity forwards a KindTurnActivity signal to the owning session's
// sink as a browser.MsgTurnActivity render event. Resolved against the live
// table and dropped when it belongs to no live session, so an envelope for a
// torn-down or foreign session can never reach another conversation's tabs —
// the same leak-safe scoping the SenderResolver methods enforce.
func (r *Registry) onTurnActivity(_ context.Context, ns, name string, active bool, cause string, _ uint64, _ string) {
	entry, ok := r.lookup(sessionKey{Namespace: ns, Name: name})
	if !ok {
		return
	}
	entry.Emit(browser.MsgTurnActivity{
		Session: browser.SessionRef{Namespace: ns, Name: name},
		Active:  active,
		Cause:   cause,
	})
}

func (r *Registry) reapLoop() {
	defer close(r.reaperDone)
	ticker := time.NewTicker(reapInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.stopReaper:
			return
		case <-ticker.C:
			r.reapIdle()
		}
	}
}

func (r *Registry) reapIdle() {
	now := r.now()
	r.mu.Lock()
	var stale []sessionKey
	var abandoned []sessionKey
	for key, slot := range r.sessions {
		if slot.entry == nil {
			// A reservation nothing ever published into: dropped directly, not
			// through teardown (which expects an entry to stop). Nothing exists
			// to tear down — only a slot charging a viewer for a session it
			// never wired. See reservationTTL for how one gets this old.
			if !slot.reservedAt.IsZero() && now.Sub(slot.reservedAt) > reservationTTL {
				abandoned = append(abandoned, key)
			}
			continue
		}
		if d, isIdle := slot.entry.idleFor(); isIdle && d > sessionIdleTTL {
			stale = append(stale, key)
		}
	}
	for _, key := range abandoned {
		delete(r.sessions, key)
	}
	r.mu.Unlock()
	for _, key := range abandoned {
		r.deps.Logger().Info("chat: reclaimed a reservation nothing was ever published into",
			"session", key.String())
	}
	for _, key := range stale {
		r.teardown(key, terminalNotice{Reason: "idle_timeout"})
	}
}

// claimSlot reserves key for subject under BOTH caps, atomically with the cap
// checks, and reports whether THIS call created the reservation (so the caller
// must release it on failure) and whether the key already holds a live entry.
//
// The two caps are separate refusals: the per-subject limit is the viewer's
// own doing and closing a session fixes it; the process ceiling is the server
// out of room, and nothing the viewer does helps. See
// ErrTooManySessionsForSubject / ErrServerAtCapacity.
func (r *Registry) claimSlot(key sessionKey, subject string) (created, live bool, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return false, false, ErrRegistryClosed
	}
	if slot, ok := r.sessions[key]; ok {
		// An existing reservation is reused (a caller that reserved before
		// creating its cluster objects, then adopted); a live entry is reported
		// so the caller can decide whether that is success or a collision.
		return false, slot.entry != nil, nil
	}
	mine := 0
	for _, slot := range r.sessions {
		if slot.subject == subject {
			mine++
		}
	}
	if mine >= limits.perSubject {
		return false, false, ErrTooManySessionsForSubject
	}
	if len(r.sessions) >= limits.total {
		return false, false, ErrServerAtCapacity
	}
	r.sessions[key] = &liveSlot{subject: subject, reservedAt: r.now()}
	return true, false, nil
}

// now is the registry's clock, indirected so a test can age a reservation
// without sleeping. Production leaves it nil and gets time.Now.
func (r *Registry) now() time.Time {
	if r.clock != nil {
		return r.clock()
	}
	return time.Now()
}

// releaseSlot drops key's RESERVATION. It is deliberately a no-op once an
// entry has been published into the slot: a caller that reserved, created and
// adopted successfully may still call its release (a deferred cleanup), and
// that must not tear down a live conversation.
func (r *Registry) releaseSlot(key sessionKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if slot, ok := r.sessions[key]; ok && slot.entry == nil {
		delete(r.sessions, key)
	}
}

// Reserve claims a live-entry slot for subject under both caps BEFORE the
// caller creates any cluster object, so a capped refusal leaves nothing behind
// — the difference between "you have too many sessions open" and "you have too
// many sessions open, and here is an orphaned Channel nobody will ever see".
//
// The returned release drops the reservation and is safe to call more than
// once; it is a no-op after Adopt has published an entry into the slot.
//
// A key this registry ALREADY holds — live, or reserved by someone else — is
// refused rather than reused: handing back a release for a slot this call did
// not create would let the caller's deferred release drop the first caller's
// slot, whose own Adopt then finds nothing to publish into. Unreachable today
// (the start route reserves a freshly-minted uuid-suffixed name), which is why
// the guard must be explicit; rehydrate checks the same thing.
func (r *Registry) Reserve(ns, name, subject string) (release func(), err error) {
	key := sessionKey{Namespace: ns, Name: name}
	created, live, err := r.claimSlot(key, subject)
	if err != nil {
		return nil, err
	}
	if live || !created {
		return nil, errSessionAlreadyLive
	}
	return func() { r.releaseSlot(key) }, nil
}

// Adopt wires an AgentSession that a caller has ALREADY authorized and
// created into this registry: it waits for the operator-minted memory-token
// Secret and then binds the host, listener and sink (see adoptRealSession,
// session.go). It never creates a session — creation belongs to an authorized
// route, and a create path here is how an unauthorized one comes back.
//
// A slot the caller already reserved is reused; a caller that adopts without
// one has the slot claimed here, under the same caps. Adopting a key this
// process already holds a live entry for is a no-op, so a duplicate adopt
// cannot produce two Hosts (and two health watchers) for one conversation.
func (r *Registry) Adopt(ctx context.Context, ns, name, subject string) error {
	key := sessionKey{Namespace: ns, Name: name}
	created, live, err := r.claimSlot(key, subject)
	if err != nil {
		return err
	}
	if live {
		return nil
	}
	entry, err := r.build(ctx, r.deps, key, subject, func(tn terminalNotice) {
		r.teardown(key, tn)
	})
	if err != nil {
		if created {
			r.releaseSlot(key)
		}
		return err
	}
	return r.publish(key, entry)
}

// publish inserts a freshly-wired entry into its reserved slot.
//
// If the registry shut down while the wiring was in flight, don't resurrect
// the entry into a closed table. shutdown() only tears down slots that already
// hold an entry, so a slot reserved by THIS in-flight call is invisible to it
// and "still reserved" alone cannot detect a shutdown that began after the
// reservation — r.closed catches that race. Either way, drop the reservation
// and stop what was built, so its relay wiring + phase watcher don't leak.
//
// The cleanup is IN-PROCESS ONLY — deliberately NO entry.deleteK8s. The
// AgentSession/Channel/creds already exist, and the operator owns CR lifecycle
// (idle-reap keeps the session wakeable; the finalizer preserves its audit on
// expiry). Deleting here is how a session the user just started vanishes when
// webd restarts under it. Same rule as teardown.
func (r *Registry) publish(key sessionKey, entry *sessionEntry) error {
	r.mu.Lock()
	slot, stillReserved := r.sessions[key]
	closed := r.closed
	if closed || !stillReserved {
		if stillReserved {
			delete(r.sessions, key)
		}
		r.mu.Unlock()
		if entry.stop != nil {
			entry.stop()
		}
		if closed {
			return fmt.Errorf("%w: session %q wired after shutdown", ErrRegistryClosed, key.String())
		}
		return fmt.Errorf("chat session %q was cancelled while it was being wired", key.String())
	}
	slot.entry = entry
	r.mu.Unlock()
	return nil
}

// lookup returns the live entry for key, or (nil, false) if absent or still
// being wired (a slot whose entry is not set yet).
func (r *Registry) lookup(key sessionKey) (*sessionEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	slot, ok := r.sessions[key]
	if !ok || slot.entry == nil {
		return nil, false
	}
	return slot.entry, true
}

// checkInteract is this package's single call-site shape for the
// agentsession#interact gate: true+nil admits, false+nil is a clean denial
// (ErrForbidden), and a non-nil error is INDETERMINATE — mapped to
// ErrAuthzUnavailable, never ErrForbidden, so a SpiceDB outage can never read
// as "this subject has no access." The raw error is logged here with session
// and subject, so callers may return it onward without logging it again.
func (r *Registry) checkInteract(ctx context.Context, key sessionKey, subject string) error {
	ok, err := r.deps.CheckInteract(ctx, key.Namespace, key.Name, subject)
	if err != nil {
		r.deps.Logger().Info("chat: CheckInteract errored", "session", key.String(), "subject", subject, "err", err.Error())
		return ErrAuthzUnavailable
	}
	if !ok {
		return ErrForbidden
	}
	return nil
}

// authorize resolves key and checks that subject holds agentsession#interact
// on it, rehydrating a conversation that outlived this process when there is
// no live entry — which is what makes the restored sidebar's rows openable.
//
// This is the RESUME door: it returns a live, writable entry, so it refuses a
// conversation in a terminal phase. Anything that only READS durable state
// (transcript, info panel) must use authorizeRead instead, or it inherits that
// refusal and 404s on every finished conversation.
func (r *Registry) authorize(ctx context.Context, key sessionKey, subject string) (*sessionEntry, error) {
	entry, ok := r.lookup(key)
	if !ok {
		return r.rehydrate(ctx, key, subject)
	}
	if err := r.checkInteract(ctx, key, subject); err != nil {
		return nil, err
	}
	return entry, nil
}

// authorizeRead authorizes a READ of key's durable record — the transcript
// (GET .../messages) and the info panel (GET .../detail) — for subject, and
// returns the conversation's canonical owner (the session's STARTER, not
// necessarily the reading subject — multiple subjects may hold interact on
// one session).
//
// Deliberately NOT authorize: that path resolves an in-memory miss through
// rehydrate, which refuses every terminal phase, and would 404 exactly the
// conversations the sidebar advertises as finished (the entry is dropped by
// the phase watcher's teardown, and gone for EVERY conversation after a webd
// restart). Transcript and settings are durable, so reading them needs no live
// entry — only a passing CheckInteract.
func (r *Registry) authorizeRead(ctx context.Context, key sessionKey, subject string) (string, error) {
	if entry, ok := r.lookup(key); ok {
		if err := r.checkInteract(ctx, key, subject); err != nil {
			return "", err
		}
		return entry.owner, nil
	}
	sess, err := r.interactableChatSession(ctx, key, subject)
	if err != nil {
		return "", err
	}
	return spiceboxv1alpha1.StartedBySubject(sess).String(), nil
}

// interactableChatSession loads key's AgentSession and proves subject holds
// agentsession#interact on it, entirely from durable state. It is the ONE
// place this rule lives — shared by the read path (authorizeRead) and the
// resume path (rehydrate) — so the two can never drift into disagreeing
// about who may see a conversation.
//
// The gate is agentsession#interact, never a started-by comparison: a
// starter-only check refuses every participant, owner and class-wide group
// grant the platform writes. Every sibling session surface
// (pkg/web/webui/sessionview, .../interact, .../agentui) gates identically.
func (r *Registry) interactableChatSession(ctx context.Context, key sessionKey, subject string) (*spiceboxv1alpha1.AgentSession, error) {
	var sess spiceboxv1alpha1.AgentSession
	if err := r.deps.K8s().Get(ctx, client.ObjectKey{Namespace: key.Namespace, Name: key.Name}, &sess); err != nil {
		if !apierrors.IsNotFound(err) {
			// NotFound IS the ordinary "no such conversation" answer and needs no
			// log. Anything else — RBAC in a namespace this webd cannot read, an
			// apiserver blip — is a real failure; answering it "not found" tells
			// the viewer their conversation is gone. Unavailable is the true,
			// retryable answer.
			r.deps.Logger().Info("chat: loading the session for an interact check failed",
				"session", key.String(), "err", err.Error())
			return nil, ErrSessionUnavailable
		}
		return nil, ErrSessionNotFound
	}
	// A session with no input channel (or a non-browser one) has no chat plane
	// to serve — a shape check, not an authorization one, so it stays ahead of
	// checkInteract and answers ErrSessionNotFound regardless of standing.
	if sess.Labels[spiceboxv1alpha1.LabelChannelKind] != browser.KindName || sess.Spec.InputChannel == nil {
		return nil, ErrSessionNotFound // a Slack/CLI session, not a chat conversation
	}
	if err := r.checkInteract(ctx, key, subject); err != nil {
		return nil, err
	}
	return &sess, nil
}

// rehydrate re-attaches this process to an existing chat AgentSession — one a
// previous webd left behind, or one this webd's idle reaper dropped — by
// rebuilding only the in-process half (Host, listener, health watcher). It
// creates no cluster objects.
//
// Standing comes from interactableChatSession. A terminal phase is refused
// here and ONLY here: re-attaching a finished conversation would spin up a
// health watcher that immediately tore it down again. Reading one is a
// different question, answered by authorizeRead — which is what keeps the
// sidebar's "ended" rows openable.
func (r *Registry) rehydrate(ctx context.Context, key sessionKey, subject string) (*sessionEntry, error) {
	sess, err := r.interactableChatSession(ctx, key, subject)
	if err != nil {
		return nil, err
	}
	if terminalPhase(sess.Status.Phase) {
		return nil, ErrSessionNotFound
	}

	// Reserve the slot before the (slow) wiring, under the same two caps Adopt
	// uses, so two concurrent requests for the same dormant session cannot each
	// build a Host and leave two health watchers running against one
	// conversation. The re-attacher — not the conversation's starter — is who
	// this slot counts against (see liveSlot.subject).
	created, live, err := r.claimSlot(key, subject)
	if err != nil {
		return nil, err
	}
	if live {
		if existing, ok := r.lookup(key); ok {
			return existing, nil // another goroutine won the race
		}
		return nil, ErrSessionNotFound
	}
	if !created {
		return nil, ErrSessionNotFound // a wiring is in flight; let the caller retry
	}

	entry, err := wireExistingSession(r.deps, key, sess, subject,
		func(tn terminalNotice) { r.teardown(key, tn) })
	if err != nil {
		r.releaseSlot(key)
		r.deps.Logger().Info("chat: rehydrate failed to wire the session", "session", key.String(), "err", err.Error())
		// The session EXISTS and the viewer may interact with it; the WIRING
		// failed (unreadable bound Channel, relay could not subscribe).
		// Reporting that as "not found" puts a readable transcript beside a
		// 404 claiming there is no such conversation.
		return nil, ErrSessionUnavailable
	}
	if err := r.publish(key, entry); err != nil {
		return nil, err
	}
	return entry, nil
}

// terminalPhase reports whether the session has finished for good. A terminal
// conversation is still listed — its transcript stays readable — but is shown
// as ended rather than offered as resumable.
func terminalPhase(phase string) bool {
	return phase == spiceboxv1alpha1.AgentSessionPhaseSucceeded ||
		phase == spiceboxv1alpha1.AgentSessionPhaseFailed
}

// SubmitMessage routes text into an existing conversation via the session's
// browser Listener — the same inbound pipeline path channelsd uses for
// slack/fake sessions.
func (r *Registry) SubmitMessage(ctx context.Context, key sessionKey, subject, text, requestID string) (channelkinds.InboundDecision, error) {
	entry, err := r.authorize(ctx, key, subject)
	if err != nil {
		return channelkinds.InboundDecision{}, err
	}
	ext, err := r.callerIdentity(subject)
	if err != nil {
		return channelkinds.InboundDecision{}, err
	}
	entry.touch()
	return entry.listener.SubmitUserMessage(ctx, ext, text, requestID)
}

// SubmitInterrupt requests a mid-turn interrupt of key's in-flight agent work
// via the session's browser Listener — the same KindInterruptRequest
// envelope publish path a Slack interactive click (once wired) would use.
// Gated exactly like SubmitMessage.
func (r *Registry) SubmitInterrupt(ctx context.Context, key sessionKey, subject, requestID string) error {
	entry, err := r.authorize(ctx, key, subject)
	if err != nil {
		return err
	}
	ext, err := r.callerIdentity(subject)
	if err != nil {
		return err
	}
	entry.touch()
	return entry.listener.SubmitInterrupt(ctx, ext, requestID)
}

// SubmitDecision submits a decision on a pending interaction (identity_choice,
// and any future decision-kind category) via the session's browser Listener —
// the same KindInteractionDecision envelope publish path a Slack interactive
// click (once wired) would use. Gated exactly like SubmitMessage.
func (r *Registry) SubmitDecision(ctx context.Context, key sessionKey, subject, category, requestRef, actionID string) error {
	entry, err := r.authorize(ctx, key, subject)
	if err != nil {
		return err
	}
	ext, err := r.callerIdentity(subject)
	if err != nil {
		return err
	}
	entry.touch()
	return entry.listener.SubmitDecision(ctx, ext, category, requestRef, actionID)
}

// callerIdentity derives the ExternalIdentity to stamp on an outbound submit
// from the CALLING subject, freshly on every call — never reused from whichever
// subject built the entry's listener. A live entry is shared across every
// subject holding agentsession#interact, so an identity baked in at
// construction time misattributes every later sender's message or decision to
// the builder (see sessionListener in session.go). subject has already passed
// authorize's CheckInteract, so a failure here is internal, not authorization.
func (r *Registry) callerIdentity(subject string) (channelkinds.ExternalIdentity, error) {
	ext, _, err := deriveIdentity(subject)
	if err != nil {
		r.deps.Logger().Info("chat: derive caller identity failed", "subject", subject, "err", err.Error())
		return channelkinds.ExternalIdentity{}, fmt.Errorf("derive caller identity: %w", err)
	}
	return ext, nil
}

// RequestResurface asks channelsd to re-deliver whatever prompt key is
// currently parked on, so a tab that just attached sees it. Gated exactly
// like SubmitMessage.
//
// It deliberately does NOT touch() the idle clock: a resurface is the server
// answering an attach, not the user doing something, and the attach itself
// already pauses the reaper (idleFor returns not-idle while any sink is
// attached).
func (r *Registry) RequestResurface(ctx context.Context, key sessionKey, subject string) error {
	entry, err := r.authorize(ctx, key, subject)
	if err != nil {
		return err
	}
	if entry.listener == nil {
		// A live entry always has one (wireSessionEntry backfills it before the
		// registry sees the entry), so nil is a wiring bug. Returning nil
		// silently would present as the very missing-prompt symptom this call
		// exists to fix.
		return fmt.Errorf("chat session %q has no listener wired", key.String())
	}
	return entry.listener.SubmitResurface(ctx)
}

// Attach authorizes and attaches a websocket sink to key's fan-out. Returns
// ErrSessionNotFound both when the session is unknown AND when it has
// already been torn down (the caller cannot distinguish, and shouldn't need
// to — both mean "there is nothing to stream to").
func (r *Registry) Attach(ctx context.Context, key sessionKey, subject string, sink emitter) (*sessionEntry, error) {
	entry, err := r.authorize(ctx, key, subject)
	if err != nil {
		return nil, err
	}
	if !entry.attach(sink) {
		return nil, ErrSessionNotFound
	}
	return entry, nil
}

// Detach removes sink from entry's fan-out. Safe to call even if entry was
// tornDown concurrently (detach on an already-cleared sink set is a no-op).
func (r *Registry) Detach(entry *sessionEntry, sink emitter) {
	entry.detach(sink)
}

// teardown is idempotent and safe to call from multiple goroutines
// concurrently (the phase-watcher and the reaper both call it by name): only
// the first caller to remove the entry from the map proceeds to actually
// tear it down.
func (r *Registry) teardown(key sessionKey, notice terminalNotice) {
	r.mu.Lock()
	slot, ok := r.sessions[key]
	if ok {
		delete(r.sessions, key)
	}
	r.mu.Unlock()
	if !ok || slot.entry == nil {
		return // already gone, or was never finalized
	}
	entry := slot.entry

	sinks, first := entry.markTorndown()
	if !first {
		return // a concurrent teardown already won (shouldn't happen post-map-delete, defensive)
	}

	if entry.stop != nil {
		entry.stop()
	}

	ended := sessionEndedMsg{
		Session:        browser.SessionRef{Namespace: entry.ns, Name: entry.name},
		Reason:         notice.Reason,
		FailureReason:  notice.FailureReason,
		FailureMessage: notice.FailureMessage,
	}
	// Both calls are non-blocking: Emit enqueues the session_ended frame onto
	// the sink's buffer, and Close closes the sink's channel so its writer
	// goroutine flushes best-effort then hangs up. That is what lets shutdown()
	// fan teardown across many sinks without stalling ~10s per stuck tab on a
	// synchronous write bounded only by the write deadline. A wedged writer
	// times out in its own detached goroutine; shutdown is never blocked.
	for _, s := range sinks {
		s.Emit(ended)
		s.Close()
	}

	// teardown is IN-PROCESS ONLY (registry removal, relay/watcher stop,
	// session_ended emit) and MUST NOT delete the AgentSession CR: a terminal
	// or idle chat session stays inspectable and resumable, and deleting here
	// silently wipes every ended conversation's transcript + audit. The
	// operator owns CR lifecycle — idle-reap keeps the CR wakeable, wall-clock
	// expiration eventually deletes it with the audit preserved by the
	// finalizer. No lifecycle path here deletes the CR; entry.deleteK8s /
	// deleteChatK8sObjects stays wired ONLY for an explicit "delete
	// conversation" user action.
}

// Shutdown tears down every live session. Called once on webd process
// shutdown (see chat.Shutdown) so a killed webd doesn't leave orphaned
// AgentSessions/Channels running with no attached UI and no relay.
//
// closed=true is set under the same lock that snapshots which sessions to tear
// down, so publish can detect a Shutdown that began after a slot was reserved
// but before its entry was published. Such a slot holds no entry, so it is
// invisible to the `slot.entry != nil` filter below; without the flag, publish
// would resurrect a live entry into a registry whose relay and reaper have
// already stopped.
func (r *Registry) shutdown(ctx context.Context) {
	close(r.stopReaper)
	<-r.reaperDone

	r.mu.Lock()
	r.closed = true
	keys := make([]sessionKey, 0, len(r.sessions))
	for key, slot := range r.sessions {
		if slot.entry != nil {
			keys = append(keys, key)
		}
	}
	r.mu.Unlock()

	if r.relay != nil {
		if r.relayCancel != nil {
			r.relayCancel()
		}
		if err := r.relay.Stop(ctx); err != nil {
			r.deps.Logger().Info("chat: outbound relay drain failed during shutdown", "err", err.Error())
		}
	}

	for _, key := range keys {
		r.teardown(key, terminalNotice{Reason: "server_shutdown"})
	}
}

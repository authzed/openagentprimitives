package main

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/resolve"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/pipeline"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"

	"github.com/nats-io/nats.go"

	pkgmemory "github.com/authzed/openagentprimitives/pkg/memory"
)

// listenerlessRole reports whether a Channel of this role runs NO inbound
// listener. Only `monitoring` is listener-less — it is a pure outbound sink for
// the monitoring relay. Every other role, INCLUDING output, runs an inbound
// listener on a kind that has one: an output Channel names a delivery
// obligation, not deafness, and still receives inbound (e.g. a human requesting
// a review on reviewbot's role=output slack Channel). See ChannelSpec.Role.
func listenerlessRole(role string) bool {
	return role == spiceboxv1alpha1.ChannelRoleMonitoring
}

// channelManager watches Channel CRs and starts/stops Listeners per Channel.
// v1 uses a 5-second poll loop; a future revision can switch to an informer.
//
// TODO(plan-2): replace the poll ticker with a controller-runtime informer for
// lower latency and reduced API-server load.
type channelManager struct {
	cli                   client.Client
	nc                    *nats.Conn
	mem                   pipeline.Memory
	restartMem            pkgmemory.Memory                       // v2 facade for restart shortcut (channel_msg_ref / turn queries)
	preferences           channelkinds.PreferencesClient         // first-party preferences client for App Home
	personalizableClasses channelkinds.PersonalizableClassLookup // "classes this user has interacted with" for App Home's preferences section
	pipe                  channelkinds.InboundPipeline
	wd                    *statusWatchdog
	authzReader           channelkinds.AuthzReader
	portalMinter          channelkinds.PortalLinkMinter
	// artifactViewMinter reaches the LISTENER (the live-view click handler
	// mints a fresh link per click). The offer sender gets its own copy via
	// the senderResolver; both must be wired from the same source or the
	// button posts but every click reports "not configured". Set via
	// SetArtifactViewMinter once the passthroughlink.Signer exists.
	artifactViewMinter channelkinds.ArtifactViewMinter
	// sessionViewMinter reaches the LISTENER, mirroring artifactViewMinter's
	// wiring shape. The session-view offer has no click-time interaction
	// today (the sender bakes the durable URL directly into the message), so
	// no listener currently consumes this — it is wired for parity/future
	// use, same pattern as artifactViewMinter.
	sessionViewMinter channelkinds.SessionViewMinter
	externalBaseURL   func() string // nil until urlProvider is wired

	mu        sync.Mutex
	listeners map[string]channelkinds.Listener // key = "ns/name" of Channel
	cancels   map[string]context.CancelFunc
	// startedWiring records which optional collaborators were present when
	// each listener was constructed (see optionalWiringLocked). A running
	// listener holds the Deps it was built with forever — startListener
	// early-returns for a key that is already running and nothing restarts it
	// for a spec edit — so without this, a listener that starts in the boot
	// window before main.go wires the minters keeps nil minters for the life
	// of the process. reconcile compares this against the current wiring and
	// restarts only when something became available.
	startedWiring map[string]wiringSet
}

// wiringSet is a bitmask of the optional, late-wired collaborators a listener
// can receive through Deps. Each is nil at construction time and set once,
// milliseconds later, by internal/cmd/channelsd/run() — so the mask only ever grows.
type wiringSet uint8

const (
	wiringPortalMinter wiringSet = 1 << iota
	wiringArtifactViewMinter
	wiringSessionViewMinter
	wiringExternalBaseURL
)

// String renders a mask for logs. Operators reading "listener restarted"
// need to know WHAT arrived, or the restart looks like a flap.
func (w wiringSet) String() string {
	names := []string{}
	for _, e := range []struct {
		bit  wiringSet
		name string
	}{
		{wiringPortalMinter, "portalLinkMinter"},
		{wiringArtifactViewMinter, "artifactViewMinter"},
		{wiringSessionViewMinter, "sessionViewMinter"},
		{wiringExternalBaseURL, "externalBaseURL"},
	} {
		if w&e.bit != 0 {
			names = append(names, e.name)
		}
	}
	return strings.Join(names, ",")
}

func newChannelManager(
	cli client.Client,
	nc *nats.Conn,
	mem pipeline.Memory,
	restartMem pkgmemory.Memory,
	preferences channelkinds.PreferencesClient,
	personalizableClasses channelkinds.PersonalizableClassLookup,
	pipe channelkinds.InboundPipeline,
	wd *statusWatchdog,
	az channelkinds.AuthzReader,
	portalMinter channelkinds.PortalLinkMinter,
) *channelManager {
	return &channelManager{
		cli:                   cli,
		nc:                    nc,
		mem:                   mem,
		restartMem:            restartMem,
		preferences:           preferences,
		personalizableClasses: personalizableClasses,
		pipe:                  pipe,
		wd:                    wd,
		authzReader:           az,
		portalMinter:          portalMinter,
		listeners:             map[string]channelkinds.Listener{},
		cancels:               map[string]context.CancelFunc{},

		startedWiring: map[string]wiringSet{},
	}
}

// SetPortalMinter swaps the channel manager's portal-link minter
// after construction. Used in main.go to wire the minter once the
// passthroughlink.Signer + externalurl.Provider exist (both load
// asynchronously after the manager itself comes up). Thread-safe
// against in-flight startListener calls — they read the minter
// when assembling each Deps, which only happens at listener start.
func (m *channelManager) SetPortalMinter(p channelkinds.PortalLinkMinter) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.portalMinter = p
}

// SetArtifactViewMinter wires the artifact-view link minter that the
// listener's live-view click handler uses to mint a fresh deep-link on
// every click. Mirrors SetPortalMinter: called from main.go once the
// passthroughlink.Signer + externalurl.Provider exist. Thread-safe against
// in-flight startListener calls, which read the minter when assembling Deps.
func (m *channelManager) SetArtifactViewMinter(a channelkinds.ArtifactViewMinter) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.artifactViewMinter = a
}

// SetSessionViewMinter wires the session-view link composer, mirroring
// SetArtifactViewMinter. Thread-safe against in-flight startListener calls,
// which read the minter when assembling Deps.
func (m *channelManager) SetSessionViewMinter(s channelkinds.SessionViewMinter) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessionViewMinter = s
}

// SetExternalBaseURL wires the externalurl.Provider getter after
// construction. Used in main.go alongside SetPortalMinter — the
// urlProvider is available only after the Kubernetes ConfigMap watcher
// has started. Thread-safe: listeners read the getter when starting,
// not when this is set.
func (m *channelManager) SetExternalBaseURL(fn func() string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.externalBaseURL = fn
}

// buildListenerDeps assembles the channelkinds.Deps handed to a listener.
// Extracted from startListener so the wiring is unit-testable: a field
// omitted here silently disables a listener-side feature (the live-view
// click handler needs ArtifactViewMinter), with no compile error.
//
// The minter / base-URL fields are read under m.mu because the Set* methods
// above write them AFTER this function's caller is already running: run()
// does `go mgr.Run(...)`, and Run calls reconcile immediately — before its
// first tick — so a listener can be starting on the manager goroutine while
// main goroutine wires the minters. Goroutine creation is the only
// happens-before edge between them, and it predates every one of those
// writes; a mutex on the write side alone establishes nothing for an
// unlocked reader.
func (m *channelManager) buildListenerDeps(ch *spiceboxv1alpha1.Channel, sec *corev1.Secret) channelkinds.Deps {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.buildListenerDepsLocked(ch, sec)
}

// buildListenerDepsLocked is buildListenerDeps with m.mu already held, for
// callers (startListener) that must read the wiring and record which fields
// were present in the same critical section — otherwise the recorded set can
// disagree with the Deps actually handed to the listener, and the restart
// check below either misses a gap or restarts forever.
func (m *channelManager) buildListenerDepsLocked(ch *spiceboxv1alpha1.Channel, sec *corev1.Secret) channelkinds.Deps {
	nc := m.nc
	return channelkinds.Deps{
		Channel:               ch,
		Secret:                sec,
		NATSPublish:           func(subj string, p []byte) error { return nc.Publish(subj, p) },
		Inbound:               m.pipe,
		K8sClient:             m.cli,
		TouchSetStatus:        m.wd.Touch,
		ForgetSetStatus:       m.wd.Forget,
		ExtendSetStatus:       m.wd.Extend,
		AuthzReader:           m.authzReader,
		PortalLinkMinter:      m.portalMinter,
		ArtifactViewMinter:    m.artifactViewMinter,
		SessionViewMinter:     m.sessionViewMinter,
		ExternalBaseURL:       m.externalBaseURL,
		Memory:                m.restartMem,
		Preferences:           m.preferences,
		PersonalizableClasses: m.personalizableClasses,
	}
}

// optionalWiringLocked reports which late-wired collaborators are currently
// available. Caller must hold m.mu.
func (m *channelManager) optionalWiringLocked() wiringSet {
	var w wiringSet
	if m.portalMinter != nil {
		w |= wiringPortalMinter
	}
	if m.artifactViewMinter != nil {
		w |= wiringArtifactViewMinter
	}
	if m.sessionViewMinter != nil {
		w |= wiringSessionViewMinter
	}
	if m.externalBaseURL != nil {
		w |= wiringExternalBaseURL
	}
	return w
}

// missingWiring returns the collaborators that exist now but did not when the
// listener for key was constructed. Non-zero means that listener is running
// with a permanently degraded feature set — its App Home has no "Manage my
// connections" button, or its live-view clicks answer "not configured" — and
// nothing will ever fix it, because startListener early-returns for a running
// key and no spec edit restarts a listener.
//
// The mask only ever grows (each Set* is called once, at startup, and never
// unsets), so this can never oscillate: at most one restart per listener, in
// the seconds after boot.
func (m *channelManager) missingWiring(key string) wiringSet {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, running := m.listeners[key]; !running {
		return 0
	}
	return m.optionalWiringLocked() &^ m.startedWiring[key]
}

// Run polls for Channel CRs every 5s and reconciles Listener state.
// Blocks until ctx is canceled.
func (m *channelManager) Run(ctx context.Context) {
	logger := log.FromContext(ctx).WithName("channelmanager")
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	m.reconcile(ctx, logger)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.reconcile(ctx, logger)
		}
	}
}

func (m *channelManager) reconcile(ctx context.Context, logger logr.Logger) {
	var list spiceboxv1alpha1.ChannelList
	if err := m.cli.List(ctx, &list); err != nil {
		logger.Error(err, "list channels")
		return
	}
	seen := map[string]bool{}
	for i := range list.Items {
		ch := &list.Items[i]
		key := ch.Namespace + "/" + ch.Name
		seen[key] = true
		if clientHostedHere(ch.Spec.Kind) {
			// builtin (webd) / local (CLI): the host process owns this
			// channel's listener + Connected status; channelsd has no
			// transport. Skip before startListener so we don't do a per-tick
			// Secret Get + resolve only to `unknown kind`/client-hosted skip.
			continue
		}
		if listenerlessRole(ch.Spec.Role) {
			// Monitoring channels are output-only sinks for the
			// monitoring relay; they get no inbound Listener.
			continue
		}
		if !channelValid(ch) && !transportViable(ctx, m.cli, ch) {
			// The Channel is invalid AND cannot speak. Nothing can be said over
			// it, so detach as before. If a listener was running, patch
			// Connected=False before stopping.
			m.mu.Lock()
			running := m.listeners[key] != nil
			m.mu.Unlock()
			if running {
				m.patchConnected(ctx, ch, false, spiceboxv1alpha1.ReasonChannelSocketDetached, "Channel no longer Valid")
			}
			m.stopListener(logger, key)
			continue
		}
		// An invalid-but-viable Channel falls through to startListener on
		// purpose, which also covers the restart case: channelsd coming up
		// DURING an outage must still attach, or the whole outage is silent for
		// everyone who writes in after the restart. The pipeline's own gate is
		// what stops such a Channel from actually serving traffic — this only
		// keeps it able to answer.
		//
		// A listener that started before main.go finished wiring the link
		// minters holds those nils forever, so give it one chance to pick them
		// up. This can only fire in the seconds after boot: the wiring set
		// only grows, so the second pass finds nothing missing.
		if missing := m.missingWiring(key); missing != 0 {
			logger.Info("restarting listener to pick up wiring that arrived after it started",
				"channel", key, "arrived", missing.String())
			m.stopListener(logger, key)
		}
		m.startListener(ctx, key, ch)
		// Bento listeners have a no-op Start; the meaningful entry point
		// is Reconcile(ctx, ch), which builds (or rebuilds, on spec
		// change) the embedded stream. We call it every poll tick — the
		// listener's internal YAML hash makes unchanged-spec ticks a
		// no-op, and spec edits propagate without requiring a
		// delete-and-recreate of the Channel CR.
		m.reconcileBento(ctx, key, ch)
		m.refreshListenerScopes(ctx, key, ch)
	}
	// Stop listeners for Channels that no longer exist.
	m.mu.Lock()
	var gone []string
	for k := range m.listeners {
		if !seen[k] {
			gone = append(gone, k)
		}
	}
	m.mu.Unlock()
	for _, k := range gone {
		m.stopListener(logger, k)
	}
}

func (m *channelManager) startListener(ctx context.Context, key string, ch *spiceboxv1alpha1.Channel) {
	m.mu.Lock()
	_, exists := m.listeners[key]
	m.mu.Unlock()
	if exists {
		return // already running
	}

	logger := log.FromContext(ctx).WithName("channelmanager")

	sec, k, err := resolve.ForChannel(ctx, m.cli, ch)
	if err != nil {
		logger.Error(err, "resolve channel", "channel", key)
		return
	}
	if !k.RelayedByChannelsd() {
		// Client-hosted kind (e.g. local TUI): its Listener runs in the
		// end-user's process, never in a channelsd pod. Skip — do not
		// start a listener, do not patch Connected. The client owns the
		// Channel's lifecycle.
		logger.Info("listener skipped: client-hosted kind", "channel", key, "kind", k.Name())
		return
	}
	// Build the Deps and record which optional collaborators they carried in
	// ONE critical section, so the recorded set is exactly what the listener
	// got. Reading them separately would let a Set* land between the two and
	// record wiring the listener never received — which reads as "nothing is
	// missing" and makes the gap permanent again.
	m.mu.Lock()
	deps := m.buildListenerDepsLocked(ch, sec)
	wiring := m.optionalWiringLocked()
	m.mu.Unlock()

	listener := k.NewListener(deps)
	listenerCtx, cancel := context.WithCancel(ctx)
	if err := listener.Start(listenerCtx); err != nil {
		cancel()
		logger.Error(err, "start listener", "channel", key)
		m.patchConnected(ctx, ch, false, spiceboxv1alpha1.ReasonChannelListenerStartFailed, err.Error())
		return
	}
	m.mu.Lock()
	m.listeners[key] = listener
	m.cancels[key] = cancel
	if m.startedWiring == nil {
		// Tests build channelManager as a struct literal rather than through
		// newChannelManager; a nil map here would panic on assignment.
		m.startedWiring = map[string]wiringSet{}
	}
	m.startedWiring[key] = wiring
	m.mu.Unlock()
	logger.Info("listener started", "channel", key)
	m.patchConnected(ctx, ch, true, spiceboxv1alpha1.ReasonChannelSocketAttached, "")
}

func (m *channelManager) stopListener(logger logr.Logger, key string) {
	m.mu.Lock()
	l, ok := m.listeners[key]
	cancel := m.cancels[key]
	delete(m.listeners, key)
	delete(m.cancels, key)
	delete(m.startedWiring, key)
	m.mu.Unlock()
	if !ok {
		return
	}
	// handlerContext, not context.Background: the kind's Stop logs its own
	// teardown failures through log.FromContext (bento's does, per stream), and
	// channelsd's global logger is a no-op sink — a background context sends
	// those lines nowhere. The returned error is logged rather than discarded
	// for the same reason: both Stop impls return nil today, so this is the
	// path a future one would fail down silently.
	if err := l.Stop(handlerContext(logger)); err != nil {
		logger.Info("listener stop errored", "channel", key, "err", err.Error())
	}
	if cancel != nil {
		cancel()
	}
}

// Stop shuts down all active listeners. Called at process shutdown.
func (m *channelManager) Stop(logger logr.Logger) {
	m.mu.Lock()
	keys := make([]string, 0, len(m.listeners))
	for k := range m.listeners {
		keys = append(keys, k)
	}
	m.mu.Unlock()
	for _, k := range keys {
		m.stopListener(logger, k)
	}
}

// patchConnected stamps the Connected condition.
//
// The patch is built from a FRESH read, never from the caller's `ch`. A JSON
// merge patch replaces status.conditions wholesale, and `ch` is the object as
// listed at the top of the reconcile tick — older than anything written since,
// including the listener's own conditions written during Start, which
// startListener runs immediately before calling this. Patching from the stale
// snapshot reverted those writes on every listener start.
func (m *channelManager) patchConnected(ctx context.Context, ch *spiceboxv1alpha1.Channel, ok bool, reason, message string) {
	var live spiceboxv1alpha1.Channel
	if err := m.cli.Get(ctx, client.ObjectKeyFromObject(ch), &live); err != nil {
		log.FromContext(ctx).Error(err, "re-read Channel to patch Connected condition",
			"channel", ch.Namespace+"/"+ch.Name)
		return
	}
	updated := live.DeepCopy()
	status := metav1.ConditionFalse
	if ok {
		status = metav1.ConditionTrue
	}
	conditions.Set(updated, &updated.Status.Conditions, metav1.Condition{
		Type:    spiceboxv1alpha1.ChannelConditionConnected,
		Status:  status,
		Reason:  reason,
		Message: message,
	})
	if err := m.cli.Status().Patch(ctx, updated, client.MergeFrom(&live)); err != nil {
		log.FromContext(ctx).Error(err, "patch Connected condition", "channel", ch.Name)
	}
}

// channelValid returns true if the Channel's Valid condition is True.
func channelValid(ch *spiceboxv1alpha1.Channel) bool {
	return conditions.IsTrue(ch.Status.Conditions, spiceboxv1alpha1.ChannelConditionValid)
}

// transportViable reports whether ch's own transport can be built and
// authenticated — regardless of whether the Channel is Valid.
//
// It is the question an INVALID Channel is judged on before its listener is
// torn down. A Channel goes Valid=False for two very different families of
// reason, and stopping the listener for both is what produced a silent drop:
// an agent whose credential expired took its Channel down with it, channelsd
// detached the socket, and a user's DM died in the transport layer before any
// code that could answer it ever ran.
//
//   - BINDING broken (no agent, an invalid agent, an unresolvable reply
//     target, an owner policy that does not resolve): the socket is fine. Keep
//     it, and let the pipeline tell the user why nothing is happening. Being
//     told "this agent isn't available" beats being ignored.
//   - TRANSPORT broken (kind not registered, spec the kind rejects, missing or
//     incomplete credentials Secret): there is nothing to speak over. Stop, as
//     before — this is what the Valid gate is for.
//
// These are the same three checks, in the same order, that the channel
// controller runs before it looks at any binding. It answers only "can this
// speak" — never "should this serve traffic", which stays the Valid gate's
// call and is enforced in the pipeline.
func transportViable(ctx context.Context, cli client.Client, ch *spiceboxv1alpha1.Channel) bool {
	if ch == nil {
		return false
	}
	logger := log.FromContext(ctx).WithName("channelmanager")
	k, ok := registry.Get(ch.Spec.Kind)
	if !ok {
		return false
	}
	if err := k.ValidateSpec(ch); err != nil {
		logger.V(1).Info("invalid Channel's transport is not viable: kind rejected the spec",
			"channel", ch.Namespace+"/"+ch.Name, "kind", ch.Spec.Kind, "err", err.Error())
		return false
	}
	sec, _, err := resolve.ForChannel(ctx, cli, ch)
	if err != nil {
		logger.V(1).Info("invalid Channel's transport is not viable: credentials unresolvable",
			"channel", ch.Namespace+"/"+ch.Name, "err", err.Error())
		return false
	}
	for _, key := range k.RequiredSecretKeys(ch) {
		if _, present := sec.Data[key]; !present {
			logger.V(1).Info("invalid Channel's transport is not viable: credentials Secret is incomplete",
				"channel", ch.Namespace+"/"+ch.Name, "missingKey", key)
			return false
		}
	}
	return true
}

// HandleSessionAttached dispatches a session-attached event to the
// listener whose Channel matches OutputChannelName, if that listener
// implements channelkinds.SessionWatcher. No-op when no such listener is
// running (e.g. the OutputChannel CR was deleted between the relay's
// publish and this handler's run, or the listener isn't a
// SessionWatcher implementer).
//
// The full AgentSession is freshly fetched here rather than reconstructed
// from the envelope so the listener's handler sees current state,
// including any fields the relay didn't (and can't) snapshot into the
// fixed-shape NATS payload.
func (m *channelManager) HandleSessionAttached(ctx context.Context, ev channelevents.SessionAttached) {
	logger := log.FromContext(ctx).WithName("session_attached")
	key := ev.Namespace + "/" + ev.OutputChannelName
	m.mu.Lock()
	listener := m.listeners[key]
	m.mu.Unlock()
	if listener == nil {
		// No active listener for that Channel. Either the Channel CR isn't
		// started yet (the startup walk will pick the session up) or it has
		// been deleted. Benign, but LOG it: when a cron thread fails to index
		// live, the human symptom is a reply silently dropped as "unowned
		// thread" minutes later, and this is one of the three places the
		// event can vanish. A bare return here made it impossible to tell
		// which one from logs.
		logger.V(1).Info("no listener for the output Channel; skipping live index (startup walk will catch it)",
			"outputChannel", key, "session", ev.Namespace+"/"+ev.SessionName)
		return
	}
	sw, ok := listener.(channelkinds.SessionWatcher)
	if !ok {
		// Listener kind doesn't implement SessionWatcher — it maintains no
		// per-session in-process state. Expected for most kinds; logged at
		// V(1) for the same diagnosability reason as above.
		logger.V(1).Info("output Channel's listener is not a SessionWatcher; skipping live index",
			"outputChannel", key, "session", ev.Namespace+"/"+ev.SessionName)
		return
	}

	var sess spiceboxv1alpha1.AgentSession
	if err := m.cli.Get(ctx, client.ObjectKey{
		Namespace: ev.Namespace, Name: ev.SessionName,
	}, &sess); err != nil {
		logger.Info("Get AgentSession failed",
			"session", ev.Namespace+"/"+ev.SessionName, "err", err.Error())
		return
	}
	sw.SessionUpdated(ctx, &sess)
}

// refreshListenerScopes re-derives the running listener's permission
// requirements against current cluster state — for slack, the capabilities of
// the AgentClass this Channel is bound to, which decide which bot-token scopes
// it actually needs.
//
// It rides the existing 5s reconcile tick because channelsd is not
// controller-runtime and has no watch to hang this off. The comparison itself
// runs against what the listener already has cached from its transport (slack:
// the granted-scope header from auth.test), so a tick costs no call out, and
// the ScopeRefresher contract requires the listener to write only when the
// object disagrees with it. Without this, ScopesValid would keep reporting
// whatever was true when the listener connected, and an operator who fixed a
// capability — or who granted the scope the condition asked them for — would
// watch a stale condition contradict them.
//
// A listener MAY refresh that cache from its transport on a far slower clock of
// its own (slack does, since a re-installed Slack app changes nothing in the
// cluster). The ScopeRefresher contract requires such a call to be taken OFF
// this goroutine: it is the single reconcile loop, so an inline round-trip
// would put every other Channel's listener start/stop and Connected status
// behind one slow third-party response.
//
// ch is this tick's freshly listed Channel. Handing it over is what lets the
// listener see a rebind to a different AgentClass — its own Deps.Channel is the
// snapshot it started with, and a running listener is never restarted for a
// spec edit — and lets it compare against current status without a read.
//
// Dispatch is via the optional interface, never a kind name: kinds that do not
// implement ScopeRefresher are left alone.
func (m *channelManager) refreshListenerScopes(ctx context.Context, key string, ch *spiceboxv1alpha1.Channel) {
	m.mu.Lock()
	l := m.listeners[key]
	m.mu.Unlock()
	if l == nil {
		return // listener didn't start (e.g. resolve failure); nothing to refresh
	}
	if r, ok := l.(channelkinds.ScopeRefresher); ok {
		r.RefreshScopes(ctx, ch)
	}
}

// reconcileBento invokes the bento listener's per-Channel Reconcile
// hook so spec edits propagate to the embedded stream. Type-asserts
// against an unexported anonymous interface so this file doesn't
// import pkg/channels/channelkinds/bento — kept as a duck-typed match against
// any future kind that adopts the same Reconcile shape.
func (m *channelManager) reconcileBento(ctx context.Context, key string, ch *spiceboxv1alpha1.Channel) {
	m.mu.Lock()
	l := m.listeners[key]
	m.mu.Unlock()
	if l == nil {
		return // listener didn't start (e.g. resolve failure); nothing to reconcile
	}
	rec, ok := l.(interface {
		Reconcile(ctx context.Context, ch *spiceboxv1alpha1.Channel) error
	})
	if !ok {
		return // listener doesn't expose Reconcile (every kind except bento today)
	}
	if err := rec.Reconcile(ctx, ch); err != nil {
		log.FromContext(ctx).Error(err, "bento listener Reconcile", "channel", key)
	}
}

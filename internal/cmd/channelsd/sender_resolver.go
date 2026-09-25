package main

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/resolve"

	"github.com/nats-io/nats.go"
)

// errClientHostedKind is returned by resolveDeps for kinds whose
// RelayedByChannelsd() is false. The three SenderResolver methods
// translate it into a nil Sender/Sink so the outbound relay drops the
// envelope rather than erroring — the client process renders it.
var errClientHostedKind = errors.New("sender resolution: client-hosted kind not relayed by channelsd")

// senderResolver implements outbound.SenderResolver. It looks up the Channel
// for a given AgentSession, loads the Secret, and constructs the kind's Sender
// via the registry. Results are cached per Channel (namespace/name key), keyed
// as "ns/channelName" for the default sender and
// "ns/channelName|subChannelName" for sub-channel senders.
type senderResolver struct {
	cli client.Client
	nc  *nats.Conn
	wd  *statusWatchdog
	// assetFetcher resolves OutboundUserMessagePayload.Attachments[i] to
	// bytes at delivery time. Wired through Deps so kind-specific senders
	// (slack files.uploadV2, future webhook multipart, etc.) don't need
	// to know about the operator URL or channelsd-system token.
	assetFetcher channelkinds.AssetFetcher
	// authzReader gives sub-channel senders read-only SpiceDB access
	// (e.g. tool_approval ephemeral fan-out via LookupSubjects). nil-safe.
	authzReader channelkinds.AuthzReader
	// artifactViewMinter, when non-nil, mints webd artifact-view deep-links
	// for the live_view_offer sub-channel sender. Wired once at startup.
	// Declared as the interface type so the zero value is a true nil
	// interface — see AGENTS.md "Nil interfaces: never assign a typed-nil pointer".
	artifactViewMinter channelkinds.ArtifactViewMinter
	// sessionViewMinter, when non-nil, composes the durable webd
	// session-view page URL for the session_view_offer sub-channel sender.
	// Wired once at startup. Declared as the interface type so the zero
	// value is a true nil interface.
	sessionViewMinter channelkinds.SessionViewMinter
	// agentUIMinter, when non-nil, composes the webd agent-UI shell page URL
	// for the agent_ui_offer sub-channel sender. Wired once at startup.
	// Declared as the interface type so the zero value is a true nil
	// interface — a *agentUIMinter variable left nil and assigned here would
	// satisfy `!= nil` at every guard and then panic on the first call.
	agentUIMinter channelkinds.AgentUIMinter
	// approverFanoutLimit caps how many approvers an approval prompt fans
	// out to (delivery-only; never an authorization bound). 0 ⇒
	// channelkinds.DefaultApproverFanoutLimit. Set from the
	// --approver-fanout-limit flag at startup.
	approverFanoutLimit int

	mu    sync.Mutex
	cache map[string]channelkinds.Sender // key = "ns/name" or "ns/name|subch"
	// wiringGen counts changes to the startup-wired collaborators above. A
	// resolve captures it in the same lock acquisition as its cache-miss
	// check and hands it back to cacheSenderIfCurrent; a Set* landing in
	// between bumps it, and the Sender built against the superseded wiring
	// is dropped instead of being written into the cache that Set just
	// cleared.
	//
	// Without it, invalidateSenders is a lost update rather than an
	// invalidation: the build runs with r.mu released (resolveDeps makes two
	// API-server Gets), so clear(r.cache) cannot reach a Sender that is
	// mid-flight, and the entry that lands afterwards is never rebuilt —
	// the Set* methods are the only callers of invalidateSendersLocked and
	// each runs once at startup.
	wiringGen uint64
	// sinkCache holds the per-Channel StreamDeltaSink. Sinks own per-thread
	// debounce + buffer state across deltas, so they MUST persist across
	// resolveDeps calls — a fresh sink on every event would erase the
	// buffered text and re-flush from empty.
	sinkCache map[string]channelkinds.StreamDeltaSink // key = "ns/channelName"
}

func newSenderResolver(cli client.Client, nc *nats.Conn, af channelkinds.AssetFetcher) *senderResolver {
	return &senderResolver{
		cli:          cli,
		nc:           nc,
		assetFetcher: af,
		cache:        map[string]channelkinds.Sender{},
		sinkCache:    map[string]channelkinds.StreamDeltaSink{},
	}
}

// The Set* methods below all wire a startup-supplied collaborator into the
// Deps every future Sender is built from. Two facts govern how they are
// written, and getting either wrong is silent:
//
//   - They run AFTER their own readers. internal/cmd/channelsd's run() calls
//     relay.Start (whose out.> subscription dispatches on its own goroutine)
//     and `go wd.Run` before it has a passthroughlink.Signer and a webd URL to
//     build the minters from. So every field they touch is shared mutable
//     state and must be written under r.mu — the same lock depsSnapshot reads
//     it under.
//   - Senders are CACHED per Channel and never rebuilt. A Sender resolved in
//     the boot window would keep the nil minter it was built with for the life
//     of the process: "View live" would report not-configured on that Channel
//     forever, with nothing in the logs to say why. So a late arrival must
//     also drop the senders built without it — invalidateSenders below.

// SetAuthzReader wires the channel-kinds AuthzReader (typically wrapping
// the channelsd SpiceDB client). Called once at startup, before the
// first SenderFor; nil-safe (sub-channel senders fall back to an empty
// LookupSubjects result, which still renders a valid public thread
// message but with no per-approver ephemerals).
func (r *senderResolver) SetAuthzReader(az channelkinds.AuthzReader) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.authzReader = az
	r.invalidateSendersLocked()
}

// SetWatchdog wires the status watchdog into Senders constructed by this
// resolver. Called once at startup, before the first SenderFor. Without this,
// Senders' setStatus/postMessage paths never call Touch/Forget and the
// watchdog never sees mid-flight progress.
func (r *senderResolver) SetWatchdog(wd *statusWatchdog) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.wd = wd
	r.invalidateSendersLocked()
}

// SetArtifactViewMinter wires the artifact-view link minter into Deps for
// the live_view_offer sub-channel sender. Called once at startup after the
// webd externalurl.Provider and passthroughlink.Signer are both ready.
// Declared as the interface type so nil stays a true nil interface.
func (r *senderResolver) SetArtifactViewMinter(m channelkinds.ArtifactViewMinter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.artifactViewMinter = m
	r.invalidateSendersLocked()
}

// SetSessionViewMinter wires the session-view link composer into Deps for
// the session_view_offer sub-channel sender. Called once at startup;
// unlike SetArtifactViewMinter this needs no signer, so it can be wired as
// soon as the webd externalurl.Provider exists.
func (r *senderResolver) SetSessionViewMinter(m channelkinds.SessionViewMinter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessionViewMinter = m
	r.invalidateSendersLocked()
}

// SetAgentUIMinter wires the agent-UI link composer into Deps for the
// agent_ui_offer sub-channel sender. Called once at startup; like
// SetSessionViewMinter it needs no signer, so it can be wired as soon as the
// webd externalurl.Provider exists.
func (r *senderResolver) SetAgentUIMinter(m channelkinds.AgentUIMinter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.agentUIMinter = m
	r.invalidateSendersLocked()
}

// SetApproverFanoutLimit wires the operator-configured approval fan-out cap
// into Deps. Called once at startup; 0 keeps the kind-side default.
func (r *senderResolver) SetApproverFanoutLimit(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.approverFanoutLimit = n
	r.invalidateSendersLocked()
}

// invalidateSendersLocked drops every cached Sender, and bumps wiringGen so a
// Sender already mid-build against the superseded wiring is not written in
// behind it, so the next resolve rebuilds against the current wiring either
// way. Caller must hold r.mu.
//
// sinkCache is deliberately left alone. Sinks own per-thread debounce and
// buffer state across deltas; dropping one mid-stream would erase the
// buffered text and re-flush the thread from empty — a visible corruption,
// where a stale sink merely misses wiring the sink does not use.
func (r *senderResolver) invalidateSendersLocked() {
	// The generation bumps even when the cache is empty. "Nothing cached" is
	// not "nothing to invalidate": during the boot window an empty cache is
	// the normal state of a resolve that has already snapshotted the old
	// wiring and has not written its entry yet, and that in-flight build is
	// exactly what this invalidation has to reach.
	r.wiringGen++
	clear(r.cache)
}

// cacheSenderIfCurrent stores sender under key iff no Set* has replaced the
// wiring since gen was captured, and reports whether it did.
//
// The resolver deliberately does not hold r.mu across a build — resolveDeps
// makes two API-server Gets — so invalidateSendersLocked on its own cannot
// drop a Sender that is mid-flight, and the entry landing after it would
// silently outlive the invalidation. The generation closes that gap.
//
// Callers capture gen in the same lock acquisition as their cache-miss check,
// which is earlier than the depsSnapshot the Sender is actually built from.
// That is deliberate and conservative: gen at capture <= gen at snapshot, so a
// superseded snapshot always implies a changed generation here, while a Set*
// landing between the two merely costs one extra rebuild of a Sender that was
// in fact current.
func (r *senderResolver) cacheSenderIfCurrent(key string, sender channelkinds.Sender, gen uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.wiringGen != gen {
		return false
	}
	r.cache[key] = sender
	return true
}

// resolveDeps loads the Channel and Secret for the given AgentSession and
// returns the kind, the constructed Deps, and the cache key for the default
// sender. Callers build either the default sender or a sub-channel sender from
// the returned deps.
func (r *senderResolver) resolveDeps(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) (channelkinds.Kind, channelkinds.Deps, string, error) {
	ch, sec, k, err := resolve.ForSession(ctx, r.cli, sess)
	if err != nil {
		return nil, channelkinds.Deps{}, "", err
	}
	if !k.RelayedByChannelsd() {
		// Client-hosted kind: channelsd has no transport to send on.
		// Returning the kind with a sentinel error makes SenderFor /
		// SubChannelSenderFor / StreamDeltaSinkFor degrade to nil so the
		// outbound relay drops the envelope (it is rendered by the
		// client's in-process host instead).
		return k, channelkinds.Deps{}, "", errClientHostedKind
	}
	deps := r.depsSnapshot()
	deps.Channel = ch
	deps.Secret = sec
	cacheKey := sess.Namespace + "/" + sess.Spec.InputChannel.Name
	return k, deps, cacheKey, nil
}

// depsSnapshot builds the Channel-independent half of the Deps handed to every
// Sender this resolver constructs: the startup-wired collaborators.
//
// It is split out from resolveDeps because those fields are written by the
// Set* methods below AFTER the goroutines that call resolveDeps are already
// running (see the file-level comment on the setters), so reading them is a
// concurrency question that has nothing to do with the per-Channel lookup.
// Keeping it one lock-guarded copy means there is exactly one place to get
// that right — and one place for a test to reach without a Kubernetes client.
func (r *senderResolver) depsSnapshot() channelkinds.Deps {
	r.mu.Lock()
	defer r.mu.Unlock()

	nc := r.nc
	deps := channelkinds.Deps{
		NATSPublish:         func(subj string, p []byte) error { return nc.Publish(subj, p) },
		Inbound:             nil, // Senders do not call into the inbound pipeline
		K8sClient:           r.cli,
		AssetFetcher:        r.assetFetcher,
		AuthzReader:         r.authzReader,
		ArtifactViewMinter:  r.artifactViewMinter,
		SessionViewMinter:   r.sessionViewMinter,
		AgentUIMinter:       r.agentUIMinter,
		ApproverFanoutLimit: r.approverFanoutLimit,
	}
	if r.wd != nil {
		deps.TouchSetStatus = r.wd.Touch
		deps.ForgetSetStatus = r.wd.Forget
		deps.ExtendSetStatus = r.wd.Extend
	}
	return deps
}

// SenderFor returns the Sender for the AgentSession's owning Channel,
// constructing and caching it on first lookup.
func (r *senderResolver) SenderFor(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) (channelkinds.Sender, error) {
	if sess.Spec.InputChannel == nil {
		return nil, fmt.Errorf("session %s/%s is not channel-attached", sess.Namespace, sess.Name)
	}
	key := sess.Namespace + "/" + sess.Spec.InputChannel.Name
	r.mu.Lock()
	if s, ok := r.cache[key]; ok {
		r.mu.Unlock()
		return s, nil
	}
	gen := r.wiringGen
	r.mu.Unlock()

	k, deps, _, err := r.resolveDeps(ctx, sess)
	if errors.Is(err, errClientHostedKind) {
		return nil, nil // client-hosted kind; relay drops the envelope
	}
	if err != nil {
		return nil, err
	}
	sender := k.NewSender(deps)
	if !r.cacheSenderIfCurrent(key, sender, gen) {
		// The Sender is still returned: this envelope goes out with the
		// wiring it was built from, which is a missing button at worst.
		// Dropping it would trade that for a missing message.
		ctrllog.FromContext(ctx).Info(
			"sender resolution: wiring changed while this Sender was being built; "+
				"delivering from it but not caching it, so the next lookup rebuilds",
			"session", sess.Namespace+"/"+sess.Name, "channel", key)
	}
	return sender, nil
}

// SubChannelSenderFor returns the kind-specific Sender for the named
// sub-channel (e.g., "permission_request"). nil + nil means the kind doesn't
// implement that sub-channel; the relay drops the envelope silently.
func (r *senderResolver) SubChannelSenderFor(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, name string) (channelkinds.Sender, error) {
	if sess.Spec.InputChannel == nil {
		return nil, fmt.Errorf("session %s/%s is not channel-attached", sess.Namespace, sess.Name)
	}
	key := sess.Namespace + "/" + sess.Spec.InputChannel.Name + "|" + name
	r.mu.Lock()
	if s, ok := r.cache[key]; ok {
		r.mu.Unlock()
		return s, nil
	}
	gen := r.wiringGen
	r.mu.Unlock()

	k, deps, _, err := r.resolveDeps(ctx, sess)
	if errors.Is(err, errClientHostedKind) {
		return nil, nil // client-hosted kind; relay drops the envelope
	}
	if err != nil {
		return nil, err
	}
	sender := k.SubChannelSender(name, deps)
	// sender may be nil when the kind doesn't implement the sub-channel;
	// we still cache the nil so we don't re-hit k8s on every envelope.
	if !r.cacheSenderIfCurrent(key, sender, gen) {
		ctrllog.FromContext(ctx).Info(
			"sub-channel sender resolution: wiring changed while this Sender was being built; "+
				"delivering from it but not caching it, so the next lookup rebuilds",
			"session", sess.Namespace+"/"+sess.Name, "channel", key, "subChannel", name)
	}
	return sender, nil
}

// StreamDeltaSinkFor returns the kind-specific StreamDeltaSink for the
// AgentSession's channel, constructing and caching a per-Channel singleton
// on first lookup. nil means the kind opts out of stream-delta rendering;
// the relay drops KindAssistantStreamDelta envelopes silently in that case.
//
// The cache is essential: sinks own per-thread debounce + buffer state
// across deltas. A fresh sink on every event would erase the buffered text
// and re-flush from empty.
//
// That is also why there is no wiringGen check here, unlike the two Sender
// paths: invalidateSendersLocked deliberately leaves sinkCache alone, so a
// sink built against superseded wiring is kept on purpose rather than lost.
// See the note on invalidateSendersLocked before adding one.
//
// The AgentClass governs whether stream rendering is enabled at all:
// when Spec.Channels.ShowAssistantStream is false (the default), this
// returns nil so the relay drops KindAssistantStreamDelta envelopes and
// the channel only sees update_status / update_plan / respond_to_user.
func (r *senderResolver) StreamDeltaSinkFor(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) (channelkinds.StreamDeltaSink, error) {
	if sess.Spec.InputChannel == nil {
		return nil, fmt.Errorf("session %s/%s is not channel-attached", sess.Namespace, sess.Name)
	}
	if !r.streamRenderingEnabled(ctx, sess) {
		return nil, nil
	}
	key := sess.Namespace + "/" + sess.Spec.InputChannel.Name
	r.mu.Lock()
	if s, ok := r.sinkCache[key]; ok {
		r.mu.Unlock()
		return s, nil
	}
	r.mu.Unlock()

	_, deps, _, err := r.resolveDeps(ctx, sess)
	if errors.Is(err, errClientHostedKind) {
		return nil, nil // client-hosted kind; relay drops the envelope
	}
	if err != nil {
		return nil, err
	}
	sink := r.buildSink(deps)
	r.mu.Lock()
	r.sinkCache[key] = sink
	r.mu.Unlock()
	return sink, nil
}

// streamRenderingEnabled reads the AgentClass's
// Spec.Channels.ShowAssistantStream flag. Default false. Errors fetching
// the class also default to false (safer to render less than to spam the
// channel with the agent's internal narrative on a misconfigured
// session). The drop is logged so operators can see why nothing is
// rendering — the silent-error rule applies.
func (r *senderResolver) streamRenderingEnabled(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) bool {
	logger := ctrllog.FromContext(ctx).WithValues(
		"session", sess.Namespace+"/"+sess.Name,
		"agentClass", sess.Spec.Class,
	)
	if sess.Spec.Class == "" {
		logger.Info("stream rendering: drop, session has no agent class")
		return false
	}
	var class spiceboxv1alpha1.AgentClass
	if err := r.cli.Get(ctx, types.NamespacedName{
		Namespace: sess.Namespace, Name: sess.Spec.Class,
	}, &class); err != nil {
		logger.Info("stream rendering: drop, AgentClass lookup failed", "err", err.Error())
		return false
	}
	if class.Spec.Channels == nil {
		return false
	}
	return class.Spec.Channels.ShowAssistantStream
}

// buildSink constructs the kind-specific StreamDeltaSink for the
// resolved Deps via the registry. Each kind owns its sink wiring
// behind the channelkinds.Kind interface; returning nil opts out
// (the relay drops KindAssistantStreamDelta for that kind silently).
func (r *senderResolver) buildSink(deps channelkinds.Deps) channelkinds.StreamDeltaSink {
	if deps.Channel == nil {
		return nil
	}
	k, ok := registry.Get(deps.Channel.Spec.Kind)
	if !ok {
		return nil
	}
	return k.NewStreamDeltaSink(deps)
}

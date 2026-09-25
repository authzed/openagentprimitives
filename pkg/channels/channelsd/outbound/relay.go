// Package outbound subscribes to ap.session.>.out.> on NATS and dispatches
// each envelope to the kind-specific Sender via session lookup. The session
// it looks up comes from the NATS SUBJECT — the identity a publisher's
// per-session JWT authorized — not from the envelope body; see handle.
package outbound

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/nats-io/nats.go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkey"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/watchdog"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/parkedprompt"
	"github.com/authzed/openagentprimitives/pkg/platform/nats/subjects"
)

// Relay subscribes to the outbound subject pattern and dispatches each
// envelope to the correct Sender via session.spec.channel lookup.
type Relay struct {
	NC      *nats.Conn
	K8s     client.Client
	Senders SenderResolver

	// ApplyStatusEvent folds one ordered status event into the per-session
	// silence-watchdog machine, reporting whether the envelope should still
	// reach the Sender (Forward) and whether the indicator was torn down
	// (ClearCaption). tool_activity and stream-delta progress fold in as
	// EvToolActivity (timing re-arm, never forwarded); setStatus-bearing
	// notification / plan_update captions as EvCaption (forwarded only when not
	// stale or yielded). Optional: nil drops the status signal and forwards
	// captions unconditionally.
	ApplyStatusEvent func(ctx context.Context, ns, name string, ev watchdog.Event) watchdog.ApplyResult

	// OnTurnActivity receives every KindTurnActivity envelope. It is a seam of
	// its own, not part of ApplyStatusEvent, because it owns the leakage-pause
	// carve-out: a paused leakage approval is invisible in-channel, so the
	// watchdog must keep ANNOUNCING it rather than fold it in as a yield (which
	// would clear and disarm). Never routed to a Sender; nil drops the envelope.
	OnTurnActivity func(ctx context.Context, ns, name string, active bool, cause string, seq uint64, uid string)

	// Mem, when non-nil, durably records relayed prompt-request envelopes so a
	// prompt the session is parked on can be re-surfaced later — including after
	// this process restarts. Optional: webd wires its own relay without one,
	// because webd holds a read-only memory token by design.
	Mem memory.Memory

	// Accept, when non-nil, is consulted with an envelope's (namespace, name)
	// BEFORE the AgentSession is loaded from the API server: returning false
	// drops the envelope with no Get and no Sender resolution.
	//
	// Every consumer subscribes to the same cluster-wide "ap.session.*.*.out.>";
	// narrowing that is not what this seam is for. It is the escape hatch for a
	// consumer that already KNOWS, from process-local state, that it can never
	// deliver: webd's chat Registry serves only its own live-session table, so a
	// rate-limited round-trip to learn that a Slack- or CLI-driven session is not
	// one of them buys nothing — and spends it on the single goroutine nats.go
	// serializes this subscription's callbacks on, directly in front of the chat
	// user's own reply.
	//
	// nil ⇒ accept everything: channelsd (which relays for every session in the
	// cluster), `oap agent chat`, and the e2e harness.
	Accept func(ns, name string) bool

	// RecordDeliverability opts this relay into stamping the Channel
	// Deliverable condition from send outcomes (see noteDeliveryOutcome).
	// Only channelsd sets it: channelsd owns Channel liveness conditions
	// (Connected, ScopesValid) and holds Channel/status RBAC, while webd's
	// chat relay and `oap agent chat` are deliberately read-only on cluster
	// state — their sends must not require (or attempt) status writes.
	RecordDeliverability bool

	// StreamDeclineTTL bounds how long the relay may remember that a kind
	// DECLINED to render stream deltas for a session — i.e. that
	// Senders.StreamDeltaSinkFor returned a nil sink. 0 ⇒ defaultStreamDeclineTTL;
	// negative ⇒ never remember (always re-resolve). See declineStreamDeltas for
	// why only the declining answer is ever remembered.
	StreamDeclineTTL time.Duration

	mu  sync.Mutex
	sub *nats.Subscription

	// declMu guards declinedStreams, which maps "<ns>/<name>" to the instant its
	// stream-delta decline expires.
	declMu          sync.Mutex
	declinedStreams map[string]time.Time

	// delivMu guards delivOutcomes, which maps a Channel's "<ns>/<name>" to
	// the last Deliverable outcome recorded on it — the memo that keeps
	// noteDeliveryOutcome from writing status once per relayed envelope.
	delivMu       sync.Mutex
	delivOutcomes map[string]deliverableOutcome
}

// defaultStreamDeclineTTL is how long a stream-delta decline is remembered when
// StreamDeclineTTL is unset. Short enough that flipping an AgentClass's
// spec.channels.showAssistantStream ON is visible well within one turn, long
// enough that a whole reply's worth of tokens costs a handful of lookups rather
// than two per token.
const defaultStreamDeclineTTL = 2 * time.Second

// maxDeclinedStreamSessions caps the decline map. Past the cap the relay stops
// memoizing rather than growing without bound — the fail-safe direction, since
// not memoizing only costs the live lookups this optimization avoids.
const maxDeclinedStreamSessions = 512

// SenderResolver looks up a Sender for a given AgentSession's channel.
type SenderResolver interface {
	SenderFor(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) (channelkinds.Sender, error)
	// SubChannelSenderFor returns the kind-specific Sender for the named
	// sub-channel (e.g., "tool_approval", "interaction"). nil + nil means the kind
	// doesn't implement that sub-channel; the relay drops the envelope
	// silently.
	SubChannelSenderFor(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, name string) (channelkinds.Sender, error)
	// StreamDeltaSinkFor returns the kind-specific StreamDeltaSink for the
	// AgentSession's channel. nil + nil means the kind opts out of live
	// stream-delta rendering; the relay drops KindAssistantStreamDelta
	// envelopes silently in that case.
	StreamDeltaSinkFor(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) (channelkinds.StreamDeltaSink, error)
}

// Start subscribes; safe to call once.
func (r *Relay) Start(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sub != nil {
		return nil
	}
	// Every session's outbound tree, cluster-wide: the ns and name tokens are
	// wildcarded and the kind suffix is left open. handle re-derives the
	// session from each message's own subject.
	sub, err := r.NC.Subscribe(subjects.AnyOutTree, func(msg *nats.Msg) {
		r.handle(ctx, msg)
	})
	if err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	r.sub = sub
	return nil
}

// Stop drains the subscription. Safe to call multiple times.
func (r *Relay) Stop(_ context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sub == nil {
		return nil
	}
	err := r.sub.Drain()
	r.sub = nil
	return err
}

// touchOnDelta invokes touch when an AssistantStreamDelta payload represents
// real forward progress (text_delta, tool_use_start). Terminal events
// (tool_use_stop, stop) and unparseable payloads are skipped. Kept separate from
// the sink path so a malformed payload doesn't block sink routing.
func touchOnDelta(raw json.RawMessage, touch func()) {
	var pl channelevents.AssistantStreamDeltaPayload
	if err := json.Unmarshal(raw, &pl); err != nil {
		return
	}
	switch pl.EventType {
	case "text_delta", "tool_use_start":
		touch()
	}
}

// streamDeltasDeclined reports whether the kind recently declined to render
// stream deltas for this session, and the decline has not yet lapsed.
func (r *Relay) streamDeltasDeclined(ns, name string) bool {
	if r.streamDeclineTTL() <= 0 {
		return false
	}
	r.declMu.Lock()
	defer r.declMu.Unlock()
	until, ok := r.declinedStreams[ns+"/"+name]
	return ok && time.Now().Before(until)
}

// declineStreamDeltas remembers, for StreamDeclineTTL, that
// Senders.StreamDeltaSinkFor resolved a NIL sink for this session — so the
// deltas that follow (the runner emits one per SSE text_delta, ungated on
// whether any kind wants them) are dropped without re-loading the AgentSession
// and re-reading the AgentClass gate for every single token.
//
// ONLY the declining answer is ever remembered, and that asymmetry is the whole
// safety argument. channelkinds.ShowsAssistantStream — the gate the nil sink
// usually comes from — is a disclosure gate: the raw model narrative "must never
// leak to a channel ... unless an AgentClass explicitly turns it on". A memo of
// "yes, stream" would keep streaming for up to a TTL after the flag went OFF; a
// memo of "no" can only withhold for up to a TTL after it went ON, and the next
// delta past the window re-asks. The stale direction is the fail-closed one, and
// the permitting answer is re-resolved live on every delta. Same reasoning for
// the other nil-sink case: webd's chat Registry refusing a session it does not
// own or has torn down.
func (r *Relay) declineStreamDeltas(ns, name string) {
	ttl := r.streamDeclineTTL()
	if ttl <= 0 {
		return
	}
	key := ns + "/" + name
	now := time.Now()
	r.declMu.Lock()
	defer r.declMu.Unlock()
	if r.declinedStreams == nil {
		r.declinedStreams = map[string]time.Time{}
	}
	if len(r.declinedStreams) >= maxDeclinedStreamSessions {
		for k, until := range r.declinedStreams {
			if !now.Before(until) {
				delete(r.declinedStreams, k)
			}
		}
		if _, alreadyTracked := r.declinedStreams[key]; !alreadyTracked && len(r.declinedStreams) >= maxDeclinedStreamSessions {
			return // at cap with nothing expired — stop memoizing rather than grow
		}
	}
	r.declinedStreams[key] = now.Add(ttl)
}

func (r *Relay) streamDeclineTTL() time.Duration {
	if r.StreamDeclineTTL == 0 {
		return defaultStreamDeclineTTL
	}
	return r.StreamDeclineTTL
}

// isStatusCaptionKind reports whether env is an ordered status caption that the
// silence-watchdog machine should gate: a setStatus-bearing notification (with
// non-empty text) or a plan_update snapshot. An empty-text Notification is the
// "clear my indicator" sentinel, NOT a caption — it must always reach the
// Sender's clear path, so it returns false here. A notification whose payload
// can't be decoded is not gated (let it through rather than swallow it on a
// decode failure).
func isStatusCaptionKind(env channelevents.Envelope) bool {
	switch env.Kind {
	case channelevents.KindPlanUpdate:
		return true
	case channelevents.KindNotification:
		var pl channelevents.NotificationPayload
		if err := json.Unmarshal(env.Payload, &pl); err != nil {
			return false
		}
		return pl.Text != ""
	default:
		return false
	}
}

// captionTextOf extracts the caption text an ordered status-caption envelope
// carries, for the EvCaption fold's Event.Text — the machine remembers it as the
// revert target for a later EvOperationActivity "cleared" tick. KindPlanUpdate
// has no active-step caption; "" is a safe revert target (EffectiveLine falls
// back to the previous caption). An undecodable notification logs and returns
// "": isStatusCaptionKind already let it through, so this re-decode is defense
// in depth, not the only guard.
func captionTextOf(env channelevents.Envelope) string {
	switch env.Kind {
	case channelevents.KindNotification:
		var pl channelevents.NotificationPayload
		if err := json.Unmarshal(env.Payload, &pl); err != nil {
			log.Log.Info("outbound relay: captionTextOf failed to decode notification payload",
				"session", env.Session.Namespace+"/"+env.Session.Name, "err", err.Error())
			return ""
		}
		return pl.Text
	default:
		return ""
	}
}

// notePendingPrompt durably records a relayed generic interaction REQUEST so a
// parked prompt can be re-surfaced later — to a user re-interacting from another
// device, to a surface that attaches afterwards, or after this process restarts.
// Applied/decision and non-prompt kinds are ignored.
//
// ONLY ResurfaceCached categories are stored. ResurfaceRegenerate
// (credential_link) never is: its actionable part is a freshly-minted signed
// link, and persisting that would leave a replayable credential in storage —
// channelinteractions.RegenerateAtPark rebuilds the prompt from the session's
// phase instead.
//
// Called BEFORE the envelope is dispatched, so a prompt can never become visible
// without its durable record in place, and no decision can land before the
// write. A write failure is logged and delivery proceeds: refusing to deliver
// would trade a lost re-surface for the silent hang this path exists to prevent.
// No-op when mem is nil (webd's relay holds a read-only memory token by design).
func notePendingPrompt(ctx context.Context, mem memory.Memory, ns, name string, env channelevents.Envelope) {
	if mem == nil {
		return
	}
	if env.Kind != channelevents.KindInteractionRequest {
		return
	}
	var pl channelevents.InteractionRequestPayload
	if err := json.Unmarshal(env.Payload, &pl); err != nil || pl.RequestRef == "" {
		return
	}
	cat, ok := channelinteractions.Get(pl.Category)
	if !ok || cat.Resurface != channelinteractions.ResurfaceCached {
		return
	}
	raw, err := json.Marshal(env)
	if err != nil {
		log.FromContext(ctx).Info("outbound relay: marshal prompt envelope for durable note failed",
			"session", ns+"/"+name, "requestRef", pl.RequestRef, "err", err.Error())
		return
	}
	if err := parkedprompt.Note(ctx, mem, memory.Scope{Kind: "session", ID: ns + "/" + name}, parkedprompt.Content{
		RequestRef:    pl.RequestRef,
		Category:      pl.Category,
		Interruptible: pl.Interruptible,
		Envelope:      raw,
	}); err != nil {
		log.FromContext(ctx).Info("outbound relay: durable parked-prompt note failed; delivering anyway (it will not survive a restart)",
			"session", ns+"/"+name, "requestRef", pl.RequestRef, "err", err.Error())
	}
}

func (r *Relay) handle(ctx context.Context, msg *nats.Msg) {
	logger := log.FromContext(ctx).WithValues("subject", msg.Subject)

	// Not every out-subject is ours: "ap.session.*.*.out.>" is cluster-wide and
	// kind-blind, and the metaagent family (scope approval, notice) has its own
	// dedicated subscription in internal/cmd/channelsd that routes it to a SUB-channel
	// sender. The skip reads the SUBJECT, before the body is touched, because
	// those payloads are raw JSON rather than Envelopes: decoding one here yields
	// Kind "" and emits a "validation failed" drop for traffic that is working
	// correctly — noise on top of the one log line a real drop must be found by.
	// See Kind.RelayHandles.
	if _, _, subjectKind, ok := channelevents.ParseOutSubjectKind(msg.Subject); ok && !subjectKind.RelayHandles() {
		return
	}

	var env channelevents.Envelope
	if err := json.Unmarshal(msg.Data, &env); err != nil {
		logger.Info("outbound relay: drop, malformed envelope", "err", err.Error())
		return
	}
	if err := env.Validate(); err != nil {
		logger.Info("outbound relay: drop, envelope validation failed", "err", err.Error())
		return
	}
	if !env.Kind.Implemented() {
		logger.Info("outbound relay: drop, reserved kind not yet implemented", "kind", string(env.Kind))
		return
	}

	// The SUBJECT is the routing authority, not the envelope. This subscription
	// is the cluster-wide "ap.session.*.*.out.>", while a runner's per-session
	// NATS JWT permits publishing under exactly one "ap.session.<ns>.<own-name>.>"
	// tree — so the subject is the only session identity NATS actually
	// authorized, and env.Session is publisher-controlled JSON. Resolving the
	// AgentSession (and so the Channel, the thread key, and the Sender) from
	// env.Session would make that grant non-load-bearing: a publisher permitted
	// on session A's subject could set env.Session to B and have its message
	// delivered into B's thread. Same rule as historyresp's "never from
	// caller-supplied data". Legitimate publishers always agree — PublishOut /
	// PublishOutSeq build subject and envelope from one (ns, name) pair — so a
	// mismatch is a bug or an attack, never normal traffic.
	ns, name, subjectOK := channelevents.ParseOutSubject(msg.Subject)
	if !subjectOK {
		logger.Info("outbound relay: drop, unparseable out subject",
			"claimedSession", env.Session.Namespace+"/"+env.Session.Name, "kind", string(env.Kind))
		return
	}
	if ns != env.Session.Namespace || name != env.Session.Name {
		// Logged loudly with the publisher-locating trio (subject, claimed
		// session, kind) per AGENTS.md's no-silent-errors rule: a dropped
		// envelope with no log is precisely the failure that rule exists for.
		logger.Info("outbound relay: drop, envelope session does not match the authorized subject",
			"subjectSession", ns+"/"+name,
			"claimedSession", env.Session.Namespace+"/"+env.Session.Name,
			"kind", string(env.Kind))
		return
	}

	logger = logger.WithValues(
		"session", ns+"/"+name,
		"kind", string(env.Kind),
	)

	// The subject has just been proven to authorize env.Session, so this is the
	// first point where the asking session is KNOWN rather than claimed. Put
	// that checked value into the payload copy every renderer reads, before any
	// dispatch can hand the envelope to one. See stampAskingSession for why a
	// forged value there suppresses attribution rather than merely faking it.
	env = stampAskingSession(env, logger)

	// ToolActivity is a control-plane signal for the silence watchdog —
	// it has no user-visible representation, so we don't load the session
	// or call into a Sender. Fold it into the machine as a timing re-arm
	// (EvToolActivity never forwards). The subject-derived ns/name is
	// sufficient for the watchdog to identify which session is making progress.
	if env.Kind == channelevents.KindToolActivity {
		if r.ApplyStatusEvent != nil {
			r.ApplyStatusEvent(ctx, ns, name, watchdog.Event{
				Kind: watchdog.EvToolActivity, Seq: env.Seq, UID: env.SessionUID,
			})
		}
		return
	}

	// TurnActivity is a control-plane signal for the silence watchdog — no
	// user-visible representation, so no session load and no Sender. It folds
	// into the machine via OnTurnActivity, which owns the leakage-pause
	// carve-out (an invisible leakage wait must keep being announced, so it is
	// NOT folded as a machine yield).
	if env.Kind == channelevents.KindTurnActivity {
		if r.OnTurnActivity != nil {
			var pl channelevents.TurnActivityPayload
			if err := json.Unmarshal(env.Payload, &pl); err != nil {
				logger.Info("outbound relay: drop turn_activity, malformed payload", "err", err.Error())
				return
			}
			r.OnTurnActivity(ctx, ns, name, pl.Active, pl.Cause, env.Seq, env.SessionUID)
		}
		return
	}

	// WidgetOffer is webui-live-only; consumed by the session-view page's
	// live mirror, never delivered to a channel sender. No session load and
	// no Sender — mirrors the TurnActivity carve-out above.
	if env.Kind == channelevents.KindWidgetOffer {
		return
	}

	// UIActionUpdate is webui-live-only for the same reason: the agent-UI page's
	// own live mirror (pkg/web/webui/agentui's runActionMirror) subscribes to
	// this subject directly, and it has no channel-message representation.
	// Without the carve-out every lifecycle transition on a channel-attached
	// session costs an apiserver Get and then a sender.Send that errors
	// "unsupported envelope kind" — up to four per button click. A log line that
	// always fires and never means anything is how operators learn to ignore the
	// one line the no-silent-errors rule exists to make meaningful.
	if env.Kind == channelevents.KindUIActionUpdate {
		return
	}

	// Everything below this point costs at least one API-server round-trip, on
	// the single goroutine nats.go dispatches this subscription's callbacks on.
	// The two guards here are the ones that can answer from process-local state.

	// Consumer-supplied pre-Get scope: a subscriber that already knows it can
	// never deliver for this session says so without a lookup. nil ⇒ no-op.
	//
	// NOT consulted for a human-directed envelope. Accept answers from
	// process-local state about the sessions a consumer SERVES, and a card
	// about a delegated child is legitimately deliverable by a host that serves
	// an ANCESTOR — a session the predicate has never heard of. Asking it here
	// drops the card before the lineage walk that would have found the right
	// reader, which is how a held child ends up parked with nobody asked.
	//
	// The cost is one Get per human-directed envelope on consumers that set
	// this. That respects what the seam is for: its own doc says it exists to
	// spare a round-trip on the single serialized callback goroutine in front
	// of the hot path — stream deltas, one per token. Cards are rare, and the
	// ownership decision still happens, one step later, where the session and
	// its lineage can actually be read.
	if r.Accept != nil && !isHumanDirected(env.Kind) && !r.Accept(ns, name) {
		return
	}

	// Stream deltas the kind recently declined to render: dropped for the rest of
	// the decline window without re-loading the AgentSession and re-reading the
	// AgentClass gate once per token. The watchdog re-arm still happens — it is
	// in-memory and runs ahead of sink resolution, so a declined delta folds in
	// exactly what a rendered one would.
	if env.Kind == channelevents.KindAssistantStreamDelta &&
		r.streamDeltasDeclined(ns, name) {
		touchOnDelta(env.Payload, func() {
			if r.ApplyStatusEvent != nil {
				r.ApplyStatusEvent(ctx, ns, name, watchdog.Event{
					Kind: watchdog.EvToolActivity, Seq: env.Seq, UID: env.SessionUID,
				})
			}
		})
		return
	}

	var sess spiceboxv1alpha1.AgentSession
	if err := r.K8s.Get(ctx, client.ObjectKey{
		Namespace: ns, Name: name,
	}, &sess); err != nil {
		logger.Info("outbound relay: drop, AgentSession lookup failed", "err", err.Error())
		return
	}
	// Two routing rules, split by who has to read the envelope
	// (humanDirectedKinds, in humandirected.go, is the whole list).
	//
	// A human-directed envelope — an interaction card and its resolutions —
	// resolves through the lineage to the nearest binding a PERSON reads, which
	// is not always this session's own. A single_turn delegated child is
	// headless by construction (no spec.inputChannel) and a held session can
	// never gain one, so under forensic hold its release card has no binding at
	// all to start from.
	// A conversational subagent has the opposite problem: it IS bound, to an
	// `agent` Channel whose far side is its parent session, so its own binding
	// resolves to a channel that no human reads and delivering a permission
	// prompt there would put a human's decision in front of another agent. The
	// walk skips those and climbs; nothing human-readable anywhere is a loud
	// drop, never a fallback onto an agent surface.
	//
	// Everything else — the agent's replies, status, offers — routes to the
	// session's own binding, which for a conversational child IS the agent
	// Channel to its parent. It needs no walk: that binding is what talking to
	// your parent is for.
	//
	// A conversational child produces no REPLY on that path, and that is
	// deliberate rather than a gap in the routing: respond_to_user is withheld
	// from a session whose binding reaches another session, because a message
	// arriving that way lands in the other agent's transcript with no content
	// inspection anywhere (pkg/agent/tool/meta/capability's respondToUserSkip).
	// Its question and its final answer travel through the delegation instead,
	// as inspected tool results.
	ownBinding := spiceboxv1alpha1.OutboundBinding(&sess)
	outBinding := ownBinding
	// bindingOwner is the session outBinding was found on — sess itself for a
	// root, an ANCESTOR for a delegated child. It is what a client-hosted host
	// is asked about below, instead of the envelope's own session.
	bindingOwner := spiceboxv1alpha1.NamespacedRef{Namespace: sess.Namespace, Name: sess.Name}
	// bindingChain is the delegation path from this envelope's session up to
	// bindingOwner. Stamped onto the card below so the approver can see how the
	// asking session relates to the one they are watching.
	var bindingChain []spiceboxv1alpha1.NamespacedRef
	if isHumanDirected(env.Kind) {
		target, err := spiceboxv1alpha1.ResolveHumanDirectedBinding(ctx, r.K8s, &sess, registry.DeliversToHuman)
		if err != nil {
			logger.Info("outbound relay: drop, human-directed lineage walk failed", "err", err.Error())
			return
		}
		if target == nil {
			logger.Info("outbound relay: drop, no human-readable channel binding anywhere in the lineage; " +
				"a prompt whose answer is a person's decision has nobody to ask")
			return
		}
		outBinding = target.Binding
		bindingOwner = target.Owner
		bindingChain = target.Chain
	} else if outBinding == nil {
		logger.Info("outbound relay: drop, session not channel-attached")
		return
	}

	// Recover a channel_id the binding never got (or lost) from the destination
	// Channel CR before any send — the reviewbot incident, where a Slack output
	// binding with no channel_id made every reply fail "external.channel_id
	// missing" into monitoring silence. Scoped to the session's OWN dedicated
	// OutputChannel: that is where outputDefaults.channelId (the recovery source)
	// lives, and the scope keeps recovery off the hot inbound-binding paths —
	// notably per-token stream deltas on a plain InputChannel-bound session,
	// whose kind may still provide an anchor. Runs only when the binding lacks a
	// channel_id, so an established output pays a single map lookup. Mutating
	// this pointer also fixes openOutboundThread and the write-back, which read
	// the same sess.Spec.OutputChannel.
	if sess.Spec.OutputChannel != nil && outBinding == sess.Spec.OutputChannel {
		r.recoverBindingChannelID(ctx, logger, bindingOwner.Namespace, outBinding)
	}

	// A session whose inbound carried no human has no message of its own to
	// open its outbound thread with, so the root would be whatever this
	// envelope turns out to be. Post the trigger's own line first and take the
	// root from THAT send. Ahead of every dispatch below, the stream sink
	// included: the sink lazily posts a bubble that can become the root too.
	//
	// Costs nothing for every other session — a nil map lookup — because the
	// answer was decided once, at session creation, where the input Channel was
	// in hand (AnnotationSessionOpening).
	r.openOutboundThread(ctx, logger, &sess)

	// AssistantStreamDelta routes to the kind's StreamDeltaSink rather than
	// the standard Sender path. Sinks own their own debounce / throttle
	// policy; generic routing here just hands the envelope across.
	if env.Kind == channelevents.KindAssistantStreamDelta {
		// Tokens flowing in is forward progress — feed the silence
		// watchdog so a long mid-generation reply (no tool calls, no
		// update_status) doesn't trip the "agent appears stuck" warning.
		// Only true progress events (text_delta, tool_use_start) qualify;
		// terminal markers (tool_use_stop, stop) are end-of-block, not
		// progress, and must NOT mask post-stream silence.
		touchOnDelta(env.Payload, func() {
			if r.ApplyStatusEvent != nil {
				r.ApplyStatusEvent(ctx, ns, name, watchdog.Event{
					Kind: watchdog.EvToolActivity, Seq: env.Seq, UID: env.SessionUID,
				})
			}
		})
		sink, err := r.Senders.StreamDeltaSinkFor(ctx, &sess)
		if err != nil {
			logger.Info("outbound relay: drop, StreamDeltaSinkFor errored", "err", err.Error())
			return
		}
		if sink == nil {
			// Kind opts out of stream-delta rendering — expected, no log.
			// Remember the refusal for a bounded window so the rest of this
			// reply's tokens cost nothing. Only the refusal is remembered; see
			// declineStreamDeltas.
			r.declineStreamDeltas(ns, name)
			return
		}
		if err := sink.OnDelta(ctx, channelkinds.SessionInfo{
			Namespace: sess.Namespace, Name: sess.Name, Channel: outBinding, Annotations: sess.Annotations,
		}, env); err != nil {
			logger.Info("outbound relay: stream sink OnDelta errored", "err", err.Error())
		}
		return
	}

	// Caption gate: setStatus-bearing notifications (non-empty text) and
	// plan_update snapshots are ordered captions. Folding them into the machine
	// drops a stale (lower-Seq) or post-yield caption instead of flickering the
	// indicator back on, and re-arms the silence timer from the same ordered
	// place. The empty-Notification "clear my indicator" sentinel is NOT a
	// caption and must always reach the Sender's clear path, so it bypasses the
	// gate.
	if r.ApplyStatusEvent != nil && isStatusCaptionKind(env) {
		res := r.ApplyStatusEvent(ctx, ns, name, watchdog.Event{
			Kind: watchdog.EvCaption, IsCaption: true, Seq: env.Seq, UID: env.SessionUID, Text: captionTextOf(env),
		})
		if !res.Forward {
			logger.Info("outbound relay: status caption suppressed (stale or post-yield)", "kind", string(env.Kind))
			return
		}
	}

	// Turn-progress gate: KindTurnProgress is the runner's render-only spinner
	// tick (cumulative tokens / elapsed). Fold it into the same machine as an
	// EvTurnProgress so a yielded session (paused awaiting a user reply) drops
	// the update instead of animating progress for a turn that has stopped. It
	// uses its own event kind — turn-progress carries an independent per-turn
	// counter and must not participate in the caption stale-drop ordering.
	if r.ApplyStatusEvent != nil && env.Kind == channelevents.KindTurnProgress {
		res := r.ApplyStatusEvent(ctx, ns, name, watchdog.Event{
			Kind: watchdog.EvTurnProgress, Seq: env.Seq, UID: env.SessionUID,
		})
		if !res.Forward {
			logger.Info("outbound relay: turn_progress suppressed (session yielded)")
			return
		}
	}

	// Tool-progress gate: KindToolProgress is the sandbox tool's per-tool
	// liveness/progress tick while a SYNC tool runs. Fold it into the same
	// machine as an EvToolProgress so a yielded session drops the update, and
	// so a running tool re-arms the silence timer. Own event kind — no caption
	// stale-drop ordering. Forwarded tool_progress falls through to the main
	// sender (default case) for rendering.
	if r.ApplyStatusEvent != nil && env.Kind == channelevents.KindToolProgress {
		res := r.ApplyStatusEvent(ctx, ns, name, watchdog.Event{
			Kind: watchdog.EvToolProgress, Seq: env.Seq, UID: env.SessionUID,
		})
		if !res.Forward {
			logger.Info("outbound relay: tool_progress suppressed (session yielded)")
			return
		}
	}

	// Operation-activity gate: KindOperationActivity is the runner's live
	// operation-subtree snapshot. Fold it into the machine (yield-suppress,
	// re-arm the silence timer, resolve the effective compact line). On forward,
	// overwrite the payload's CompactLine with the machine's EffectiveLine (which
	// reverts to the remembered caption on a cleared tick) so every sender that
	// shows one line renders the resolved value. Own event kind — no caption
	// stale-drop ordering. Falls through to the sender (default case) to render.
	if r.ApplyStatusEvent != nil && env.Kind == channelevents.KindOperationActivity {
		var pl channelevents.OperationActivityPayload
		if err := json.Unmarshal(env.Payload, &pl); err != nil {
			logger.Info("outbound relay: drop operation_activity, malformed payload", "err", err.Error())
			return
		}
		res := r.ApplyStatusEvent(ctx, ns, name, watchdog.Event{
			Kind: watchdog.EvOperationActivity, Seq: env.Seq, UID: env.SessionUID, Text: pl.CompactLine,
		})
		if !res.Forward {
			logger.Info("outbound relay: operation_activity suppressed (session yielded)")
			return
		}
		// Reflect the machine-resolved effective line back onto the envelope so
		// the sender renders the resolved (possibly reverted) compact line.
		pl.CompactLine = res.EffectiveLine
		reb, err := json.Marshal(pl)
		if err != nil {
			logger.Info("outbound relay: drop operation_activity, re-marshal failed", "err", err.Error())
			return
		}
		env.Payload = reb
	}

	// Durably record relayed prompt REQUEST envelopes so the inbound pipeline can
	// re-surface them if the user re-interacts before the prompt is resolved.
	// No-op when Mem is nil, and for every other kind dispatched below.
	notePendingPrompt(ctx, r.Mem, sess.Namespace, sess.Name, env)

	var (
		sender channelkinds.Sender
		err    error
	)
	switch env.Kind {
	case channelevents.KindToolSessionDelta, channelevents.KindToolSessionEvent:
		sender, err = r.Senders.SubChannelSenderFor(ctx, &sess, "tool_session")
	case channelevents.KindLiveViewOffer:
		sender, err = r.Senders.SubChannelSenderFor(ctx, &sess, "live_view_offer")
	case channelevents.KindSessionViewOffer:
		sender, err = r.Senders.SubChannelSenderFor(ctx, &sess, "session_view_offer")
	case channelevents.KindAgentUIOffer:
		// The agent's own decision to hand this user the agent-defined UI
		// (show_agent_ui), as opposed to the session_view_offer anchor above,
		// which the runner publishes automatically for an mcp-ui widget.
		// Different page, different publisher, same "here is a link" shape.
		sender, err = r.Senders.SubChannelSenderFor(ctx, &sess, channelkinds.SubChannelAgentUIOffer)
	case channelevents.KindThreadTitle:
		// Agent-set conversation title. Renders as native setTitle (DM) or an
		// in-place edit of the "started" message (bot-rooted channel thread).
		sender, err = r.Senders.SubChannelSenderFor(ctx, &sess, "thread_title")
	case channelevents.KindInterruptApplied, channelevents.KindEnqueueAck:
		// Runner's answer to a channel-side mid-turn interrupt request, or
		// channelsd's proactive queued-message ack; both render on the
		// queued_messages sub-channel as a timeline note. The request side
		// (KindInterruptRequest) flows IN to the runner, not through this relay.
		//
		// Slack's "queued_messages" sub-channel sender returns nil on purpose
		// (pkg/channels/channelkinds/slack/kind.go): its enqueue ack rides the
		// generic interaction sender, and KindInterruptApplied's response_url edit
		// belongs to internal/cmd/channelsd/interrupt_applied_bridge.go. The nil-sender
		// drop below is therefore expected for Slack — the same graceful degrade
		// as the KindUserEcho case.
		sender, err = r.Senders.SubChannelSenderFor(ctx, &sess, "queued_messages")
	case channelevents.KindUserEcho:
		// Mirrors a view-originated user message back into the session's origin
		// channel. Kinds that ARE the view surface (builtin/local/bento) don't
		// implement this sub-channel; their nil sender degrades gracefully below —
		// no log-worthy failure, there is simply nothing to mirror to.
		sender, err = r.Senders.SubChannelSenderFor(ctx, &sess, "user_echo")
	case channelevents.KindInteractionRequest, channelevents.KindInteractionApplied,
		channelevents.KindInteractionDecisionRejected:
		// KindInteractionDecisionRejected flows OUT per-clicker after the decision
		// pipe refuses a click (lacked standing, spectator on an already-resolved
		// prompt, category mismatch, or a bound-handler failure). The interaction
		// sub-channel sender renders it to the clicker; the shared prompt is left
		// intact, so a rejected click never resolves the card.
		//
		// The resolver (senderResolver.SubChannelSenderFor, and the ForSession
		// lookup beneath it) independently re-derives which Channel to build a
		// Sender against from sess.Spec.InputChannel/OutputChannel — it does not
		// see outBinding. Handing it the unmutated session would build the wrong
		// Sender for both shapes the human-directed walk exists to serve: a
		// headless delegated child has both fields nil and would be refused
		// outright, and a conversational child has an `agent` InputChannel and
		// would get an agent Sender for a card the walk just routed to a human's
		// Slack. lookupSess is a shallow copy whose binding fields are collapsed
		// onto the already-resolved outBinding — OutputChannel cleared so it
		// cannot win the resolver's precedence — so the resolver builds against
		// the same Channel this envelope is about to be routed through. sess
		// itself (used for the write-back below and for sessInfo) is untouched
		// by this copy.
		lookupSess := sess
		lookupSess.Spec.InputChannel = outBinding
		lookupSess.Spec.OutputChannel = nil
		// And the IDENTITY of the session that owns that binding, not the
		// envelope's own. A client-hosted host (the `local` TUI, webd's chat
		// registry) decides what it may render by asking whether it serves the
		// session it was handed. Handed a delegated child's name it refuses a
		// card it is the right reader for — the person it serves owns that
		// whole tree — and the card is dropped with nobody asked, leaving the
		// child parked until its approval times out. The resolved ancestor is
		// a session the host recognizes, so its existing ownership check
		// answers correctly with no lineage walk of its own.
		//
		// This cannot widen delivery: outBinding came from
		// ResolveHumanDirectedBinding, whose walk only ever records a binding
		// whose kind DeliversToHuman, so the surface reached is a person's
		// either way.
		lookupSess.Namespace = bindingOwner.Namespace
		lookupSess.Name = bindingOwner.Name

		sender, err = r.Senders.SubChannelSenderFor(ctx, &lookupSess, "interaction")
		// The same correction, one field further in. The resolver above is not
		// the only thing that would otherwise act on the session's own binding:
		// the card's audience identities carry a channel-kind tag its publisher
		// derived from that binding, and the surface skips any recipient whose
		// tag is not its own. Re-stamp them onto the binding this envelope is
		// actually going out through. See restampAudienceKinds.
		env = restampAudienceKinds(env, outBinding.Kind, registry.DeliversToHuman, logger)
		// And the path this card travelled to reach its reader. Here rather
		// than beside stampAskingSession because the chain does not exist until
		// the lineage walk above has run.
		env = stampAskingChain(env, bindingChain, logger)
	default:
		sender, err = r.Senders.SenderFor(ctx, &sess)
	}
	if err != nil {
		logger.Info("outbound relay: drop, sender resolution errored", "err", err.Error())
		return
	}
	if sender == nil {
		logger.Info("outbound relay: drop, no sender for this kind on this channel")
		return
	}
	sessInfo := channelkinds.SessionInfo{
		Namespace: sess.Namespace, Name: sess.Name, Channel: outBinding, Annotations: sess.Annotations,
		// SessionInitiator is the BARE canonical id of the session starter; the
		// annotation stores the "user:"-prefixed form, which StartedByCanonical
		// strips. Out-of-band applied senders (slack's credential_linked notice)
		// carry no recipient in the payload and resolve it from here — without it
		// they silently skip delivery.
		SessionInitiator: spiceboxv1alpha1.StartedByCanonical(&sess),
	}
	res, err := sender.Send(ctx, sessInfo, env)
	// Every real send outcome — success or failure, own binding or a
	// human-directed ancestor's — is folded into that Channel's Deliverable
	// condition, so the monitoring watcher sees a channel that swallows
	// deliveries instead of only channelsd's logs seeing it.
	r.noteDeliveryOutcome(ctx, bindingOwner.Namespace, outBinding, err)
	if err != nil {
		logger.Info("outbound relay: sender.Send errored", "err", err.Error())
		// A failed user-reply delivery must be SURFACED, not just logged: the
		// user is waiting for this reply and would otherwise see nothing (the
		// runner publishes turn_activity=paused on yield, which CLEARS the
		// silence watchdog, so the stall path won't fire either). Common
		// respond_to_user failures are envelope-specific (over-long text, a bad
		// attachment, a malformed block) while the channel itself is healthy, so
		// a degraded plain-text notice on the SAME sender does reach the user.
		if env.Kind == channelevents.KindUserMessage {
			r.surfaceDeliveryFailure(ctx, logger, sender, bindingOwner.Namespace, sessInfo, env)
		}
		return
	}
	// Write-back: when a Sender hands back routing metadata captured by the send
	// (slack's first chat.postMessage returns the new thread root's ts when none
	// was supplied), patch it onto the session's channel binding so subsequent
	// sends thread under the same root AND inbound thread replies match back to
	// this session. OutputChannel-bound (cron-spawned) sessions go through
	// patchOutputChannel; an InputChannel-only session carrying a
	// forked-from-thread annotation goes through patchInputChannelThreadTS, so
	// the inbound pipeline's primary lookup finds the child next time.
	//
	// ONLY when this envelope went out through the session's own binding. A
	// human-directed card is delivered through an ANCESTOR's channel, so the
	// thread root it captures is a thread in someone else's channel — patching
	// it onto this session's binding would point the inbound matcher at a
	// conversation this session does not live in. Identity is exact: the walk
	// returns sess's own binding pointer when it stops at sess, and every
	// non-human-directed envelope never leaves ownBinding. If that ever stopped
	// holding, the guard would skip a write-back it should have made — costing
	// threading on a first send, not misrouting one.
	if outBinding == ownBinding {
		if sess.Spec.OutputChannel != nil {
			if err := r.patchOutputChannel(ctx, &sess, res.External); err != nil {
				logger.Info("outbound relay: patch OutputChannel from SendResult failed", "err", err.Error())
			}
		} else if _, hasForkedFrom := sess.Annotations[spiceboxv1alpha1.AnnotationForkedFromThread]; hasForkedFrom {
			if err := r.patchInputChannelThreadTS(ctx, &sess, res.External); err != nil {
				logger.Info("outbound relay: patch InputChannel thread_ts from SendResult failed", "err", err.Error())
			}
		}
	}
}

// deliveryFailureNotice is the degraded plain-text reply posted when the
// agent's user-facing message could not be delivered. Kept short and
// attachment-free so it survives the failure modes that sink the original
// (over-long text, a bad attachment) and reaches the waiting user.
const deliveryFailureNotice = "I generated a reply but couldn't deliver it to this channel. " +
	"Please try your request again; if this keeps happening, check the channelsd logs."

// surfaceDeliveryFailure attempts a best-effort plain-text fallback notice on
// the SAME sender after the original user-reply Send failed, so the waiting
// user learns the reply was lost instead of seeing unexplained silence. If the
// fallback itself fails, the channel is genuinely unreachable — we log it and
// let the silence watchdog be the last line of defense (never the first).
//
// The fallback's outcome is folded into the Deliverable condition too
// (channelNS names the Channel CR's namespace, which for a user_message is
// always the session's own binding): a fallback that LANDS proves the channel
// works and the original failure was envelope-specific — over-long text, a
// bad attachment — so the condition converges back to True instead of
// flagging a healthy channel as an incident.
func (r *Relay) surfaceDeliveryFailure(
	ctx context.Context,
	logger logr.Logger,
	sender channelkinds.Sender,
	channelNS string,
	sessInfo channelkinds.SessionInfo,
	orig channelevents.Envelope,
) {
	notice, err := channelevents.BuildEnvelope(
		orig.Session.Namespace, orig.Session.Name,
		channelevents.KindUserMessage,
		channelevents.OutboundUserMessagePayload{Text: deliveryFailureNotice},
	)
	if err != nil {
		logger.Info("outbound relay: build delivery-failure notice failed", "err", err.Error())
		return
	}
	_, err = sender.Send(ctx, sessInfo, notice)
	r.noteDeliveryOutcome(ctx, channelNS, sessInfo.Channel, err)
	if err != nil {
		logger.Info("outbound relay: delivery-failure notice also failed to send; channel unreachable",
			"err", err.Error())
	}
}

// openOutboundThread posts the session's opening line — what triggered it — and
// takes the outbound thread's root from that send, so the first thing anyone
// reads in the thread says what the thread is about.
//
// Only a session created with AnnotationSessionOpening has one, and only an
// inbound that carried no human gets one: a webhook payload or a cron tick,
// where there is no message of a person's to be the root. A human-initiated
// session returns on the first line and its thread stays rooted by the person's
// own message, unchanged.
//
// Best-effort by design. The opening is a framing message, never a gate: every
// failure path logs and returns, leaving the agent's own output to go out and
// root the thread — which is the behaviour every session has today, not a new
// failure.
//
// The opening text is never cleared — on a send failure so the next envelope
// retries, and on success so it survives for channelsd to re-render the
// pinned status from later. What stops a second post once one has landed is
// thread_ts when the kind's Sender reports one back (today: slack), and
// AnnotationSessionOpeningSent otherwise — see its doc comment for why the
// text's mere presence can no longer be that signal.
func (r *Relay) openOutboundThread(ctx context.Context, logger logr.Logger, sess *spiceboxv1alpha1.AgentSession) {
	opening := sess.Annotations[spiceboxv1alpha1.AnnotationSessionOpening]
	if opening == "" {
		return
	}
	// No dedicated destination to open, one already rooted on a thread, or
	// already posted by a kind that never reports a root back: the anchor
	// question is settled either way.
	if sess.Spec.OutputChannel == nil ||
		sess.Spec.OutputChannel.External["thread_ts"] != "" ||
		sess.Annotations[spiceboxv1alpha1.AnnotationSessionOpeningSent] != "" {
		return
	}
	sessRef := sess.Namespace + "/" + sess.Name
	sender, err := r.Senders.SenderFor(ctx, sess)
	if err != nil {
		logger.Info("outbound relay: session opening skipped, sender resolution errored",
			"session", sessRef, "err", err.Error())
		return
	}
	if sender == nil {
		logger.Info("outbound relay: session opening skipped, no sender on this session's output channel",
			"session", sessRef)
		return
	}
	env, err := channelevents.BuildEnvelope(sess.Namespace, sess.Name,
		channelevents.KindUserMessage,
		channelevents.OutboundUserMessagePayload{Text: opening})
	if err != nil {
		logger.Info("outbound relay: session opening skipped, envelope build failed",
			"session", sessRef, "err", err.Error())
		return
	}
	res, err := sender.Send(ctx, channelkinds.SessionInfo{
		Namespace:        sess.Namespace,
		Name:             sess.Name,
		Channel:          sess.Spec.OutputChannel,
		Annotations:      sess.Annotations,
		SessionInitiator: spiceboxv1alpha1.StartedByCanonical(sess),
	}, env)
	r.noteDeliveryOutcome(ctx, sess.Namespace, sess.Spec.OutputChannel, err)
	if err != nil {
		logger.Info("outbound relay: session opening send failed; the agent's first output will root the thread",
			"session", sessRef, "err", err.Error())
		return
	}
	// Mark it posted regardless of what the Sender reported back. A kind that
	// also returns a thread_ts (slack) gets that as the primary "already open"
	// signal via patchOutputChannel below; a kind that does not would otherwise
	// have no record that this already happened, and would re-post the line
	// ahead of every subsequent envelope for the session's life.
	if err := r.markSessionOpeningSent(ctx, sess); err != nil {
		logger.Info("outbound relay: marking the session opening as sent failed; it may be posted again",
			"session", sessRef, "err", err.Error())
	}
	// Anchor on the root this send created, so the envelope the caller is about
	// to dispatch — and every later turn, and every inbound reply in the thread
	// — lands inside it rather than beside it.
	if err := r.patchOutputChannel(ctx, sess, res.External); err != nil {
		logger.Info("outbound relay: anchoring on the session opening's thread root failed",
			"session", sessRef, "err", err.Error())
	}
}

// markSessionOpeningSent stamps AnnotationSessionOpeningSent so a kind whose
// Sender reports no thread routing metadata back does not re-post the opening
// line ahead of every subsequent envelope — see its doc comment. A MergePatch
// for the same reason patchOutputChannel uses one: a concurrent reconciler
// edit to the session must merge rather than 409. The in-memory copy is
// updated to match so the caller's remaining work sees the same object the
// server holds.
func (r *Relay) markSessionOpeningSent(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) error {
	patchBytes, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]any{
				spiceboxv1alpha1.AnnotationSessionOpeningSent: "true",
			},
		},
	})
	if err != nil {
		return fmt.Errorf("marshal session-opening-sent patch: %w", err)
	}
	if err := r.K8s.Patch(ctx,
		&spiceboxv1alpha1.AgentSession{
			ObjectMeta: metav1.ObjectMeta{Name: sess.Name, Namespace: sess.Namespace},
		},
		client.RawPatch(types.MergePatchType, patchBytes),
	); err != nil {
		return fmt.Errorf("patch session.metadata.annotations: %w", err)
	}
	if sess.Annotations == nil {
		sess.Annotations = map[string]string{}
	}
	sess.Annotations[spiceboxv1alpha1.AnnotationSessionOpeningSent] = "true"
	return nil
}

// patchOutputChannel applies a JSON-merge-patch to session.spec.outputChannel
// with the routing metadata captured by the Sender on first send. A MergePatch
// is used (rather than client.Update) so concurrent reconciler edits don't
// produce 409 conflicts — controllers reading the session in flight will see
// the merged result on their next watch event. Returns nil when no patch is
// needed (no metadata, or session has no OutputChannel binding).
func (r *Relay) patchOutputChannel(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, captured map[string]string) error {
	if len(captured) == 0 || sess.Spec.OutputChannel == nil {
		return nil
	}
	threadTS := captured["thread_ts"]
	if threadTS == "" {
		// No thread_ts captured ⇒ nothing for the inbound matcher to key
		// on; captured fields without one are unused, so skip the patch.
		return nil
	}
	channelID := captured["channel_id"]
	if channelID == "" {
		// Fall back to the binding's existing channel_id — Sender may
		// have only newly-captured the thread_ts.
		channelID = sess.Spec.OutputChannel.External["channel_id"]
	}
	// Skip when the binding already records this exact thread root —
	// re-patching is harmless idempotently but burns api-server quota
	// on every reply in an established thread. (Senders should only
	// return write-back metadata on first send, but defense in depth.)
	if sess.Spec.OutputChannel.External["thread_ts"] == threadTS {
		return nil
	}
	key := fmt.Sprintf("thread:%s:%s", channelID, threadTS)
	// Patch BOTH spec.outputChannel.{key,external} AND
	// metadata.labels[LabelOutputChannelKey] in a single MergePatch so
	// the index label is in sync with the spec atomically — there is no
	// window where the slack listener could observe an outputChannel
	// key without a matching label (which would silently drop a thread
	// reply that arrived between the two patches).
	patchBody := map[string]any{
		"metadata": map[string]any{
			"labels": map[string]string{
				spiceboxv1alpha1.LabelOutputChannelKey: channelkey.LabelValue(key),
			},
		},
		"spec": map[string]any{
			"outputChannel": map[string]any{
				"key": key,
				"external": map[string]string{
					"channel_id": channelID,
					"thread_ts":  threadTS,
				},
			},
		},
	}
	patchBytes, err := json.Marshal(patchBody)
	if err != nil {
		return fmt.Errorf("marshal outputChannel patch: %w", err)
	}
	if err := r.K8s.Patch(ctx,
		&spiceboxv1alpha1.AgentSession{
			ObjectMeta: metav1.ObjectMeta{Name: sess.Name, Namespace: sess.Namespace},
		},
		client.RawPatch(types.MergePatchType, patchBytes),
	); err != nil {
		return fmt.Errorf("patch session.spec.outputChannel: %w", err)
	}

	// Reflect the patch onto the in-memory session. A caller still holding it
	// must route into the thread the send it just made created, or it opens a
	// second thread alongside the one it was told about — which is exactly what
	// happens after openOutboundThread, whose own caller hands the pending
	// envelope to the Sender next. Merged rather than replaced, matching what
	// the merge-patch above did on the server.
	mergedExternal := maps.Clone(sess.Spec.OutputChannel.External)
	if mergedExternal == nil {
		mergedExternal = map[string]string{}
	}
	mergedExternal["channel_id"] = channelID
	mergedExternal["thread_ts"] = threadTS
	sess.Spec.OutputChannel.External = mergedExternal
	sess.Spec.OutputChannel.Key = key

	// Patch succeeded — publish a control-plane SessionAttached event so any
	// listener that maintains per-session in-process state (today: the
	// slack listener's threadIndex) learns the new anchor in real time
	// instead of waiting for the next channelsd restart's startup walk.
	// Publish failure is best-effort: the patch is durable, and the
	// listener's startup walk catches whatever the NATS hop missed.
	ev := channelevents.SessionAttached{
		Namespace:         sess.Namespace,
		SessionUID:        string(sess.UID),
		SessionName:       sess.Name,
		OutputChannelName: sess.Spec.OutputChannel.Name,
		OutputChannelKind: sess.Spec.OutputChannel.Kind,
		OutputChannelKey:  key,
		External: maps.Clone(map[string]string{
			"channel_id": channelID,
			"thread_ts":  threadTS,
		}),
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		log.FromContext(ctx).Info("session_attached: marshal failed",
			"session", sess.Namespace+"/"+sess.Name, "err", err.Error())
		return nil
	}
	if err := r.NC.Publish(channelevents.SessionAttachedSubject, payload); err != nil {
		log.FromContext(ctx).Info("session_attached: NATS publish failed",
			"session", sess.Namespace+"/"+sess.Name, "err", err.Error())
		// Same fall-through: startup walk catches it later.
	}
	return nil
}

// patchInputChannelThreadTS stamps the thread_ts captured on first send onto
// spec.inputChannel.external and sets LabelChannelKey, so the inbound pipeline's
// primary lookup finds this session on the next inbound. Used for restart-fork
// children, which have no OutputChannel binding and are created with thread_ts
// cleared from InputChannel.External so the Slack sender posts at channel level.
// The InputChannel analogue of patchOutputChannel, MergePatch and all. Returns
// nil when no patch is needed.
func (r *Relay) patchInputChannelThreadTS(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, captured map[string]string) error {
	if len(captured) == 0 || sess.Spec.InputChannel == nil {
		return nil
	}
	threadTS := captured["thread_ts"]
	if threadTS == "" {
		return nil
	}
	channelID := captured["channel_id"]
	if channelID == "" {
		channelID = sess.Spec.InputChannel.External["channel_id"]
	}
	// Skip when the binding already records this exact thread root —
	// idempotent guard matching patchOutputChannel's pattern.
	if sess.Spec.InputChannel.External["thread_ts"] == threadTS {
		return nil
	}
	key := fmt.Sprintf("thread:%s:%s", channelID, threadTS)
	// Patch spec.inputChannel.external AND LabelChannelKey atomically so
	// the inbound pipeline's label lookup sees the new thread root without
	// a window where the key is set but the label isn't (or vice versa).
	patchBody := map[string]any{
		"metadata": map[string]any{
			"labels": map[string]string{
				spiceboxv1alpha1.LabelChannelKey: channelkey.LabelValue(key),
			},
		},
		"spec": map[string]any{
			"inputChannel": map[string]any{
				"key": key,
				"external": map[string]string{
					"channel_id": channelID,
					"thread_ts":  threadTS,
				},
			},
		},
	}
	patchBytes, err := json.Marshal(patchBody)
	if err != nil {
		return fmt.Errorf("marshal inputChannel patch: %w", err)
	}
	if err := r.K8s.Patch(ctx,
		&spiceboxv1alpha1.AgentSession{
			ObjectMeta: metav1.ObjectMeta{Name: sess.Name, Namespace: sess.Namespace},
		},
		client.RawPatch(types.MergePatchType, patchBytes),
	); err != nil {
		return fmt.Errorf("patch session.spec.inputChannel: %w", err)
	}
	log.FromContext(ctx).Info("outbound relay: patched inputChannel thread_ts for restart-fork child",
		"session", sess.Namespace+"/"+sess.Name, "threadTS", threadTS, "channelID", channelID)
	return nil
}

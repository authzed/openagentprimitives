// Package admind is the operator-mounted admin API: live session
// aggregation (CRD watch + NATS overlay), cross-session audit queries,
// and session kill. It is deliberately self-contained (constructor-
// injected deps only) so it can split into its own binary later.
package admind

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
)

// SessionState is the live view of one AgentSession: the CRD snapshot
// (authoritative) overlaid with sub-turn NATS signals (fresher).
type SessionState struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Class     string `json:"class,omitempty"`
	// Model is the uniform <provider>/<model> display id resolved from
	// status.effectiveSettings.model (composed via modelDisplay: bare Name when
	// Provider is empty); session-constant.
	Model       string `json:"model,omitempty"`
	ChannelKind string `json:"channelKind,omitempty"`
	// ChannelName is the input Channel CR's name (spec.inputChannel.name), so
	// the sessions table can show the concrete channel, not just its kind.
	ChannelName string `json:"channelName,omitempty"`
	// Phase is the AgentSession CRD's own lifecycle phase, not the live
	// working/idle overlay Active carries.
	Phase string `json:"phase,omitempty"`
	// StartedAt is when the session began; nil until the CRD reports it.
	StartedAt *time.Time `json:"startedAt,omitempty"`
	// StartedBy is the creating user's canonical id (the started-by-canonical-id
	// annotation channelsd stamps at session creation); empty for kubectl-driven
	// sessions that carry no annotation.
	StartedBy string `json:"startedBy,omitempty"`
	// TurnCount is the session's turn total, from the CRD's turn-end snapshot.
	TurnCount int32 `json:"turnCount"`
	// InputTokens/OutputTokens are the session's token usage, taken from
	// whichever is further ahead: the CRD's turn-end snapshot or the live
	// in-flight turn count.
	InputTokens  int64 `json:"inputTokens"`
	OutputTokens int64 `json:"outputTokens"`
	// ByModel is the session's per-served-model token/cost breakdown
	// (status.estimatedCost.byModel), each bucket's Model already the uniform
	// <provider>/<model> display id. EMPTY for a session that has not ended
	// yet, in which case the Overview/Budget aggregators attribute this
	// session's InputTokens/OutputTokens/Model as a single blended row.
	ByModel []spiceboxv1alpha1.ModelCostBucket `json:"byModel,omitempty"`
	// ByTool is the session's per-interactive-toolkit cost breakdown
	// (status.estimatedCost.byTool). Added to the token-repriced estimate in the
	// budget/overview panels so admind totals include inner sub-agent spend.
	ByTool []spiceboxv1alpha1.ToolCostBucket `json:"byTool,omitempty"`
	// BundleSessions mirrors AgentSession.status.bundleSessions — one entry per
	// tool bundle this session resolved to a SpiceboxSession, carried straight
	// through from the watched CRD (no new fetch). A bundle's sandbox backend
	// (kind/ref/prewarmed/phase) is NOT here: that lives only on the bundle's
	// own SpiceboxSession.status.sandbox, so handleSessionDetail fetches it
	// live, per bundle, at request time (see BundleSandbox). Empty for a
	// session with no resolved bundles.
	// json:"-" deliberately: this is a SERVER-SIDE input, not a payload. Its
	// one consumer is handleSessionDetail, which turns it into the enriched
	// Bundles list the UI actually renders. SessionState is broadcast verbatim
	// on the SSE stream to every connected admin console on every session
	// update, so shipping the raw bundle list there would put bytes nothing
	// reads on the hot path.
	BundleSessions []spiceboxv1alpha1.ResolvedBundle `json:"-"`
	// ToolCallCount is the session's tool-call total, from the CRD's turn-end
	// snapshot.
	ToolCallCount int32 `json:"toolCallCount"`
	// ElapsedSeconds is the IN-FLIGHT turn's elapsed wall time, not the
	// session's age.
	ElapsedSeconds int `json:"elapsedSeconds"`
	// Active is the live working/idle dot: whether the session is doing work
	// right now, per the turn-activity overlay over the CRD phase.
	Active bool `json:"active"`
	// ActivityCause is why the session is in that state — the publisher's
	// cause on the turn-activity signal, or the CRD phase's when it overrides.
	ActivityCause string `json:"activityCause,omitempty"`
	// StatusText is the latest agent-published notification line.
	StatusText string `json:"statusText,omitempty"`
	// Plan is the session's current plan state, forwarded verbatim.
	Plan json.RawMessage `json:"plan,omitempty"` // raw PlanUpdatePayload
	// PendingToolGrants and PendingLeakageApprovals count approvals of those
	// two families awaiting a decision.
	PendingToolGrants       int `json:"pendingToolGrants"`
	PendingLeakageApprovals int `json:"pendingLeakageApprovals"`
	// PendingContentInspectionApprovals counts content-inspection approvals
	// awaiting decision (the third approval kind alongside the two above).
	PendingContentInspectionApprovals int `json:"pendingContentInspectionApprovals"`
	// LastEventAt is when this entry was last folded, from either source.
	LastEventAt time.Time `json:"lastEventAt"`
}

// Approval is one pending approval folded from a session's generic
// PendingInteractions list, surfaced in the cluster-wide read-only Approvals
// queue. Kind ∈ {"tool_call", "leakage", "content_inspection"} (the admind
// label for the interaction Category). Tool carries the publisher's one-line
// interaction Summary; Requester is unset for every family (the generic
// interaction list carries no requester) — both are omitted when absent.
type Approval struct {
	Kind        string    `json:"kind"`                 // which approval family is waiting
	Tool        string    `json:"tool,omitempty"`       // the publisher's one-line summary of what is being asked
	Namespace   string    `json:"namespace"`            // waiting session's namespace
	Name        string    `json:"name"`                 // waiting session's name
	Class       string    `json:"agentClass,omitempty"` // the session's AgentClass; "" when unresolved
	Requester   string    `json:"requester,omitempty"`  // who asked; unset for every family today
	RequestedAt time.Time `json:"requestedAt"`          // when the session started waiting
}

// RecentEvent is one ring-buffer item for the session detail view.
type RecentEvent struct {
	At      time.Time       `json:"at"`                // when the envelope was folded
	Kind    string          `json:"kind"`              // the channelevents kind that carried it
	Payload json.RawMessage `json:"payload,omitempty"` // the envelope's payload, verbatim
}

// SSEFrame is one server-sent event: Event names the type ("session" =
// SessionState upsert, "remove" = {namespace,name}), Data is its JSON.
type SSEFrame struct {
	Event string
	Data  []byte
}

// RingSize is the maximum number of recent events retained per session.
const RingSize = 100

// ToolCallRingSize bounds the cluster-wide tool-call stream retained for the
// Live tool-calls view.
const ToolCallRingSize = 200

// ToolCall is one entry in the global tool-activity stream: which session
// dispatched which tool, and when. Fed from KindToolActivity envelopes.
type ToolCall struct {
	At        time.Time `json:"t"`                    // when the dispatch was observed
	Namespace string    `json:"namespace"`            // dispatching session's namespace
	Name      string    `json:"name"`                 // dispatching session's name
	Class     string    `json:"agentClass,omitempty"` // the session's AgentClass; "" when unresolved
	Tool      string    `json:"tool"`                 // the tool the agent called
}

type sessionEntry struct {
	state     SessionState
	recent    []RecentEvent // newest last, capped at RingSize
	approvals []Approval    // session's pending approvals, replaced each UpsertSession
	// createdAt is when this entry first appeared, used only to age out
	// provisional entries (see confirmed).
	createdAt time.Time
	// confirmed is true once an AgentSession from the informer has been folded
	// into this entry. An entry created from a NATS envelope alone is
	// PROVISIONAL: the CRD watch legitimately lags NATS by a beat, but it can
	// also never arrive at all — the NATS subscription is cluster-wide while
	// the informer is scoped by --watch-namespaces — and nothing else would
	// ever remove such an entry. Provisional entries are pruned after stubTTL.
	confirmed bool
	// phaseInactive / phaseCause are the CRD's own verdict on whether this
	// session is doing work, recomputed each UpsertSession (see phaseActivity).
	// Held on the entry so a late turn_activity cannot re-arm the working dot
	// for a session the CRD has already parked or finished.
	phaseInactive bool
	phaseCause    string
	// sawTurnActivity is true once a real turn_activity has been folded. Until
	// then the CRD phase seeds Active; afterwards the more-responsive overlay
	// owns it (a lagging phase must never flip a genuinely-working session).
	sawTurnActivity bool
}

// tombstone records that a session's AgentSession was observed deleted, so an
// envelope from its runner — which keeps publishing through its termination
// grace period, long after the finalizer has dropped — cannot lazily re-create
// the entry no deleter will ever visit again.
type tombstone struct {
	at time.Time
	// loggedDrop keeps the "dropped a post-delete envelope" log to one line per
	// deleted session rather than one per envelope.
	loggedDrop bool
}

const (
	// stubTTL bounds how long an entry created from NATS alone may live
	// without the informer confirming it. Orders of magnitude beyond any
	// watch-lags-NATS window, so the legitimate case is unaffected.
	stubTTL = 2 * time.Minute
	// tombstoneTTL bounds how long a deleted session suppresses envelope-only
	// re-creation. Well past any pod termination grace period; a re-created
	// AgentSession with the same name clears its tombstone immediately.
	tombstoneTTL = 10 * time.Minute
	// sweepInterval rate-limits the prune so it costs one clock read on the
	// overwhelming majority of folds.
	sweepInterval = 30 * time.Second
)

// chunkCadenceKinds are the out.* kinds published at token/chunk cadence. The
// aggregator folds nothing from them, so processing one is pure cost: a copy of
// the payload into the per-session ring the console's Recent Activity tab reads
// (~100 deltas per streaming turn evict everything else), plus a marshal of the
// whole SessionState and a fan-out to every SSE subscriber — per token. The
// operator's subscription is `ap.session.*.*.out.>`, which matches them, and
// the runner's stream hook is gated only on channel-attachment, never on
// whether any channel renders streaming. Declared as a set rather than a
// kind-equality branch so a new streaming kind is one row here.
var chunkCadenceKinds = map[channelevents.Kind]struct{}{
	channelevents.KindAssistantStreamDelta: {},
	channelevents.KindToolSessionDelta:     {},
}

// Aggregator maintains live session state. All methods are safe for
// concurrent use; subscriber channels are buffered and lossy-on-overflow
// (a slow SSE client drops frames rather than blocking the watch/NATS
// callbacks — the next upsert resyncs it).
type Aggregator struct {
	mu         sync.RWMutex
	sessions   map[string]*sessionEntry // key "ns/name"
	tombstones map[string]*tombstone    // key "ns/name", pruned after tombstoneTTL
	lastSweep  time.Time
	toolCalls  []ToolCall // global ring, newest last, capped at ToolCallRingSize
	subs       map[chan SSEFrame]struct{}
	logger     logr.Logger
	// now is the clock, injectable so the TTL sweep is testable without
	// sleeping. Always UTC.
	now func() time.Time
}

func NewAggregator(logger logr.Logger) *Aggregator {
	return newAggregatorWithClock(logger, func() time.Time { return time.Now().UTC() })
}

// newAggregatorWithClock is the constructor NewAggregator delegates to; tests
// inject a fake clock through it to exercise the TTL sweep.
func newAggregatorWithClock(logger logr.Logger, now func() time.Time) *Aggregator {
	return &Aggregator{
		sessions:   map[string]*sessionEntry{},
		tombstones: map[string]*tombstone{},
		subs:       map[chan SSEFrame]struct{}{},
		logger:     logger,
		now:        now,
	}
}

// sweepLocked ages out provisional entries and expired tombstones. Caller holds
// the write lock. Rate-limited to one pass per sweepInterval: without it the
// two maps are append-only, and with --watch-namespaces set every out-of-scope
// session (the NATS subscription is cluster-wide, the informer is not) would
// leak a permanent blank stub into the sessions list and the KPIs.
func (a *Aggregator) sweepLocked(now time.Time) {
	if now.Sub(a.lastSweep) < sweepInterval {
		return
	}
	a.lastSweep = now
	for k, e := range a.sessions {
		if !e.confirmed && now.Sub(e.createdAt) > stubTTL {
			delete(a.sessions, k)
			a.logger.Info("admind: pruned a session the CRD watch never confirmed",
				"session", k, "age", now.Sub(e.createdAt).String())
		}
	}
	for k, ts := range a.tombstones {
		if now.Sub(ts.at) > tombstoneTTL {
			delete(a.tombstones, k)
		}
	}
}

// phaseActivity derives, from the AgentSession CRD alone, whether the session
// is doing work — the authority for "definitely not working", against which the
// NATS turn_activity overlay is reconciled. Mirrors webd's derivePause: every
// approval family parks on the generic PendingInteractions list, info_leakage
// keeps its own cause, and each parked/terminal phase maps to its PauseCause.
// Only Pending, Running, and the not-yet-stamped empty phase read as working —
// a raw `Phase == Running` test would report every parked session active. The
// Awaiting* phases need no arm of their own: an interaction category that parks
// a session is exactly one that leaves an entry on PendingInteractions.
func phaseActivity(s *spiceboxv1alpha1.AgentSession) (active bool, cause string) {
	for _, e := range s.Status.PendingInteractions {
		if e.Category == categories.InfoLeakage {
			return false, channelevents.PauseCauseLeakageApproval
		}
	}
	if len(s.Status.PendingInteractions) > 0 || len(s.Status.PendingRequesters) > 0 {
		return false, channelevents.PauseCauseApproval
	}
	switch s.Status.Phase {
	case spiceboxv1alpha1.AgentSessionPhaseAwaitingRetry:
		return false, channelevents.PauseCauseRetry
	case spiceboxv1alpha1.AgentSessionPhaseFailed:
		return false, channelevents.PauseCauseFailed
	case spiceboxv1alpha1.AgentSessionPhaseIdle:
		return false, channelevents.PauseCauseReply
	case spiceboxv1alpha1.AgentSessionPhaseSucceeded:
		return false, channelevents.PauseCauseComplete
	default: // Pending / Running / not yet stamped
		return true, ""
	}
}

func key(ns, name string) string { return ns + "/" + name }

// modelDisplay composes the uniform "<provider>/<model>" display id from a
// session's resolved provider + model name — the same composition rule as
// the runner's servedModelDisplay (pkg/agent/runner/loop.go): a bare model
// name when provider is empty, "<provider>/<model>" when both are set.
func modelDisplay(provider, name string) string {
	if provider == "" {
		return name
	}
	return provider + "/" + name
}

// bareModel strips the FIRST "/"-segment (the uniform display id's AP
// provider prefix, e.g. "anthropic", "openrouter") from a served-model
// display string, recovering the bare model id the price tables
// (llmpricing/models.go) and the model-catalog overrides
// (effectivePrices' overrides[e.Name]) are keyed by. Only the first segment
// strips: a routed OpenRouter display ("openrouter/anthropic/claude-3.5-
// sonnet") still carries its own provider segment afterward
// ("anthropic/claude-3.5-sonnet") — matching how pkg/agent/postsession/cost prices
// per-bucket usage by the bare served-model id, not a further-stripped leaf.
// No "/" -> unchanged; empty -> empty. Every prices.Estimate(...) call site in
// this package must pass its model through bareModel first — Estimate does an
// exact map-key lookup and the map is keyed bare.
func bareModel(display string) string {
	if _, rest, ok := strings.Cut(display, "/"); ok {
		return rest
	}
	return display
}

// UpsertSession folds a CRD add/update into the state. CRD fields win for
// everything the CRD owns; NATS-overlay fields (Active/StatusText/Plan/
// progress counters) are preserved unless the CRD is fresher.
func (a *Aggregator) UpsertSession(s *spiceboxv1alpha1.AgentSession) {
	a.mu.Lock()
	now := a.now()
	a.sweepLocked(now)
	k := key(s.Namespace, s.Name)
	// A live AgentSession always beats a tombstone — including a session whose
	// name is reused inside the tombstone's TTL.
	delete(a.tombstones, k)
	e, ok := a.sessions[k]
	if !ok {
		e = &sessionEntry{state: SessionState{Namespace: s.Namespace, Name: s.Name}, createdAt: now}
		a.sessions[k] = e
	}
	e.confirmed = true
	st := &e.state
	st.Class = s.Spec.Class
	// Resolved model is stamped onto status by the AgentSession reconciler
	// (4-tier settings snapshot) and is session-constant; the Overview
	// aggregator groups tokens by it.
	if es := s.Status.EffectiveSettings; es != nil && es.Model.Name != "" {
		st.Model = modelDisplay(es.Model.Provider, es.Model.Name)
	}
	// ByModel is the CRD's authoritative per-served-model breakdown, stamped
	// once at SessionEnd (pkg/agent/postsession/cost); mirror it wholesale each
	// upsert rather than an overlay-no-regress merge (unlike the NATS token
	// overlay below, there is no fresher concurrent writer to protect against).
	if ec := s.Status.EstimatedCost; ec != nil {
		// Defensive copy: ec.ByModel backs onto the informer cache's object; a
		// later Snapshot()/sortedModels-style sort-in-place elsewhere on the
		// SAME backing array would race the informer's own reads of it.
		st.ByModel = append([]spiceboxv1alpha1.ModelCostBucket(nil), ec.ByModel...)
		st.ByTool = append([]spiceboxv1alpha1.ToolCostBucket(nil), ec.ByTool...)
	} else {
		st.ByModel = nil
		st.ByTool = nil
	}
	// Defensive copy for the same reason as ByModel above: s.Status.BundleSessions
	// backs onto the informer cache's object.
	st.BundleSessions = append([]spiceboxv1alpha1.ResolvedBundle(nil), s.Status.BundleSessions...)
	st.Phase = s.Status.Phase
	if s.Spec.InputChannel != nil {
		st.ChannelKind = s.Spec.InputChannel.Kind
		st.ChannelName = s.Spec.InputChannel.Name
	}
	if s.Status.StartedAt != nil {
		t := s.Status.StartedAt.Time
		st.StartedAt = &t
	} else if !s.CreationTimestamp.IsZero() {
		// No controller stamps Status.StartedAt today (the field exists but is
		// never populated), so fall back to the CR's creation time — the
		// sessions table then always shows a start time rather than a blank.
		t := s.CreationTimestamp.Time
		st.StartedAt = &t
	}
	// CRD annotation is authoritative; kubectl-driven sessions leave it blank.
	st.StartedBy = spiceboxv1alpha1.StartedBySubject(s).String()
	if p := s.Status.Progress; p != nil {
		// CRD progress is a turn-end snapshot; only adopt it when it is
		// ahead of the NATS overlay (runner restarts reset the overlay).
		if p.InputTokens > st.InputTokens {
			st.InputTokens = p.InputTokens
		}
		if p.OutputTokens > st.OutputTokens {
			st.OutputTokens = p.OutputTokens
		}
		st.TurnCount = p.TurnCount
		st.ToolCallCount = p.ToolCallCount
	}
	// Fold the session's pending approvals off the single generic
	// PendingInteractions list — since Slice C2 every approval family
	// (tool_approval, info_leakage, content_inspection) parks there; the typed
	// pendingToolGrants/pendingLeakageApprovals lists were deleted. Map each
	// entry's Category onto its admind-facing Kind label and its aggregate
	// counter. There is no discrete tool column post-migration (Approval.Tool
	// shows the publisher's one-line interaction Summary) and the generic list
	// carries no requester (Approval.Requester stays empty). CRD status is
	// authoritative; the entry's approvals are replaced wholesale each upsert and
	// the counters are zeroed first so a re-upsert with an empty list clears
	// them. Stored on the entry so DeleteSession drops them with the session.
	st.PendingToolGrants = 0
	st.PendingLeakageApprovals = 0
	st.PendingContentInspectionApprovals = 0
	apps := make([]Approval, 0, len(s.Status.PendingInteractions))
	for _, e := range s.Status.PendingInteractions {
		var kind string
		switch e.Category {
		case string(categories.ToolApproval):
			kind = "tool_call"
			st.PendingToolGrants++
		case string(categories.InfoLeakage):
			kind = "leakage"
			st.PendingLeakageApprovals++
		case string(categories.ContentInspection):
			kind = "content_inspection"
			st.PendingContentInspectionApprovals++
		default:
			// Non-approval categories (identity_choice, credential_link, …) are
			// not approvals the admin console surfaces here.
			continue
		}
		apps = append(apps, Approval{
			Kind: kind, Tool: e.Summary,
			Namespace: s.Namespace, Name: s.Name, Class: st.Class,
			RequestedAt: e.RequestedAt.Time,
		})
	}
	e.approvals = apps

	// Reconcile the NATS activity overlay against the CRD. Active has exactly
	// one other writer — the turn_activity arm of HandleEnvelopeBytes — and the
	// runner is that signal's only publisher, so a runner that is OOM-killed,
	// evicted, or crashes mid-turn never emits active:false: without this the
	// overlay latches true for as long as the CR exists, permanently inflating
	// the ActiveSessions KPI and painting a dead session with a live pulsing
	// dot. Same backstop the web chat applies by synthesising a
	// turn_activity(active:false) off the phase when the runner's real-time one
	// was missed (pkg/web/webui/chat/session.go).
	//
	// The correction runs in ONE direction, the rule webd's shouldSelfHeal
	// states: the phase clears a stuck-working state, and never flips a
	// genuinely-working session off — a seed is written only while no real
	// signal has ever arrived for this entry, which is the fresh-process case
	// (the informer replays every CR; core NATS replays nothing).
	active, cause := phaseActivity(s)
	e.phaseInactive, e.phaseCause = !active, cause
	switch {
	case !active:
		st.Active, st.ActivityCause = false, cause
	case !e.sawTurnActivity:
		st.Active, st.ActivityCause = true, ""
	}

	st.LastEventAt = now
	out := *st
	a.mu.Unlock()
	a.broadcastSession(out)
}

func (a *Aggregator) DeleteSession(ns, name string) {
	a.mu.Lock()
	k := key(ns, name)
	delete(a.sessions, k)
	a.tombstones[k] = &tombstone{at: a.now()}
	a.mu.Unlock()
	data, err := json.Marshal(map[string]string{"namespace": ns, "name": name})
	if err != nil {
		a.logger.Info("admind: marshal remove frame", "err", err.Error())
		return
	}
	a.broadcast(SSEFrame{Event: "remove", Data: data})
}

// HandleEnvelopeBytes folds one NATS out.* envelope into the state, keyed off
// the SUBJECT it arrived on. Malformed envelopes are logged and dropped — a bad
// publisher must not kill the subscription.
//
// subject is REQUIRED, and it is the routing authority. The operator subscribes
// this to the cluster-wide "ap.session.*.*.out.>" while a publisher's
// per-session NATS JWT permits exactly one "ap.session.<ns>.<own-name>.>"
// subtree, so the subject is the only session identity NATS actually
// authorized; Envelope.Session is publisher-controlled JSON. Keying off the
// envelope would let a principal permitted on ONE session's out subject drive
// any other session's row in the admin console's live view — inject activity,
// flip the working dot, rewrite the status caption, or conjure a session that
// does not exist — and an operator makes decisions from that view. Same rule,
// and the same ParseOutSubject, as the outbound relay's handle
// (pkg/channels/channelsd/outbound/relay.go).
func (a *Aggregator) HandleEnvelopeBytes(subject string, b []byte) {
	var env channelevents.Envelope
	if err := json.Unmarshal(b, &env); err != nil {
		a.logger.Info("admind: malformed envelope dropped", "subject", subject, "err", err.Error())
		return
	}
	if _, chunk := chunkCadenceKinds[env.Kind]; chunk {
		// Nothing here folds them, and at token cadence they would evict the
		// recent-event ring and re-marshal + fan out the whole SessionState per
		// token. Dropped before the lock is taken — and before the subject
		// cross-check below, deliberately: a kind that reaches no state needs no
		// authority to discard, whereas checking first would let a hostile
		// publisher emit a log line per token.
		return
	}
	ns, name, subjectOK := channelevents.ParseOutSubject(subject)
	if !subjectOK {
		a.logger.Info("admind: envelope on an unparseable out subject dropped",
			"subject", subject,
			"claimedSession", env.Session.Namespace+"/"+env.Session.Name,
			"kind", string(env.Kind))
		return
	}
	if ns != env.Session.Namespace || name != env.Session.Name {
		// Logged with the publisher-locating trio (subject, claimed session,
		// kind) per AGENTS.md's no-silent-errors rule. A silent drop here would
		// be worse than the defect it closes: an operator would watch a session
		// stop updating in the live view with nothing to grep for.
		a.logger.Info("admind: envelope session does not match the authorized subject; dropped",
			"subject", subject,
			"subjectSession", key(ns, name),
			"claimedSession", env.Session.Namespace+"/"+env.Session.Name,
			"kind", string(env.Kind))
		return
	}

	a.mu.Lock()
	now := a.now()
	a.sweepLocked(now)
	k := key(ns, name)
	e, ok := a.sessions[k]
	if !ok {
		if ts := a.tombstones[k]; ts != nil {
			// The AgentSession is gone but its runner is still publishing
			// through its termination grace period (the finalizer stops the pod
			// asynchronously and drops immediately). Re-creating the entry here
			// leaves a blank stub no deleter ever visits again.
			if !ts.loggedDrop {
				ts.loggedDrop = true
				a.logger.Info("admind: dropping envelopes for a deleted session", "session", k, "kind", string(env.Kind))
			}
			a.mu.Unlock()
			return
		}
		// NATS can race ahead of the CRD watch; create a provisional stub the
		// next UpsertSession confirms (and the sweep prunes if it never does).
		e = &sessionEntry{state: SessionState{Namespace: ns, Name: name}, createdAt: now}
		a.sessions[k] = e
	}
	st := &e.state
	st.LastEventAt = now

	switch env.Kind {
	case channelevents.KindTurnProgress:
		var p channelevents.TurnProgressPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			a.logger.Info("admind: malformed KindTurnProgress payload dropped",
				"session", key(ns, name), "err", err.Error())
		} else {
			st.InputTokens, st.OutputTokens, st.ElapsedSeconds = p.InputTokens, p.OutputTokens, p.ElapsedSeconds
		}
	case channelevents.KindTurnActivity:
		var p channelevents.TurnActivityPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			a.logger.Info("admind: malformed KindTurnActivity payload dropped",
				"session", key(ns, name), "err", err.Error())
		} else {
			e.sawTurnActivity = true
			st.Active, st.ActivityCause = p.Active, p.Cause
			if p.Active && e.phaseInactive {
				// The CRD already parked or finished this session; a late
				// signal from a runner in its termination grace period must not
				// re-arm the working dot.
				st.Active, st.ActivityCause = false, e.phaseCause
			}
		}
	case channelevents.KindNotification:
		var p channelevents.NotificationPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			a.logger.Info("admind: malformed KindNotification payload dropped",
				"session", key(ns, name), "err", err.Error())
		} else {
			st.StatusText = p.Text
		}
	case channelevents.KindPlanUpdate:
		st.Plan = append(json.RawMessage(nil), env.Payload...)
	case channelevents.KindToolActivity:
		var p channelevents.ToolActivityPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			a.logger.Info("admind: malformed KindToolActivity payload dropped",
				"session", key(ns, name), "err", err.Error())
		} else {
			a.toolCalls = append(a.toolCalls, ToolCall{
				At: now, Namespace: ns, Name: name, Class: st.Class, Tool: p.Tool,
			})
			if len(a.toolCalls) > ToolCallRingSize {
				a.toolCalls = a.toolCalls[len(a.toolCalls)-ToolCallRingSize:]
			}
		}
	}

	e.recent = append(e.recent, RecentEvent{At: now, Kind: string(env.Kind), Payload: append(json.RawMessage(nil), env.Payload...)})
	if len(e.recent) > RingSize {
		e.recent = e.recent[len(e.recent)-RingSize:]
	}
	out := *st
	a.mu.Unlock()
	a.broadcastSession(out)
}

// Snapshot returns every session's state, newest StartedAt first.
func (a *Aggregator) Snapshot() []SessionState {
	a.mu.RLock()
	out := make([]SessionState, 0, len(a.sessions))
	for _, e := range a.sessions {
		out = append(out, e.state)
	}
	a.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		ti, tj := time.Time{}, time.Time{}
		if out[i].StartedAt != nil {
			ti = *out[i].StartedAt
		}
		if out[j].StartedAt != nil {
			tj = *out[j].StartedAt
		}
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return key(out[i].Namespace, out[i].Name) < key(out[j].Namespace, out[j].Name)
	})
	return out
}

// Get returns one session's state + a copy of its recent-event ring.
func (a *Aggregator) Get(ns, name string) (SessionState, []RecentEvent, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	e, ok := a.sessions[key(ns, name)]
	if !ok {
		return SessionState{}, nil, false
	}
	recent := make([]RecentEvent, len(e.recent))
	copy(recent, e.recent)
	return e.state, recent, true
}

// Class returns the session's AgentClass if it is (still) tracked — the
// audit engine's ResolveClass fallback for live sessions.
func (a *Aggregator) Class(ns, name string) string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if e, ok := a.sessions[key(ns, name)]; ok {
		return e.state.Class
	}
	return ""
}

// ToolCalls returns a newest-first copy of the global tool-call ring (the
// internal ring is newest-last, so this reverses it).
func (a *Aggregator) ToolCalls() []ToolCall {
	a.mu.RLock()
	defer a.mu.RUnlock()
	n := len(a.toolCalls)
	out := make([]ToolCall, n)
	for i, tc := range a.toolCalls {
		out[n-1-i] = tc
	}
	return out
}

// Approvals flattens every tracked session's pending approvals into one
// cluster-wide queue, oldest-requested first (session key then kind break
// ties), under the read lock. Returns a non-nil (possibly empty) slice so
// the JSON encodes as [] rather than null.
func (a *Aggregator) Approvals() []Approval {
	a.mu.RLock()
	out := []Approval{}
	for _, e := range a.sessions {
		out = append(out, e.approvals...)
	}
	a.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if !out[i].RequestedAt.Equal(out[j].RequestedAt) {
			return out[i].RequestedAt.Before(out[j].RequestedAt)
		}
		ki, kj := key(out[i].Namespace, out[i].Name), key(out[j].Namespace, out[j].Name)
		if ki != kj {
			return ki < kj
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

// Subscribe registers an SSE subscriber. The returned cancel MUST be
// called when the client disconnects.
func (a *Aggregator) Subscribe() (<-chan SSEFrame, func()) {
	ch := make(chan SSEFrame, 64)
	a.mu.Lock()
	a.subs[ch] = struct{}{}
	a.mu.Unlock()
	return ch, func() {
		a.mu.Lock()
		delete(a.subs, ch)
		a.mu.Unlock()
	}
}

func (a *Aggregator) broadcastSession(st SessionState) {
	data, err := json.Marshal(st)
	if err != nil {
		a.logger.Info("admind: marshal session frame", "session", key(st.Namespace, st.Name), "err", err.Error())
		return
	}
	a.broadcast(SSEFrame{Event: "session", Data: data})
}

func (a *Aggregator) broadcast(fr SSEFrame) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for ch := range a.subs {
		select {
		case ch <- fr:
		default: // lossy: slow client drops frames; next upsert resyncs
		}
	}
}

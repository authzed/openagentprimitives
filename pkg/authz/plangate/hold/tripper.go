// Package hold holds the forensic-hold trippers: the things that decide, from
// observed evidence, that a session should be frozen for review.
//
// A tripper runs in the OPERATOR, never in the runner. The evidence it reads is
// produced by the runner, and the runner is the party under suspicion — a
// compromised one would simply decline to trip itself. Running the check in a
// different process from the one being checked is the point.
//
// A tripper's OnSignal must never write a memory entry. It receives
// memory.SignalEntryAppended, which fires after every successful append-only
// Put; a hook that reacted by Putting would fire the signal again, recursing
// without bound. The correct shape (this package's) is read-only: read the
// durable plangate_audit log and, on trip, create a SessionHold CR through the
// Kubernetes client instead.
package hold

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession/cosidecar"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
)

// Tripper decides whether observed evidence warrants freezing a session. New
// trippers register via Setup; consumers (SendSignal's dispatch loop) are
// untouched.
type Tripper interface {
	Name() string
	OnSignal(ctx context.Context, sig memory.Signal) error
}

type DenialStreakDeps struct {
	// Threshold is the number of CONSECUTIVE denied/would-deny governed calls
	// that trips a hold. ZERO MEANS DISABLED — an unset threshold must never
	// read as "freeze everything".
	Threshold int
	Mem       memory.Memory
	Client    client.Client
	Logger    *slog.Logger
}

// DenialStreak trips when an agent keeps attempting calls outside its approved
// phase ceiling after being refused.
type DenialStreak struct{ deps DenialStreakDeps }

func NewDenialStreak(deps DenialStreakDeps) *DenialStreak { return &DenialStreak{deps: deps} }

func (d *DenialStreak) Name() string { return "plangate-denial-streak" }

// logger returns deps.Logger, falling back to slog.Default() so a DenialStreak
// built without one (a test constructing it by hand to reach shouldTrip) never
// panics on a nil receiver if OnSignal/trip is later exercised too.
func (d *DenialStreak) logger() *slog.Logger {
	if d.deps.Logger != nil {
		return d.deps.Logger
	}
	return slog.Default()
}

func (d *DenialStreak) shouldTrip(streak int) bool {
	if d.deps.Threshold <= 0 {
		return false
	}
	return streak >= d.deps.Threshold
}

// consecutiveDenials counts the denial run ending at the most recent record.
// An allowed governed call resets it: the agent found its way back inside the
// ceiling, which is the behaviour the gate is meant to produce.
func consecutiveDenials(records []plangateaudit.Content) int {
	n := 0
	for i := len(records) - 1; i >= 0; i-- {
		switch records[i].Outcome {
		case plangateaudit.OutcomeDenied, plangateaudit.OutcomeWouldDeny:
			n++
		case plangateaudit.OutcomeAllow:
			return n
		default:
			// Records that are not governed-call outcomes (card_built,
			// phase_selected, …) are not evidence either way; skip without
			// breaking the run.
			continue
		}
	}
	return n
}

// consecutiveDenialsAcrossClosure is consecutiveDenials' counterpart for a
// delegation closure: the denial run ending at the most recent record,
// merged across every session in the tree rather than read from one
// session's own log. records must already be sorted oldest-first by At — the
// same order loadPlanGateRecords returns for a single scope; the caller
// merges several such slices and sorts the concatenation before calling this.
//
// Records from different sessions have no reliable mutual order: each
// session's plan_gate_audit log is an independent append-only chain written
// by an independent runner process, and nothing synchronizes their clocks —
// the same reason the bronzethread golden is compared sorted rather than
// replayed chronologically. Merging by each record's own At is therefore a
// best-effort total order, and a TIE — two or more records sharing the exact
// same At, which a coarse clock or concurrent writers make more than
// incidental — has no resolvable order at all.
//
// This function treats every group of records sharing an identical At as one
// atomic step rather than picking an arbitrary order within it. If such a
// group contains an allow, the WHOLE group is read as the point the run
// broke: none of the group's denials are credited, exactly as if the allow
// were known to be the newest member of the group. The alternative —
// crediting the tied denials and hoping the allow actually preceded them —
// would trip a containment control on an ambiguity it cannot actually
// resolve. The fail-safe direction for a containment control is to trip
// LATER, never to trip ON a tie, so ties resolve toward NOT tripping.
func consecutiveDenialsAcrossClosure(records []plangateaudit.Content) int {
	n := 0
	i := len(records) - 1
	for i >= 0 {
		// Walk left across every record sharing records[i]'s exact At, forming
		// the tied group [j+1, i].
		j := i
		for j >= 0 && records[j].At.Equal(records[i].At) {
			j--
		}
		group := records[j+1 : i+1]

		hasAllow := false
		denials := 0
		for _, r := range group {
			switch r.Outcome {
			case plangateaudit.OutcomeAllow:
				hasAllow = true
			case plangateaudit.OutcomeDenied, plangateaudit.OutcomeWouldDeny:
				denials++
			}
		}
		if hasAllow {
			return n
		}
		n += denials
		i = j
	}
	return n
}

// activePhaseIndex returns the PhaseIndex carried by the most recent record
// that names one, or -1 when no record in the log does. The per-call gate
// events (allow/would_deny/denied) all stamp PhaseIndex, so the most recent
// one in a denial streak names the phase the agent kept getting refused in.
func activePhaseIndex(records []plangateaudit.Content) int {
	for i := len(records) - 1; i >= 0; i-- {
		if records[i].PhaseIndex != nil {
			return int(*records[i].PhaseIndex)
		}
	}
	return -1
}

// loadPlanGateRecords reads the plan_gate_audit log for scope, oldest-first —
// the same order the plan gate's own fold replays it in.
//
// The ctx a signal arrives on carries no approval of this hook's own (see
// memory.Local.SendSignal), so it is wrapped in a system approval here, the
// same way kg_ingestion's and lifecycle's OnSignal do, before reaching
// plangateaudit.List's Query.
func loadPlanGateRecords(ctx context.Context, mem memory.Memory, scope memory.Scope) ([]plangateaudit.Content, error) {
	ctx = memory.WithSystemApproval(ctx, "operator:plangate-denial-streak")
	return plangateaudit.List(ctx, mem, scope)
}

// OnSignal reacts to a plan_gate_audit append by recomputing the denial
// streak and, on threshold, freezing the session that owns it.
//
// A parent that spreads N-1 denials across each of many children never
// crosses ANY single session's own threshold, so the streak has to be a
// property of the delegation tree, not of whichever session's Put happened
// to fire this signal. Every signal therefore resolves the signalling
// session's lineage first: its topmost ancestor (the tree's root, via
// ResolveRoot) and every other member of that tree (via ListClosure). A
// session with no lineage — its own root, no labelled descendants — takes
// the pre-existing single-scope path unchanged, so the 99% of sessions that
// never delegate pay for exactly one plan_gate_audit read per signal; the
// closure walk only turns into extra memory reads once delegation is
// actually in play.
//
// Reading AgentSession here needs get/list on agentsessions — granted
// already, not by a marker in this package: controller-gen only scans
// pkg/apis/... and pkg/controllers/... (magefiles/magefile.go), so a
// kubebuilder:rbac marker placed in this package would compile and grant
// nothing. The permission is inherited from the operator's aggregate
// ClusterRole, assembled from other controllers' markers (see
// config/manager/role.yaml).
func (d *DenialStreak) OnSignal(ctx context.Context, sig memory.Signal) error {
	if sig.Kind != memory.SignalEntryAppended || d.deps.Threshold <= 0 {
		return nil
	}

	ns, sessionName, ok := strings.Cut(sig.Scope.ID, "/")
	if !ok || ns == "" || sessionName == "" {
		return fmt.Errorf("denial-streak: session scope id %q is not <namespace>/<name>", sig.Scope.ID)
	}

	var sess spiceboxv1alpha1.AgentSession
	if err := d.deps.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: sessionName}, &sess); err != nil {
		if apierrors.IsNotFound(err) {
			d.logger().Info("denial-streak: session gone before lineage resolution; skipping",
				"session", sig.Scope.ID)
			return nil
		}
		return fmt.Errorf("denial-streak: get AgentSession %s/%s: %w", ns, sessionName, err)
	}

	root, err := spiceboxv1alpha1.ResolveRoot(ctx, d.deps.Client, &sess)
	if err != nil {
		return fmt.Errorf("denial-streak: resolve root for %s: %w", sig.Scope.ID, err)
	}

	closure, err := spiceboxv1alpha1.ListClosure(ctx, d.deps.Client, root.Namespace, root.Name)
	if err != nil {
		return fmt.Errorf("denial-streak: list delegation closure for %s/%s: %w", root.Namespace, root.Name, err)
	}

	rootScope := memory.Scope{Kind: sig.Scope.Kind, ID: root.Namespace + "/" + root.Name}

	if len(closure) == 0 && root.Namespace == ns && root.Name == sessionName {
		// The common case: sess is its own root and has no labelled
		// descendants. Take the pre-existing single-scope path unchanged.
		records, err := loadPlanGateRecords(ctx, d.deps.Mem, sig.Scope)
		if err != nil {
			return fmt.Errorf("denial-streak: load plangate_audit for %s: %w", sig.Scope.ID, err)
		}
		streak := consecutiveDenials(records)
		if !d.shouldTrip(streak) {
			return nil
		}
		return d.trip(ctx, sig.Scope, streak, records, 1)
	}

	// Delegation is in play: read every member's own plan_gate_audit log — the
	// root's plus every descendant ListClosure returned (descendants only; the
	// root itself is never labelled with its own name) — and merge them by
	// each record's own At into one oldest-first timeline before folding.
	scopes := make([]memory.Scope, 0, len(closure)+1)
	scopes = append(scopes, rootScope)
	for i := range closure {
		m := &closure[i]
		scopes = append(scopes, memory.Scope{Kind: sig.Scope.Kind, ID: m.Namespace + "/" + m.Name})
	}

	var all []plangateaudit.Content
	for _, s := range scopes {
		records, err := loadPlanGateRecords(ctx, d.deps.Mem, s)
		if err != nil {
			return fmt.Errorf("denial-streak: load plangate_audit for %s: %w", s.ID, err)
		}
		all = append(all, records...)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].At.Before(all[j].At) })

	streak := consecutiveDenialsAcrossClosure(all)
	if !d.shouldTrip(streak) {
		return nil
	}
	return d.trip(ctx, rootScope, streak, all, len(scopes))
}

// trip creates the SessionHold that freezes scope's session.
//
// The name is deterministic from the session and the streak, not
// GenerateName: a retry of this exact call (the streak unchanged) must land
// on the SAME object, so a transient failure followed by a retry sees
// AlreadyExists rather than minting a second hold. There is no cached streak
// state to clear on failure — the streak is recomputed fresh from the
// durable log(s) on every signal — so returning the error here is enough for
// the next denial's signal to retry the trip; nothing here can silently give
// up.
//
// scope is always the tree's ROOT scope when closureSize > 1 — the streak
// decides, this creates the hold on the root, and the SessionHold
// reconciler's cascade (pkg/controllers/sessionhold's cascadeHold) is what
// carries it back down to the children. closureSize is 1 for a single,
// non-delegated session and, when the streak was computed across a
// delegation closure, the size of the WHOLE tree the streak was folded over
// — root plus every ListClosure descendant, computed before any record is
// read, NOT filtered down to which members' logs actually contained a
// denial. It exists only to shape the reason string: naming the tree's full
// size rather than a filtered "how many actually denied" count is
// deliberate — it tells whoever is triaging the freeze how many sessions
// the cascade is about to freeze, which is the number they need.
func (d *DenialStreak) trip(ctx context.Context, scope memory.Scope, streak int, records []plangateaudit.Content, closureSize int) error {
	ns, sessionName, ok := strings.Cut(scope.ID, "/")
	if !ok || ns == "" || sessionName == "" {
		return fmt.Errorf("denial-streak: session scope id %q is not <namespace>/<name>", scope.ID)
	}

	// A genuine OwnerReference is required, not optional: activeHoldFor
	// (pkg/controllers/agentsession/hold.go) discovers holds by List +
	// spec.sessionRef.Name, not by owner-ref, so an ownerless hold left behind
	// by a deleted session would keep matching a differently-provisioned
	// session later recreated under the same name — channel-attached sessions
	// are named deterministically, so that recreation is ordinary, not
	// exotic — and freeze it on sight with a stale reason. Fetching fresh here
	// also means a session already gone by the time the trip fires is
	// correctly a no-op rather than a dangling, ownerless hold nothing will
	// ever garbage-collect.
	var sess spiceboxv1alpha1.AgentSession
	if err := d.deps.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: sessionName}, &sess); err != nil {
		if apierrors.IsNotFound(err) {
			d.logger().Info("denial-streak: session gone before trip; not creating a hold",
				"session", scope.ID, "streak", streak)
			return nil
		}
		return fmt.Errorf("denial-streak: get AgentSession %s/%s: %w", ns, sessionName, err)
	}

	// An unreleased hold already covers this session: existence, not name
	// equality, is the dedup key. The hold's name embeds the streak value
	// (below), so Create's AlreadyExists only dedups a RETRY AT THE SAME
	// streak -- a later signal that recomputes a HIGHER streak names a
	// DIFFERENT object and would sail straight past it. Within one session
	// that was near-harmless: the first Active stamp cancels the runner's
	// context, so no further denials ever arrive to retrigger this. Across a
	// delegation closure it is not -- the hold lands on the root, and the
	// root's own cancellation does nothing to the children, which keep
	// running (and denying) until their own cascaded holds go Active, at
	// least one reconcile hop away and behind a snapshot Job each. Every
	// child denial in that window would otherwise mint another root hold
	// under a new name, each publishing its own release card and cascading
	// its own full set. Mirrors holdsFor/activeHoldFor
	// (pkg/controllers/agentsession/hold.go): List + filter, no index needed
	// at this namespace's cardinality.
	var existing spiceboxv1alpha1.SessionHoldList
	if err := d.deps.Client.List(ctx, &existing, client.InNamespace(ns)); err != nil {
		return fmt.Errorf("denial-streak: list SessionHolds in %s: %w", ns, err)
	}
	for i := range existing.Items {
		h := &existing.Items[i]
		if h.Spec.SessionRef.Name == sessionName && !h.IsReleased() {
			d.logger().Info("denial-streak: an unreleased hold already covers this session; not minting another",
				"session", scope.ID, "streak", streak, "existing", h.Name)
			return nil
		}
	}

	reason := fmt.Sprintf("%d consecutive plan-gate denials", streak)
	if closureSize > 1 {
		// closureSize is the delegation TREE's size, not a count of sessions
		// that actually denied — see trip's doc comment. Naming it that way is
		// deliberate: it is what a human triaging the freeze needs (how many
		// sessions the cascade is about to freeze), not implied evidence.
		reason = fmt.Sprintf("%d consecutive plan-gate denials across a delegation tree of %d sessions",
			streak, closureSize)
	}
	// Suppressed entirely (not just "which session's") when closureSize > 1:
	// records there is a MERGED multi-session timeline, so activePhaseIndex
	// can return a CHILD's PhaseIndex while the hold this builds is created
	// on the ROOT -- printing "... in phase P" as though P described the
	// root's own plan when it may name a different session's entirely.
	if phase := activePhaseIndex(records); phase >= 0 && closureSize == 1 {
		reason += fmt.Sprintf(" in phase %d", phase)
	}

	h := &spiceboxv1alpha1.SessionHold{
		ObjectMeta: metav1.ObjectMeta{
			Name:            fmt.Sprintf("plangate-denial-streak-%s-%d", sessionName, streak),
			Namespace:       ns,
			OwnerReferences: cosidecar.OwnerRef(&sess),
		},
		Spec: spiceboxv1alpha1.SessionHoldSpec{
			SessionRef: spiceboxv1alpha1.NamespacedRef{Namespace: ns, Name: sessionName},
			Reason:     reason,
			Source:     "tripper/" + d.Name(),
		},
	}

	if err := d.deps.Client.Create(ctx, h); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("denial-streak: create SessionHold %s/%s: %w", ns, h.Name, err)
	}
	d.logger().Info("plan-gate denial streak tripped a forensic hold",
		"session", scope.ID, "streak", streak, "hold", h.Name)
	return nil
}

// KindName is the registered memory Kind whose ScopeHooks deliver
// SignalEntryAppended to the configured Tripper. Nothing ever Puts an entry
// of this Kind — like kg_ingestion (pkg/memory/kinds/kgingestion), it exists
// solely to carry ScopeHooks.
const KindName = "plangate_hold"

var (
	tripperMu  sync.RWMutex
	tripperRef Tripper
)

// Setup wires t as the plangate_hold Kind's ScopeHooks. Call once from the
// operator after constructing the tripper. A nil t (the threshold-zero case)
// is valid and leaves NewScopeHooks returning a no-op.
func Setup(t Tripper) {
	tripperMu.Lock()
	tripperRef = t
	tripperMu.Unlock()
}

// Teardown clears the wiring. Tests use this; production never does.
func Teardown() {
	tripperMu.Lock()
	tripperRef = nil
	tripperMu.Unlock()
}

// Kind implements memory.Kind for plangate_hold.
type Kind struct{}

func (Kind) Name() string     { return KindName }
func (Kind) IDPrefix() string { return "pgh-" }

// WriteAuthority: hooks-only, like kg_ingestion — nothing Puts this kind, and
// the fail-closed zero value (ComponentWritten) is the correct answer for a
// Kind with no writer.
func (Kind) WriteAuthority() memory.WriteAuthority { return memory.ComponentWritten }
func (Kind) Retention() memory.Retention           { return memory.Retention{} }
func (Kind) ContentSchema() reflect.Type           { return nil }
func (Kind) IndexedFields() []string               { return nil }

func (Kind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks {
	tripperMu.RLock()
	defer tripperMu.RUnlock()
	if tripperRef == nil {
		return noopHooks{}
	}
	return tripperRef
}

type noopHooks struct{}

func (noopHooks) OnSignal(_ context.Context, _ memory.Signal) error { return nil }

func init() { memory.RegisterKind(Kind{}) }
